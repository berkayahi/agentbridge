package localcontrol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/managed"
)

var (
	ErrDeviceLinkUnauthenticated = errors.New("localcontrol: device link is not authenticated")
	ErrDeviceLinkProtocol        = errors.New("localcontrol: invalid device link protocol message")
)

const (
	// A device command may include a cold provider start on a resource-limited
	// Pi. Keep the signed command short-lived, but long enough to cover that
	// bounded startup path. The upper validation bound below remains five
	// minutes so callers cannot turn this into an unbounded authorization.
	// Verification and delivery can include a cold Go/npm install on a
	// resource-constrained Pi. Keep the signed command bounded below the
	// five-minute protocol ceiling while leaving enough room for the reply.
	defaultDeviceCommandExpiry = 4 * time.Minute
	deviceLinkCapability       = "local-control-request-response"
)

// SignedDeviceLinkConfig describes the controller side of a paired-device
// request/response link. The controller key is local authority material; the
// peer key is the public key recorded by pairing. Neither key is sent in a
// command payload.
type SignedDeviceLinkConfig struct {
	Transport       managed.Transport
	Identity        deviceidentity.Key
	PeerPublicKey   []byte
	OrganizationID  string
	DeviceID        string
	ConnectionEpoch uint64
	ControllerEpoch uint64
	Clock           func() time.Time
	ExpiresAfter    time.Duration
	NextSequence    DeviceLinkSequence
}

// DeviceLinkSequence reserves monotonically increasing protocol counters
// before a frame is sent. It is persisted by the controller so a short-lived
// WSS connection or a controller restart cannot replay a new command at an
// old message/sequence number.
type DeviceLinkSequence func(context.Context) (messageID, sequence uint64, err error)

// WebSocketDeviceLinkConfig is the concrete WSS construction used for a
// paired Pi endpoint. The endpoint must be wss://; certificate verification is
// left enabled by managed.NewWebSocketTransport and cannot be disabled here.
type WebSocketDeviceLinkConfig struct {
	Identity        deviceidentity.Key
	PeerPublicKey   []byte
	OrganizationID  string
	DeviceID        string
	ConnectionEpoch uint64
	ControllerEpoch uint64
	Endpoint        string
	Origin          string
	TLSConfig       *tls.Config
	Header          http.Header
	Dialer          *net.Dialer
	Clock           func() time.Time
	ExpiresAfter    time.Duration
	NextSequence    DeviceLinkSequence
	MaxMessageBytes int
	ReadPoll        time.Duration
}

// SignedDeviceLink turns the public managed frame contract into the typed
// local-control DeviceLink. Calls are serialized because a request/response
// transport has one in-flight receive stream; FencedLink should wrap this link
// when it is installed into a DeviceRuntime.
type SignedDeviceLink struct {
	transport       managed.Transport
	identity        deviceidentity.Key
	peerPublicKey   []byte
	organizationID  string
	deviceID        string
	connectionEpoch uint64
	controllerEpoch uint64
	clock           func() time.Time
	expiresAfter    time.Duration
	nextSequence    DeviceLinkSequence

	mu                sync.Mutex
	handshaken        bool
	nextMessageID     uint64
	nextSequenceValue uint64
}

