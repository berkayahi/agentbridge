package localcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/store"
)

func (s *Service) ListDevices(ctx context.Context) (DevicesResponse, error) {
	values, err := s.store.ListDevices(ctx)
	if err != nil {
		return DevicesResponse{}, err
	}
	return DevicesResponse{Devices: values}, nil
}

func (s *Service) CreatePairingChallenge(ctx context.Context, request CreatePairingChallengeRequest) (PairingChallengeResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if !s.identity.HasPrivate() {
		return PairingChallengeResponse{}, ErrNotConfigured
	}

	deviceID := strings.TrimSpace(request.DeviceID)
	browserFingerprint := strings.TrimSpace(request.BrowserFingerprint)
	payload := struct {
		DeviceID           string `json:"device_id"`
		BrowserFingerprint string `json:"browser_fingerprint"`
	}{deviceID, browserFingerprint}
	var cached PairingChallengeResponse
	if done, err := s.replay(ctx, request.IdempotencyKey, "create_pairing_challenge", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(deviceID) || browserFingerprint == "" {
		return PairingChallengeResponse{}, ErrInvalidRequest
	}
	now := s.clock().UTC()
	challenge, _, _, err := pairingChallenge(now, request, s.newID("challenge"), s.identity.PublicKey())
	if err != nil {
		return PairingChallengeResponse{}, err
	}
	if err := s.store.CreatePairingChallenge(ctx, challenge); err != nil {
		return PairingChallengeResponse{}, err
	}
	response := PairingChallengeResponse{Challenge: challenge}
	if err := s.remember(ctx, request.IdempotencyKey, "create_pairing_challenge", payload, response); err != nil {
		return PairingChallengeResponse{}, err
	}
	return response, nil
}

func (s *Service) PairDevice(ctx context.Context, request PairDeviceRequest) (DeviceResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		ChallengeID string     `json:"challenge_id"`
		Name        string     `json:"name"`
		Kind        DeviceKind `json:"kind"`
		Endpoint    string     `json:"endpoint"`
		PublicKey   []byte     `json:"public_key"`
		Signature   []byte     `json:"signature"`
	}{request.ChallengeID, strings.TrimSpace(request.Name), request.Kind, strings.TrimSpace(request.Endpoint), request.PublicKey, request.Signature}
	var cached DeviceResponse
	if done, err := s.replay(ctx, request.IdempotencyKey, "pair_device", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.ChallengeID) || payload.Name == "" || request.Kind != DeviceKindRaspberryPi || len(request.PublicKey) != 32 || len(request.Signature) != 64 || !validDeviceEndpoint(payload.Endpoint) {
		return DeviceResponse{}, ErrInvalidRequest
	}
	challenge, err := s.store.GetPairingChallenge(ctx, request.ChallengeID)
	if err != nil {
		return DeviceResponse{}, err
	}
	now := s.clock().UTC()
	if !now.Before(challenge.ExpiresAt) {
		return DeviceResponse{}, ErrPairingExpired
	}
	claim := deviceidentity.Claim{ID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID, BrowserFingerprint: challenge.BrowserFingerprint, ExpiresAt: challenge.ExpiresAt}
	proofChallenge := deviceidentity.Challenge{ClaimID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID, Nonce: challenge.Nonce, TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt}
	proof := deviceidentity.Proof{ClaimID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID, Nonce: challenge.Nonce, PublicKey: append([]byte(nil), request.PublicKey...), Signature: append([]byte(nil), request.Signature...), TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt}
	if err := deviceidentity.VerifyProof(claim, proofChallenge, proof, now); err != nil {
		return DeviceResponse{}, fmt.Errorf("verify device pairing: %w", ErrInvalidDeviceProof)
	}
	fingerprint := deviceidentity.EnrollmentFingerprint(request.PublicKey)
	device, existingErr := s.store.GetDevice(ctx, challenge.DeviceID)
	mutation := AtomicDeviceMutation{ChallengeID: challenge.ID, PublicKey: append([]byte(nil), request.PublicKey...)}
	switch {
	case existingErr == nil && device.State != DeviceStateRevoked:
		return DeviceResponse{}, fmt.Errorf("pair device %q: %w", challenge.DeviceID, store.ErrConflict)
	case existingErr == nil:
		// A revoked enrollment may be replaced only through a fresh challenge
		// and proof. Preserve the device identity, fence all old assignments via
		// UpdateDevice, and advance the connection epoch so late frames from the
		// revoked key cannot become valid again.
		if device.ConnectionEpoch == ^uint64(0) || device.Revision == int64(^uint64(0)>>1) {
			return DeviceResponse{}, fmt.Errorf("re-enroll device %q: %w", challenge.DeviceID, ErrInvalidRequest)
		}
		mutation.ExpectedRevision, mutation.ExpectedConnectionEpoch, mutation.ExpectedState = device.Revision, device.ConnectionEpoch, device.State
		device.Name = payload.Name
		device.Kind = request.Kind
		device.Fingerprint = fingerprint
		device.Endpoint = payload.Endpoint
		device.State = DeviceStatePaired
		device.ConnectionEpoch++
		device.Revision++
		device.UpdatedAt = now
	case errors.Is(existingErr, store.ErrNotFound):
		device = Device{ID: challenge.DeviceID, Name: payload.Name, Kind: request.Kind, Fingerprint: fingerprint, Endpoint: payload.Endpoint, State: DeviceStatePaired, ConnectionEpoch: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
		mutation.Create = true
	default:
		return DeviceResponse{}, existingErr
	}
	eventType := "device_paired"
	if existingErr == nil {
		eventType = "device_reenrolled"
	}
	response := DeviceResponse{Device: device}
	mutation.Device = device
	mutation.Event = localEvent(s.newID("event"), "device", device.ID, "", device.Revision, eventType, map[string]any{"fingerprint": device.Fingerprint, "connection_epoch": device.ConnectionEpoch}, now)
	if err := s.persistDeviceMutation(ctx, "pair_device", request.IdempotencyKey, payload, response, mutation); err != nil {
		return DeviceResponse{}, err
	}
	return response, nil
}

func (s *Service) RotateDevice(ctx context.Context, request RotateDeviceRequest) (DeviceResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		DeviceID  string `json:"device_id"`
		Revision  int64  `json:"revision"`
		PublicKey []byte `json:"public_key"`
	}{request.DeviceID, request.Revision, request.PublicKey}
	var cached DeviceResponse
	if done, err := s.replay(ctx, request.IdempotencyKey, "rotate_device", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.DeviceID) || request.Revision <= 0 || len(request.PublicKey) != 32 {
		return DeviceResponse{}, ErrInvalidRequest
	}
	device, err := s.store.GetDevice(ctx, request.DeviceID)
	if err != nil {
		return DeviceResponse{}, err
	}
	if device.Kind != DeviceKindRaspberryPi || device.State == DeviceStateRevoked {
		return DeviceResponse{}, ErrDeviceRevoked
	}
	if device.Revision != request.Revision {
		return DeviceResponse{}, fmt.Errorf("device revision %d, expected %d: %w", device.Revision, request.Revision, ErrStaleRevision)
	}
	if device.ConnectionEpoch == ^uint64(0) || device.Revision == int64(^uint64(0)>>1) {
		return DeviceResponse{}, fmt.Errorf("rotate device %q: %w", device.ID, ErrInvalidRequest)
	}
	mutation := AtomicDeviceMutation{ExpectedRevision: device.Revision, ExpectedConnectionEpoch: device.ConnectionEpoch, ExpectedState: device.State}
	device.Fingerprint = deviceidentity.EnrollmentFingerprint(request.PublicKey)
	device.ConnectionEpoch++
	device.Revision++
	device.UpdatedAt = s.clock().UTC()
	response := DeviceResponse{Device: device}
	mutation.Device = device
	mutation.PublicKey = append([]byte(nil), request.PublicKey...)
	mutation.Event = localEvent(s.newID("event"), "device", device.ID, "", device.Revision, "device_key_rotated", map[string]any{"fingerprint": device.Fingerprint, "connection_epoch": device.ConnectionEpoch}, device.UpdatedAt)
	if err := s.persistDeviceMutation(ctx, "rotate_device", request.IdempotencyKey, payload, response, mutation); err != nil {
		return DeviceResponse{}, err
	}
	return response, nil
}

