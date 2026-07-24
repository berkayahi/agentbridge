package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/managed"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

// composeDeviceAgent creates the headless Pi processor from the same runtime
// adapters and repository profiles used by the standalone controller. The
// controller remains authoritative; the shadow v2 task row on the Pi exists
// only to persist provider sessions, workspace evidence, and restart recovery.
func composeDeviceAgent(
	cfg config.DeviceAgentConfig,
	data *sqlite.RuntimeStore,
	executor *localRuntimeExecutor,
	verifier localVerifier,
	committer localCommitter,
) (*http.Server, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	identity, err := deviceidentity.Load(cfg.IdentityPath)
	if err != nil {
		return nil, fmt.Errorf("load device-agent identity: %w", err)
	}
	controllerPublicKey, err := loadDevicePublicKey(cfg.ControllerPublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load device-agent controller key: %w", err)
	}
	state, err := managed.NewFileStateStore(cfg.ReplayStatePath)
	if err != nil {
		return nil, fmt.Errorf("open device-agent replay state: %w", err)
	}
	replay, err := managed.NewReplayGuardWithInbox(state, cfg.OrganizationID, cfg.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("configure device-agent replay guard: %w", err)
	}
	results, err := localcontrol.NewFileDeviceResultStore(cfg.ResultsPath)
	if err != nil {
		return nil, fmt.Errorf("open device-agent result store: %w", err)
	}
	handler := newDeviceExecutionHandler(data, executor, verifier, committer)
	agent, err := localcontrol.NewDeviceAgent(localcontrol.DeviceAgentConfig{
		Identity: identity, ControllerPublicKey: controllerPublicKey,
		OrganizationID: cfg.OrganizationID, DeviceID: cfg.DeviceID,
		ConnectionEpoch: cfg.ConnectionEpoch, ControllerEpoch: cfg.ControllerEpoch,
		Replay: replay, Results: results, Handler: handler.Handle,
	})
	if err != nil {
		return nil, fmt.Errorf("configure device agent: %w", err)
	}
	handlerHTTP, err := localcontrol.NewDeviceAgentWebSocketHandler(agent, managed.MaxFrameBytes, time.Second)
	if err != nil {
		return nil, fmt.Errorf("configure device-agent WSS handler: %w", err)
	}
	if err := validateDeviceTLSFiles(cfg.TLSCertPath, cfg.TLSKeyPath); err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           handlerHTTP,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}, nil
}

type deviceExecutionHandler struct {
	store     *sqlite.RuntimeStore
	executor  *localRuntimeExecutor
	verifier  localVerifier
	committer localCommitter
}

const maxDeviceObservationEvents = 200

func newDeviceExecutionHandler(data *sqlite.RuntimeStore, executor *localRuntimeExecutor, verifier localVerifier, committer localCommitter) *deviceExecutionHandler {
	return &deviceExecutionHandler{store: data, executor: executor, verifier: verifier, committer: committer}
}

func (h *deviceExecutionHandler) Handle(ctx context.Context, command localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
	if h == nil || h.store == nil || h.executor == nil {
		return localcontrol.DeviceReply{}, localcontrol.ErrNotConfigured
	}
	if err := h.ensureShadowTask(ctx, command); err != nil {
		return localcontrol.DeviceReply{}, err
	}
	if command.Operation == "observe" {
		return h.observe(ctx, command)
	}
	view, err := h.view(ctx, command)
	if err != nil {
		return localcontrol.DeviceReply{}, err
	}
	switch command.Operation {
	case "start":
		var request localcontrol.StartRequest
		if err := decodeDevicePayload(command.Payload, &request); err != nil {
			return localcontrol.DeviceReply{}, err
		}
		request.TaskID, request.Revision = command.TaskID, command.Revision
		if request.IdempotencyKey == "" {
			request.IdempotencyKey = command.ID
		}
		if err := h.executor.Start(ctx, view, request); err != nil {
			return localcontrol.DeviceReply{}, err
		}
		return acceptedDeviceReply(), nil
	case "resume":
		var request localcontrol.ResumeRequest
		if err := decodeDevicePayload(command.Payload, &request); err != nil {
			return localcontrol.DeviceReply{}, err
		}
		request.TaskID, request.Revision = command.TaskID, command.Revision
		if request.IdempotencyKey == "" {
			request.IdempotencyKey = command.ID
		}
		if err := h.executor.Resume(ctx, view, request); err != nil {
			return localcontrol.DeviceReply{}, err
		}
		return acceptedDeviceReply(), nil
	case "approve":
		var payload struct {
			ApprovalID string `json:"approval_id"`
			UserID     string `json:"user_id"`
			Allow      bool   `json:"allow"`
		}
		if err := decodeDevicePayload(command.Payload, &payload); err != nil {
			return localcontrol.DeviceReply{}, err
		}
		if err := h.executor.Approve(ctx, view, payload.ApprovalID, payload.UserID, payload.Allow); err != nil {
			return localcontrol.DeviceReply{}, err
		}
		return acceptedDeviceReply(), nil
	case "cancel":
		if err := h.executor.Cancel(ctx, view); err != nil {
			return localcontrol.DeviceReply{}, err
		}
		return acceptedDeviceReply(), nil
	case "verify":
		receipt, err := h.verifier.Verify(ctx, view)
		if err != nil {
			return localcontrol.DeviceReply{}, err
		}
		return encodedDeviceReply(receipt)
	case "commit":
		receipt, err := h.committer.Commit(ctx, view)
		if err != nil {
			return localcontrol.DeviceReply{}, err
		}
		return encodedDeviceReply(receipt)
	default:
		return localcontrol.DeviceReply{}, localcontrol.ErrInvalidRequest
	}
}