func NewSignedDeviceLink(config SignedDeviceLinkConfig) (*SignedDeviceLink, error) {
	if config.Transport == nil || !config.Identity.HasPrivate() || len(config.PeerPublicKey) != ed25519.PublicKeySize || !validID(config.OrganizationID) || !validID(config.DeviceID) || config.ConnectionEpoch == 0 || config.ControllerEpoch == 0 {
		return nil, ErrInvalidRequest
	}
	if _, ok := config.Transport.(managed.HandshakeTransport); !ok {
		return nil, ErrDeviceLinkUnauthenticated
	}
	if _, err := deviceidentity.FromPublic(config.PeerPublicKey); err != nil {
		return nil, ErrDeviceLinkUnauthenticated
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.ExpiresAfter <= 0 {
		config.ExpiresAfter = defaultDeviceCommandExpiry
	}
	if config.ExpiresAfter >= 5*time.Minute {
		return nil, ErrInvalidRequest
	}
	return &SignedDeviceLink{
		transport:       config.Transport,
		identity:        config.Identity,
		peerPublicKey:   append([]byte(nil), config.PeerPublicKey...),
		organizationID:  config.OrganizationID,
		deviceID:        config.DeviceID,
		connectionEpoch: config.ConnectionEpoch,
		controllerEpoch: config.ControllerEpoch,
		clock:           config.Clock,
		expiresAfter:    config.ExpiresAfter,
		nextSequence:    config.NextSequence,
	}, nil
}

// NewWebSocketDeviceLink dials the configured secure Pi endpoint and applies
// the same signed handshake and frame checks as NewSignedDeviceLink.
func NewWebSocketDeviceLink(ctx context.Context, config WebSocketDeviceLinkConfig) (*SignedDeviceLink, error) {
	transport, err := managed.NewWebSocketTransport(ctx, managed.WebSocketConfig{
		URL: config.Endpoint, Origin: config.Origin, TLSConfig: config.TLSConfig,
		Header: config.Header, Dialer: config.Dialer,
		MaxMessageBytes: config.MaxMessageBytes, ReadPoll: config.ReadPoll,
	})
	if err != nil {
		return nil, err
	}
	link, err := NewSignedDeviceLink(SignedDeviceLinkConfig{
		Transport: transport, Identity: config.Identity, PeerPublicKey: config.PeerPublicKey,
		OrganizationID: config.OrganizationID, DeviceID: config.DeviceID,
		ConnectionEpoch: config.ConnectionEpoch, ControllerEpoch: config.ControllerEpoch,
		Clock: config.Clock, ExpiresAfter: config.ExpiresAfter, NextSequence: config.NextSequence,
	})
	if err != nil {
		_ = transport.Close()
		return nil, err
	}
	return link, nil
}

func (l *SignedDeviceLink) Execute(ctx context.Context, command DeviceCommand) (DeviceReply, error) {
	if l == nil || l.transport == nil {
		return DeviceReply{}, ErrDeviceLinkUnavailable
	}
	if err := ctx.Err(); err != nil {
		return DeviceReply{}, err
	}
	if err := validateDeviceCommand(command); err != nil {
		return DeviceReply{}, err
	}
	if command.DeviceID != l.deviceID || command.ConnectionEpoch != l.connectionEpoch {
		return DeviceReply{}, fmt.Errorf("device command is outside the signed link fence: %w", ErrDeviceFence)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureHandshake(ctx); err != nil {
		return DeviceReply{}, err
	}
	frame, err := l.nextCommandFrame(ctx, command)
	if err != nil {
		return DeviceReply{}, err
	}
	if err := l.transport.Send(ctx, frame); err != nil {
		return DeviceReply{}, fmt.Errorf("send device command: %w", err)
	}
	response, err := l.transport.Receive(ctx)
	if err != nil {
		return DeviceReply{}, fmt.Errorf("receive device reply: %w", err)
	}
	return l.decodeReply(response, frame, command)
}

func (l *SignedDeviceLink) Close() error {
	if l == nil || l.transport == nil {
		return nil
	}
	return l.transport.Close()
}

func (l *SignedDeviceLink) ensureHandshake(ctx context.Context) error {
	if l.handshaken {
		return nil
	}
	handshaker, ok := l.transport.(managed.HandshakeTransport)
	if !ok {
		return ErrDeviceLinkUnauthenticated
	}
	local, err := managed.SignHandshake(managed.Handshake{
		Major: managed.ProtocolMajor, Minor: managed.ProtocolMinor,
		OrganizationID: l.organizationID, DeviceID: l.deviceID,
		ConnectionEpoch: l.connectionEpoch, ControllerEpoch: l.controllerEpoch,
		Capabilities: []string{deviceLinkCapability},
	}, l.identity)
	if err != nil {
		return fmt.Errorf("sign device link handshake: %w", err)
	}
	remote, err := handshaker.PerformHandshake(ctx, local)
	if err != nil {
		return fmt.Errorf("perform device link handshake: %w", err)
	}
	if remote.OrganizationID != l.organizationID || remote.DeviceID != l.deviceID || remote.ConnectionEpoch != l.connectionEpoch || remote.ControllerEpoch != l.controllerEpoch {
		return fmt.Errorf("device link handshake identity or epoch mismatch: %w", ErrDeviceFence)
	}
	if !hasCapability(remote.Capabilities, deviceLinkCapability) {
		return fmt.Errorf("device link capability was not negotiated: %w", ErrDeviceLinkProtocol)
	}
	if _, err := managed.Negotiate(local, remote); err != nil {
		return fmt.Errorf("negotiate device link handshake: %w", err)
	}
	if err := managed.VerifyHandshakeSignature(remote, l.peerPublicKey); err != nil {
		return fmt.Errorf("verify device link handshake: %w", ErrDeviceLinkUnauthenticated)
	}
	l.handshaken = true
	return nil
}

func (l *SignedDeviceLink) nextCommandFrame(ctx context.Context, command DeviceCommand) (managed.Frame, error) {
	if l.nextSequence != nil {
		messageID, sequence, err := l.nextSequence(ctx)
		if err != nil {
			return managed.Frame{}, err
		}
		if messageID == 0 || sequence == 0 {
			return managed.Frame{}, ErrDeviceLinkProtocol
		}
		l.nextMessageID, l.nextSequenceValue = messageID, sequence
	} else {
		if l.nextMessageID == ^uint64(0) || l.nextSequenceValue == ^uint64(0) {
			return managed.Frame{}, ErrDeviceLinkProtocol
		}
		l.nextMessageID++
		l.nextSequenceValue++
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return managed.Frame{}, fmt.Errorf("encode device command: %w", err)
	}
	now := l.clock().UTC()
	digest := sha256.Sum256(payload)
	frame := managed.Frame{
		Major: managed.ProtocolMajor, Minor: managed.ProtocolMinor,
		OrganizationID: l.organizationID, DeviceID: l.deviceID,
		ConnectionEpoch: l.connectionEpoch, ControllerEpoch: l.controllerEpoch,
		MessageID: l.nextMessageID, CommandID: command.ID,
		ExecutionID: command.ExecutionID, SessionID: command.SessionID, ResourceID: command.Operation,
		CausationID: command.ID, CorrelationID: command.ID, Sequence: l.nextSequenceValue,
		IssuedAt: now, ExpiresAt: now.Add(l.expiresAfter), PayloadType: "command",
		PayloadDigest: digest[:], Payload: payload, SigningKeyID: l.identity.Fingerprint(),
	}
	canonical, err := frame.CanonicalSigningBytes()
	if err != nil {
		return managed.Frame{}, fmt.Errorf("canonicalize device command: %w", err)
	}
	frame.Signature, err = l.identity.Sign(canonical)
	if err != nil {
		return managed.Frame{}, fmt.Errorf("sign device command: %w", err)
	}
	if err := frame.Validate(now); err != nil {
		return managed.Frame{}, fmt.Errorf("validate device command: %w", err)
	}
	return frame, nil
}

func (l *SignedDeviceLink) decodeReply(frame managed.Frame, commandFrame managed.Frame, command DeviceCommand) (DeviceReply, error) {
	now := l.clock().UTC()
	if err := frame.Validate(now); err != nil {
		return DeviceReply{}, fmt.Errorf("validate device reply: %w", ErrDeviceLinkProtocol)
	}
	if frame.PayloadType != "event" || frame.OrganizationID != l.organizationID || frame.DeviceID != l.deviceID || frame.ConnectionEpoch != l.connectionEpoch || frame.ControllerEpoch != l.controllerEpoch || frame.CommandID != command.ID || frame.CorrelationID != command.ID || frame.ResourceID != command.Operation {
		return DeviceReply{}, fmt.Errorf("device reply correlation or fence mismatch: %w", ErrDeviceLinkProtocol)
	}
	peer, err := deviceidentity.FromPublic(l.peerPublicKey)
	if err != nil {
		return DeviceReply{}, ErrDeviceLinkUnauthenticated
	}
	canonical, err := frame.CanonicalSigningBytes()
	if err != nil || frame.SigningKeyID != peer.Fingerprint() || !peer.Verify(canonical, frame.Signature) {
		return DeviceReply{}, ErrDeviceLinkUnauthenticated
	}
	var reply DeviceReply
	if err := decodeStrictJSON(frame.Payload, &reply); err != nil {
		return DeviceReply{}, fmt.Errorf("decode device reply: %w", ErrDeviceLinkProtocol)
	}
	if reply.MessageID == 0 || reply.DeviceID != commandFrame.DeviceID || reply.ConnectionEpoch != commandFrame.ConnectionEpoch {
		return DeviceReply{}, fmt.Errorf("device reply identity or epoch mismatch: %w", ErrDeviceFence)
	}
	return reply, nil
}

func decodeStrictJSON(value []byte, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(value), managed.MaxPayloadBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON")
		}
		return err
	}
	return nil
}

var _ DeviceLink = (*SignedDeviceLink)(nil)