func (s *Service) SetDeviceState(ctx context.Context, request DeviceMutationRequest, state DeviceState) (DeviceResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		DeviceID string      `json:"device_id"`
		Revision int64       `json:"revision"`
		State    DeviceState `json:"state"`
	}{request.DeviceID, request.Revision, state}
	operation := "set_device_" + string(state)
	var cached DeviceResponse
	if done, err := s.replay(ctx, request.IdempotencyKey, operation, payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.DeviceID) || request.Revision <= 0 || (state != DeviceStatePaired && state != DeviceStateUnreachable && state != DeviceStateRevoked) {
		return DeviceResponse{}, ErrInvalidRequest
	}
	device, err := s.store.GetDevice(ctx, request.DeviceID)
	if err != nil {
		return DeviceResponse{}, err
	}
	if device.Revision != request.Revision {
		return DeviceResponse{}, fmt.Errorf("device revision %d, expected %d: %w", device.Revision, request.Revision, ErrStaleRevision)
	}
	if device.State == DeviceStateRevoked {
		return DeviceResponse{}, ErrDeviceRevoked
	}
	if device.Revision == int64(^uint64(0)>>1) || (state == DeviceStatePaired && device.ConnectionEpoch == ^uint64(0)) {
		return DeviceResponse{}, fmt.Errorf("update device %q: %w", device.ID, ErrInvalidRequest)
	}
	mutation := AtomicDeviceMutation{ExpectedRevision: device.Revision, ExpectedConnectionEpoch: device.ConnectionEpoch, ExpectedState: device.State}
	device.State = state
	device.Revision++
	device.UpdatedAt = s.clock().UTC()
	if state == DeviceStatePaired {
		device.ConnectionEpoch++
	}
	response := DeviceResponse{Device: device}
	mutation.Device = device
	mutation.Event = localEvent(s.newID("event"), "device", device.ID, "", device.Revision, "device_state_changed", map[string]any{"state": state, "connection_epoch": device.ConnectionEpoch}, device.UpdatedAt)
	if err := s.persistDeviceMutation(ctx, operation, request.IdempotencyKey, payload, response, mutation); err != nil {
		return DeviceResponse{}, err
	}
	return response, nil
}