func (h *deviceExecutionHandler) observe(ctx context.Context, command localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
	var payload struct {
		AfterCursor uint64 `json:"after_cursor"`
	}
	if err := decodeDevicePayload(command.Payload, &payload); err != nil {
		return localcontrol.DeviceReply{}, err
	}
	if payload.AfterCursor > 1_000_000_000 {
		return localcontrol.DeviceReply{}, fmt.Errorf("device observation cursor: %w", localcontrol.ErrInvalidRequest)
	}
	events, err := h.store.Events(ctx, command.TaskID)
	if err != nil {
		return localcontrol.DeviceReply{}, err
	}
	if payload.AfterCursor > uint64(len(events)) {
		return localcontrol.DeviceReply{}, fmt.Errorf("device observation cursor is ahead of the event log: %w", localcontrol.ErrInvalidRequest)
	}
	// Cursor is the last cursor actually returned, not the current total event
	// count. Keeping it at the batch boundary is what lets the controller resume
	// a bounded response without skipping events that follow the first page.
	observation := localcontrol.DeviceObservation{
		Cursor:    payload.AfterCursor,
		Events:    make([]localcontrol.DeviceEvent, 0),
		Approvals: make([]localcontrol.ApprovalView, 0),
	}
	for index, event := range events {
		cursor := uint64(index + 1)
		if cursor <= payload.AfterCursor {
			continue
		}
		if len(observation.Events) >= maxDeviceObservationEvents {
			break
		}
		observation.Events = append(observation.Events, localcontrol.DeviceEvent{
			Cursor: cursor, ID: event.ID, TaskID: event.TaskID, Type: string(event.Type),
			Payload: append(json.RawMessage(nil), event.Payload...), CreatedAt: event.CreatedAt,
		})
		observation.Cursor = cursor
	}
	approvals, err := h.store.PendingApprovals(ctx)
	if err != nil {
		return localcontrol.DeviceReply{}, err
	}
	for _, approval := range approvals {
		if approval.TaskID != command.TaskID {
			continue
		}
		observation.Approvals = append(observation.Approvals, localcontrol.ApprovalView{
			ID: approval.ID, TaskID: approval.TaskID, Kind: approval.Kind, Status: string(approval.Status),
			RequestPayload: append(json.RawMessage(nil), approval.RequestPayload...),
			RequestedAt:    approval.RequestedAt, ExpiresAt: approval.ExpiresAt,
		})
	}
	return encodedDeviceReply(observation)
}

