package localcontrol

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/managed"
)

var (
	ErrDeviceResultNotFound = errors.New("localcontrol: device command result not found")
	ErrDeviceAgentProtocol  = errors.New("localcontrol: invalid device agent command")
)

const maxDeviceReplyError = 2048

// DeviceCommandHandler is the only Pi-side execution hook. Its implementation
// owns the selected device's provider, repository, verification, and Git
// credentials; the controller supplies only the typed high-level command.
type DeviceCommandHandler func(context.Context, DeviceCommand) (DeviceReply, error)

type DeviceCommandResult struct {
	RequestDigest string      `json:"request_digest"`
	Reply         DeviceReply `json:"reply"`
}

type DeviceResultStore interface {
	Load(context.Context, string) (DeviceCommandResult, error)
	Save(context.Context, string, DeviceCommandResult) error
}

// MemoryDeviceResultStore is useful for process-local tests. Production Pi
// composition should use FileDeviceResultStore so accepted command replies
// survive a service restart.
type MemoryDeviceResultStore struct {
	mu      sync.Mutex
	results map[string]DeviceCommandResult
}

func NewMemoryDeviceResultStore() *MemoryDeviceResultStore {
	return &MemoryDeviceResultStore{results: make(map[string]DeviceCommandResult)}
}

func (s *MemoryDeviceResultStore) Load(ctx context.Context, id string) (DeviceCommandResult, error) {
	if err := ctx.Err(); err != nil {
		return DeviceCommandResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.results[id]
	if !ok {
		return DeviceCommandResult{}, ErrDeviceResultNotFound
	}
	return cloneDeviceCommandResult(value), nil
}

func (s *MemoryDeviceResultStore) Save(ctx context.Context, id string, value DeviceCommandResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(value.RequestDigest) == "" || value.Reply.MessageID == 0 {
		return ErrDeviceAgentProtocol
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.results[id]; ok {
		if existing.RequestDigest != value.RequestDigest {
			return ErrIdempotencyConflict
		}
		return nil
	}
	s.results[id] = cloneDeviceCommandResult(value)
	return nil
}

// FileDeviceResultStore is the Pi-side durable command-result cache. It is
// separate from managed.FileStateStore because the latter owns the inbound
// cursor/inbox protocol and this file owns typed local-control replies.
type FileDeviceResultStore struct {
	path string
	mu   sync.Mutex
}

type fileDeviceResultState struct {
	Version int                            `json:"version"`
	Results map[string]DeviceCommandResult `json:"results"`
}

func NewFileDeviceResultStore(path string) (*FileDeviceResultStore, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, ErrDeviceAgentProtocol
	}
	return &FileDeviceResultStore{path: filepath.Clean(path)}, nil
}

func (s *FileDeviceResultStore) Load(ctx context.Context, id string) (DeviceCommandResult, error) {
	if err := ctx.Err(); err != nil {
		return DeviceCommandResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return DeviceCommandResult{}, err
	}
	value, ok := state.Results[id]
	if !ok {
		return DeviceCommandResult{}, ErrDeviceResultNotFound
	}
	return cloneDeviceCommandResult(value), nil
}

func (s *FileDeviceResultStore) Save(ctx context.Context, id string, value DeviceCommandResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(value.RequestDigest) == "" || value.Reply.MessageID == 0 {
		return ErrDeviceAgentProtocol
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return err
	}
	if existing, ok := state.Results[id]; ok {
		if existing.RequestDigest != value.RequestDigest {
			return ErrIdempotencyConflict
		}
		return nil
	}
	state.Results[id] = cloneDeviceCommandResult(value)
	return s.saveLocked(state)
}

func (s *FileDeviceResultStore) loadLocked() (fileDeviceResultState, error) {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return fileDeviceResultState{Version: 1, Results: make(map[string]DeviceCommandResult)}, nil
	}
	if err != nil {
		return fileDeviceResultState{}, fmt.Errorf("inspect device result store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fileDeviceResultState{}, ErrDeviceAgentProtocol
	}
	file, err := os.Open(s.path)
	if err != nil {
		return fileDeviceResultState{}, fmt.Errorf("open device result store: %w", err)
	}
	defer file.Close()
	var state fileDeviceResultState
	decoder := json.NewDecoder(io.LimitReader(file, 64<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return fileDeviceResultState{}, fmt.Errorf("decode device result store: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fileDeviceResultState{}, ErrDeviceAgentProtocol
	}
	if state.Version != 1 || state.Results == nil {
		return fileDeviceResultState{}, ErrDeviceAgentProtocol
	}
	return state, nil
}

func (s *FileDeviceResultStore) saveLocked(state fileDeviceResultState) error {
	if state.Version == 0 {
		state.Version = 1
	}
	if state.Results == nil {
		state.Results = make(map[string]DeviceCommandResult)
	}
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create device result directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("protect device result directory: %w", err)
	}
	value, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode device result store: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".device-results-*")
	if err != nil {
		return fmt.Errorf("create device result store: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(value, '\n')); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write device result store: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync device result store: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("install device result store: %w", err)
	}
	return nil
}

type DeviceAgentConfig struct {
	Identity            deviceidentity.Key
	ControllerPublicKey []byte
	OrganizationID      string
	DeviceID            string
	ConnectionEpoch     uint64
	ControllerEpoch     uint64
	Replay              *managed.ReplayGuard
	Results             DeviceResultStore
	Handler             DeviceCommandHandler
	Clock               func() time.Time
	ExpiresAfter        time.Duration
}

// DeviceAgent validates and dispatches one signed controller frame. A network
// adapter may feed frames into Handle and send the returned frame; keeping the
// protocol processor independent makes the headless Pi acceptance testable
// without pretending that a Mac has Raspberry Pi hardware or systemd.
type DeviceAgent struct {
	identity        deviceidentity.Key
	controllerKey   deviceidentity.Key
	organizationID  string
	deviceID        string
	connectionEpoch uint64
	controllerEpoch uint64
	replay          *managed.ReplayGuard
	results         DeviceResultStore
	handler         DeviceCommandHandler
	clock           func() time.Time
	expiresAfter    time.Duration

	mu            sync.Mutex
	nextMessageID uint64
	nextSequence  uint64
}

func NewDeviceAgent(config DeviceAgentConfig) (*DeviceAgent, error) {
	if !config.Identity.HasPrivate() || len(config.ControllerPublicKey) != ed25519.PublicKeySize || !validID(config.OrganizationID) || !validID(config.DeviceID) || config.ConnectionEpoch == 0 || config.ControllerEpoch == 0 || config.Handler == nil {
		return nil, ErrDeviceAgentProtocol
	}
	controllerKey, err := deviceidentity.FromPublic(config.ControllerPublicKey)
	if err != nil {
		return nil, ErrDeviceLinkUnauthenticated
	}
	if config.Replay == nil {
		config.Replay, err = managed.NewReplayGuard(&managed.MemoryCursorStore{}, config.OrganizationID, config.DeviceID)
		if err != nil {
			return nil, err
		}
	}
	if config.Results == nil {
		config.Results = NewMemoryDeviceResultStore()
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.ExpiresAfter <= 0 {
		config.ExpiresAfter = defaultDeviceCommandExpiry
	}
	if config.ExpiresAfter >= 5*time.Minute {
		return nil, ErrDeviceAgentProtocol
	}
	return &DeviceAgent{
		identity: config.Identity, controllerKey: controllerKey,
		organizationID: config.OrganizationID, deviceID: config.DeviceID,
		connectionEpoch: config.ConnectionEpoch, controllerEpoch: config.ControllerEpoch,
		replay: config.Replay, results: config.Results, handler: config.Handler,
		clock: config.Clock, expiresAfter: config.ExpiresAfter,
	}, nil
}

func (a *DeviceAgent) Handle(ctx context.Context, frame managed.Frame) (managed.Frame, error) {
	if a == nil || a.replay == nil || a.results == nil || a.handler == nil {
		return managed.Frame{}, ErrDeviceAgentProtocol
	}
	if err := ctx.Err(); err != nil {
		return managed.Frame{}, err
	}
	now := a.clock().UTC()
	if err := frame.Validate(now); err != nil {
		return managed.Frame{}, fmt.Errorf("validate device agent frame: %w", ErrDeviceAgentProtocol)
	}
	if frame.PayloadType != "command" || frame.OrganizationID != a.organizationID || frame.DeviceID != a.deviceID || frame.ConnectionEpoch != a.connectionEpoch || frame.ControllerEpoch != a.controllerEpoch || frame.CommandID == "" {
		return managed.Frame{}, fmt.Errorf("device agent frame identity or epoch mismatch: %w", ErrDeviceFence)
	}
	canonical, err := frame.CanonicalSigningBytes()
	if err != nil || frame.SigningKeyID != a.controllerKey.Fingerprint() || !a.controllerKey.Verify(canonical, frame.Signature) {
		return managed.Frame{}, ErrDeviceLinkUnauthenticated
	}
	var command DeviceCommand
	if err := decodeStrictJSON(frame.Payload, &command); err != nil {
		return managed.Frame{}, fmt.Errorf("decode device agent command: %w", ErrDeviceAgentProtocol)
	}
	if command.ID != frame.CommandID || command.Operation != frame.ResourceID || command.DeviceID != a.deviceID || command.ConnectionEpoch != a.connectionEpoch {
		return managed.Frame{}, fmt.Errorf("typed device agent command mismatch: %w", ErrDeviceFence)
	}
	if err := validateDeviceCommand(command); err != nil {
		return managed.Frame{}, fmt.Errorf("unsupported device agent command: %w", ErrDeviceAgentProtocol)
	}
	digest := sha256.Sum256(frame.Payload)
	digestText := hex.EncodeToString(digest[:])

	a.mu.Lock()
	defer a.mu.Unlock()
	// Admit the signed frame before consulting the durable result cache. A
	// A cached command may be retried with a new message/sequence after a lost
	// response, but an old signed frame must still be rejected by the replay
	// cursor. Looking in the cache first would turn that old frame into a
	// replayable authorization token after a process restart. The cache key is
	// also scoped to the connection epoch: a reconnect must not reuse a reply
	// for a command whose signed payload carries a new epoch fence.
	if err := a.replay.Accept(ctx, frame, now); err != nil {
		return managed.Frame{}, err
	}
	resultKey := deviceResultKey(a.connectionEpoch, command.ID)
	if cached, cacheErr := a.results.Load(ctx, resultKey); cacheErr == nil {
		if cached.RequestDigest != digestText {
			return managed.Frame{}, ErrIdempotencyConflict
		}
		return a.responseFrame(frame, cached.Reply)
	} else if !errors.Is(cacheErr, ErrDeviceResultNotFound) {
		return managed.Frame{}, cacheErr
	}
	reply, err := a.handler(ctx, command)
	if err != nil {
		reply = DeviceReply{Accepted: false, Error: deviceReplyError(err)}
	}
	if reply.DeviceID == "" {
		reply.DeviceID = a.deviceID
	}
	if reply.ConnectionEpoch == 0 {
		reply.ConnectionEpoch = a.connectionEpoch
	}
	if reply.DeviceID != a.deviceID || reply.ConnectionEpoch != a.connectionEpoch {
		return managed.Frame{}, ErrDeviceFence
	}
	if reply.MessageID == 0 {
		if a.nextMessageID == ^uint64(0) {
			return managed.Frame{}, ErrDeviceAgentProtocol
		}
		a.nextMessageID++
		reply.MessageID = a.nextMessageID
	}
	if err := a.results.Save(ctx, resultKey, DeviceCommandResult{RequestDigest: digestText, Reply: reply}); err != nil {
		return managed.Frame{}, err
	}
	return a.responseFrame(frame, reply)
}

func deviceResultKey(connectionEpoch uint64, commandID string) string {
	return fmt.Sprintf("%d:%s", connectionEpoch, commandID)
}

func deviceReplyError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > maxDeviceReplyError {
		return message[:maxDeviceReplyError]
	}
	return message
}

func (a *DeviceAgent) responseFrame(request managed.Frame, reply DeviceReply) (managed.Frame, error) {
	if a.nextMessageID == ^uint64(0) || a.nextSequence == ^uint64(0) {
		return managed.Frame{}, ErrDeviceAgentProtocol
	}
	a.nextMessageID++
	a.nextSequence++
	payload, err := json.Marshal(reply)
	if err != nil {
		return managed.Frame{}, fmt.Errorf("encode device agent reply: %w", err)
	}
	now := a.clock().UTC()
	digest := sha256.Sum256(payload)
	frame := managed.Frame{
		Major: managed.ProtocolMajor, Minor: managed.ProtocolMinor,
		OrganizationID: a.organizationID, DeviceID: a.deviceID,
		ConnectionEpoch: a.connectionEpoch, ControllerEpoch: a.controllerEpoch,
		MessageID: a.nextMessageID, CommandID: request.CommandID,
		ExecutionID: request.ExecutionID, SessionID: request.SessionID, ResourceID: request.ResourceID,
		CausationID: request.CommandID, CorrelationID: request.CommandID, Sequence: a.nextSequence,
		IssuedAt: now, ExpiresAt: now.Add(a.expiresAfter), PayloadType: "event",
		PayloadDigest: digest[:], Payload: payload, SigningKeyID: a.identity.Fingerprint(),
	}
	canonical, err := frame.CanonicalSigningBytes()
	if err != nil {
		return managed.Frame{}, err
	}
	frame.Signature, err = a.identity.Sign(canonical)
	if err != nil {
		return managed.Frame{}, err
	}
	if err := frame.Validate(now); err != nil {
		return managed.Frame{}, fmt.Errorf("validate device agent reply: %w", err)
	}
	return frame, nil
}

func cloneDeviceCommandResult(value DeviceCommandResult) DeviceCommandResult {
	value.Reply.Payload = append([]byte(nil), value.Reply.Payload...)
	return value
}

var _ DeviceResultStore = (*MemoryDeviceResultStore)(nil)
var _ DeviceResultStore = (*FileDeviceResultStore)(nil)