func (s *Service) persistDeviceMutation(ctx context.Context, operation, idempotencyKey string, payload any, response DeviceResponse, mutation AtomicDeviceMutation) error {
	if atomic, ok := s.store.(AtomicDeviceAuthority); ok {
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		mutation.Idempotency = IdempotencyRecord{
			Key: idempotencyKey, Operation: operation, RequestHash: requestHash(operation, payload),
			ResponseBytes: encoded, CreatedAt: s.clock().UTC(),
		}
		return atomic.ApplyDeviceMutation(ctx, mutation)
	}
	if mutation.Create {
		if err := s.store.CreateDevice(ctx, mutation.Device, mutation.PublicKey); err != nil {
			return err
		}
	} else if err := s.store.UpdateDevice(ctx, mutation.Device, mutation.PublicKey); err != nil {
		return err
	}
	if mutation.ChallengeID != "" {
		if err := s.store.ConsumePairingChallenge(ctx, mutation.ChallengeID, mutation.Device.UpdatedAt); err != nil {
			return err
		}
	}
	if _, err := s.store.AppendLocalEvent(ctx, mutation.Event); err != nil {
		return err
	}
	return s.remember(ctx, idempotencyKey, operation, payload, response)
}

func (s *Service) SelectTaskDevice(ctx context.Context, request SelectDeviceRequest) (AssignmentResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	targetID := strings.TrimSpace(request.TargetDeviceID)
	payload := struct {
		TaskID         string `json:"task_id"`
		Revision       int64  `json:"revision"`
		TargetDeviceID string `json:"target_device_id"`
	}{request.TaskID, request.Revision, targetID}
	var cached AssignmentResponse
	if done, err := s.replay(ctx, request.IdempotencyKey, "select_task_device", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.TaskID) || !validID(targetID) || request.Revision <= 0 {
		return AssignmentResponse{}, ErrInvalidRequest
	}
	view, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return AssignmentResponse{}, err
	}
	if err := checkRevision(view, request.Revision); err != nil {
		return AssignmentResponse{}, err
	}
	device, err := s.store.GetDevice(ctx, targetID)
	if err != nil {
		return AssignmentResponse{}, err
	}
	if err := requirePairedDevice(device); err != nil {
		return AssignmentResponse{}, err
	}
	now := s.clock().UTC()
	event := localEvent(s.newID("event"), "task", request.TaskID, request.TaskID, request.Revision+1, "device_selected", map[string]any{"device_id": targetID, "connection_epoch": device.ConnectionEpoch}, now)
	assignment, storedEvent, err := s.store.AssignTaskDevice(ctx, request.TaskID, request.Revision, targetID, device.ConnectionEpoch, event)
	if err != nil {
		return AssignmentResponse{}, err
	}
	next, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return AssignmentResponse{}, err
	}
	response := AssignmentResponse{Task: next, Assignment: assignment, Event: storedEvent}
	if err := s.remember(ctx, request.IdempotencyKey, "select_task_device", payload, response); err != nil {
		return AssignmentResponse{}, err
	}
	return response, nil
}

func validDeviceEndpoint(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.User != nil || parsed.Fragment != "" || strings.Contains(value, "#") || parsed.Host == "" {
		return false
	}
	// The paired-device adapter speaks signed WebSocket frames. Accepting an
	// HTTPS endpoint here would defer a configuration error until the first
	// task execution, where the transport can only dial WSS.
	return parsed.Scheme == "wss"
}