func (h *deviceExecutionHandler) ensureShadowTask(ctx context.Context, command localcontrol.DeviceCommand) error {
	if command.TaskID == "" || command.RepositoryID == "" || !command.Provider.Valid() || strings.TrimSpace(command.Title) == "" || strings.TrimSpace(command.Prompt) == "" {
		return fmt.Errorf("device command lacks execution manifest: %w", localcontrol.ErrInvalidRequest)
	}
	if command.RepositoryRemote != "" {
		binding, err := h.store.GetRepository(ctx, command.RepositoryID)
		if err == nil {
			if binding.Remote != command.RepositoryRemote {
				return fmt.Errorf("device repository binding changed for %q: %w", command.RepositoryID, localcontrol.ErrDeviceFence)
			}
		} else if errors.Is(err, store.ErrNotFound) {
			if err := h.store.EnsureRepositoryBinding(ctx, command.RepositoryID, command.RepositoryRemote); err != nil {
				return fmt.Errorf("persist device repository binding: %w", err)
			}
		} else {
			return err
		}
	}
	existing, err := h.store.Task(ctx, command.TaskID)
	if err == nil {
		if existing.RepoProfileID != command.RepositoryID || existing.Provider != command.Provider || existing.Title != command.Title || existing.Prompt != command.Prompt {
			return fmt.Errorf("device execution manifest changed for %q: %w", command.TaskID, localcontrol.ErrDeviceFence)
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	now := time.Now().UTC()
	initial, err := json.Marshal(map[string]string{"source": "paired-device", "device_id": command.DeviceID})
	if err != nil {
		return err
	}
	value := workmodel.Task{
		ID: command.TaskID, RepoProfileID: command.RepositoryID, Title: command.Title,
		Prompt: command.Prompt, Provider: command.Provider, State: workmodel.Queued,
		CreatedAt: now, UpdatedAt: now,
	}
	err = h.store.CreateTask(ctx, value, workmodel.Event{
		ID: command.TaskID + "-device-created", TaskID: command.TaskID,
		Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityInternal,
		Payload: initial, CreatedAt: now,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrConflict) {
		return err
	}
	existing, loadErr := h.store.Task(ctx, command.TaskID)
	if loadErr != nil {
		return errors.Join(err, loadErr)
	}
	if existing.RepoProfileID != command.RepositoryID || existing.Provider != command.Provider || existing.Title != command.Title || existing.Prompt != command.Prompt {
		return fmt.Errorf("device execution manifest conflicted for %q: %w", command.TaskID, localcontrol.ErrDeviceFence)
	}
	return nil
}

func (h *deviceExecutionHandler) view(ctx context.Context, command localcontrol.DeviceCommand) (localcontrol.TaskView, error) {
	task, err := h.store.Task(ctx, command.TaskID)
	if err != nil {
		return localcontrol.TaskView{}, err
	}
	runtimeID := command.RuntimeID
	if runtimeID == "" {
		runtimeID = string(task.Provider)
	}
	repositoryRemote := ""
	if repository, repositoryErr := h.store.GetRepository(ctx, task.RepoProfileID); repositoryErr == nil {
		repositoryRemote = repository.Remote
	}
	return localcontrol.TaskView{
		ID: command.TaskID, RepositoryID: task.RepoProfileID, RepositoryRemote: repositoryRemote, TargetDeviceID: localcontrol.LocalDeviceID,
		TargetEpoch: 1, Title: task.Title, Prompt: task.Prompt, Provider: task.Provider,
		State: task.State, Revision: task.Revision, ExecutionID: command.ExecutionID,
		SessionID: command.SessionID, RuntimeID: runtimeID, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}, nil
}

func acceptedDeviceReply() localcontrol.DeviceReply {
	return localcontrol.DeviceReply{Accepted: true}
}

func encodedDeviceReply(value any) (localcontrol.DeviceReply, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return localcontrol.DeviceReply{}, err
	}
	return localcontrol.DeviceReply{Accepted: true, Payload: payload}, nil
}

func decodeDevicePayload(payload []byte, destination any) error {
	if len(payload) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode device command payload: %w", localcontrol.ErrInvalidRequest)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode device command payload: %w", localcontrol.ErrInvalidRequest)
	}
	return nil
}

func loadDevicePublicKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("controller public key file is not owner-only regular data")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(contents) == ed25519.PublicKeySize {
		return append([]byte(nil), contents...), nil
	}
	text := strings.TrimSpace(string(contents))
	for _, encoding := range []*base64.Encoding{base64.RawStdEncoding, base64.StdEncoding, base64.RawURLEncoding, base64.URLEncoding} {
		decoded, decodeErr := encoding.DecodeString(text)
		if decodeErr == nil && len(decoded) == ed25519.PublicKeySize {
			return decoded, nil
		}
	}
	return nil, deviceidentity.ErrInvalidKey
}

func validateDeviceTLSFiles(certPath, keyPath string) error {
	for name, path := range map[string]string{"certificate": certPath, "private key": keyPath} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect device-agent TLS %s: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("device-agent TLS %s is not a regular file", name)
		}
		if name == "private key" && info.Mode().Perm()&0o077 != 0 {
			return errors.New("device-agent TLS private key is not owner-only")
		}
	}
	return nil
}
