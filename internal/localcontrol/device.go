package localcontrol

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

const LocalDeviceID = "local-mac"

type DeviceKind string

const (
	DeviceKindLocalMac    DeviceKind = "local_mac"
	DeviceKindRaspberryPi DeviceKind = "raspberry_pi"
)

type DeviceState string

const (
	DeviceStatePaired      DeviceState = "paired"
	DeviceStateUnreachable DeviceState = "unreachable"
	DeviceStateRevoked     DeviceState = "revoked"
)

var (
	ErrDeviceNotPaired    = fmt.Errorf("localcontrol: device is not paired")
	ErrDeviceUnreachable  = fmt.Errorf("localcontrol: device is unreachable")
	ErrDeviceRevoked      = fmt.Errorf("localcontrol: device is revoked")
	ErrDeviceFence        = fmt.Errorf("localcontrol: device assignment is fenced")
	ErrPairingExpired     = fmt.Errorf("localcontrol: pairing challenge expired")
	ErrPairingUsed        = fmt.Errorf("localcontrol: pairing challenge already used")
	ErrInvalidDeviceProof = fmt.Errorf("localcontrol: invalid device proof")
)

type Device struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Kind            DeviceKind  `json:"kind"`
	Fingerprint     string      `json:"fingerprint"`
	Endpoint        string      `json:"endpoint,omitempty"`
	State           DeviceState `json:"state"`
	ConnectionEpoch uint64      `json:"connection_epoch"`
	Revision        int64       `json:"revision"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

type DeviceAssignment struct {
	TaskID             string    `json:"task_id"`
	DeviceID           string    `json:"device_id"`
	AssignmentEpoch    uint64    `json:"assignment_epoch"`
	LastAckCursor      uint64    `json:"last_ack_cursor"`
	LastObservedCursor uint64    `json:"last_observed_cursor"`
	State              string    `json:"state"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type DeviceCommandState string

const (
	DeviceCommandPending   DeviceCommandState = "pending"
	DeviceCommandInFlight  DeviceCommandState = "in_flight"
	DeviceCommandCompleted DeviceCommandState = "completed"
	DeviceCommandFailed    DeviceCommandState = "failed"
)

// DeviceCommandRecord is the controller-owned durable work item for a
// remote-device operation. RequestPayload is the typed local-control request,
// never a provider command or filesystem path.
type DeviceCommandRecord struct {
	ID              string             `json:"id"`
	TaskID          string             `json:"task_id"`
	DeviceID        string             `json:"device_id"`
	AssignmentEpoch uint64             `json:"assignment_epoch"`
	Operation       string             `json:"operation"`
	RequestHash     string             `json:"request_hash"`
	RequestPayload  json.RawMessage    `json:"request_payload,omitempty"`
	Revision        int64              `json:"revision"`
	State           DeviceCommandState `json:"state"`
	Attempts        int                `json:"attempts"`
	LastError       string             `json:"last_error,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

type PairingChallenge struct {
	ID                  string    `json:"id"`
	DeviceID            string    `json:"device_id"`
	BrowserFingerprint  string    `json:"browser_fingerprint"`
	Nonce               string    `json:"nonce"`
	TrustSetDigest      string    `json:"trust_set_digest"`
	ControllerPublicKey []byte    `json:"controller_public_key,omitempty"`
	ExpiresAt           time.Time `json:"expires_at"`
	CreatedAt           time.Time `json:"created_at"`
}

type CreatePairingChallengeRequest struct {
	DeviceID           string `json:"device_id"`
	BrowserFingerprint string `json:"browser_fingerprint"`
	IdempotencyKey     string `json:"idempotency_key"`
}

type PairDeviceRequest struct {
	ChallengeID    string     `json:"challenge_id"`
	Name           string     `json:"name"`
	Kind           DeviceKind `json:"kind"`
	Endpoint       string     `json:"endpoint,omitempty"`
	PublicKey      []byte     `json:"public_key"`
	Signature      []byte     `json:"signature"`
	IdempotencyKey string     `json:"idempotency_key"`
}

type RotateDeviceRequest struct {
	DeviceID       string `json:"device_id"`
	Revision       int64  `json:"revision"`
	PublicKey      []byte `json:"public_key"`
	IdempotencyKey string `json:"idempotency_key"`
}

type DeviceMutationRequest struct {
	DeviceID       string `json:"device_id"`
	Revision       int64  `json:"revision"`
	IdempotencyKey string `json:"idempotency_key"`
}

type SelectDeviceRequest struct {
	TaskID         string `json:"task_id"`
	Revision       int64  `json:"revision"`
	TargetDeviceID string `json:"target_device_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type DeviceResponse struct {
	Device Device `json:"device"`
}
type DevicesResponse struct {
	Devices []Device `json:"devices"`
}
type PairingChallengeResponse struct {
	Challenge PairingChallenge `json:"challenge"`
}

// AtomicDeviceMutation is the durable unit for device lifecycle changes. The
// production SQLite store applies the device row, optional challenge
// consumption, local event, and idempotency response together.
type AtomicDeviceMutation struct {
	ChallengeID             string
	Create                  bool
	ExpectedRevision        int64
	ExpectedConnectionEpoch uint64
	ExpectedState           DeviceState
	Device                  Device
	PublicKey               []byte
	Event                   Event
	Idempotency             IdempotencyRecord
}

// AtomicDeviceAuthority is optional so deterministic non-SQLite controller
// fakes can keep using the smaller DeviceAuthority contract. Production
// RuntimeStore implements it and is the only runtime composition used by
// standalone serve.
type AtomicDeviceAuthority interface {
	ApplyDeviceMutation(context.Context, AtomicDeviceMutation) error
}

// AtomicCreationAuthority keeps the first local resource row, its audit/event
// record, and the retry response in one production transaction.
type AtomicCreationAuthority interface {
	CreateProjectAtomically(context.Context, Project, Event, IdempotencyRecord) error
	CreateRepositoryAtomically(context.Context, Repository, Event, IdempotencyRecord) error
	CreateBoardAtomically(context.Context, Board, Event, IdempotencyRecord) error
	CreateTaskAtomically(context.Context, AtomicTaskCreation) (Event, error)
}

// AtomicTaskCreation is the complete first durable task boundary. The task,
// execution/session lineage, device assignment, runtime event, local event,
// and idempotency response must commit or roll back together.
type AtomicTaskCreation struct {
	ProjectID      string
	BoardID        string
	TargetDeviceID string
	Task           workmodel.Task
	InitialEvent   workmodel.Event
	LocalEvent     Event
	Idempotency    IdempotencyRecord
}

type AssignmentResponse struct {
	Task       TaskView         `json:"task"`
	Assignment DeviceAssignment `json:"assignment"`
	Event      Event            `json:"event"`
}

type ReplayDeviceCommandsRequest struct {
	DeviceID string `json:"device_id,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type ReplayDeviceCommandsResponse struct {
	DeviceID string                `json:"device_id"`
	Replayed int                   `json:"replayed"`
	Pending  []DeviceCommandRecord `json:"pending"`
}

type DeviceAuthority interface {
	CreateDevice(context.Context, Device, []byte) error
	GetDevice(context.Context, string) (Device, error)
	DevicePublicKey(context.Context, string) ([]byte, error)
	NextDeviceLinkSequence(context.Context, string) (uint64, uint64, error)
	ListDevices(context.Context) ([]Device, error)
	UpdateDevice(context.Context, Device, []byte) error
	MarkDeviceUnreachable(context.Context, string) error
	CreatePairingChallenge(context.Context, PairingChallenge) error
	GetPairingChallenge(context.Context, string) (PairingChallenge, error)
	ConsumePairingChallenge(context.Context, string, time.Time) error
	TaskDevice(context.Context, string) (DeviceAssignment, error)
	AdvanceTaskDeviceObservationCursor(context.Context, string, string, uint64, uint64) error
	ApplyDeviceObservation(context.Context, string, string, uint64, int64, uint64, []Event, []workmodel.Approval) error
	AssignTaskDevice(context.Context, string, int64, string, uint64, Event) (DeviceAssignment, Event, error)
	EnqueueDeviceCommand(context.Context, DeviceCommandRecord) error
	GetDeviceCommand(context.Context, string) (DeviceCommandRecord, error)
	ClaimDeviceCommand(context.Context, string, time.Time) (bool, error)
	ResetDeviceCommand(context.Context, string, string, time.Time) error
	CompleteDeviceCommand(context.Context, string, time.Time) error
	FailDeviceCommand(context.Context, string, string, time.Time) error
	ListPendingDeviceCommands(context.Context, string, int) ([]DeviceCommandRecord, error)
}

func newPairingNonce() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func pairingChallenge(now time.Time, request CreatePairingChallengeRequest, id string, controllerPublicKey []byte) (PairingChallenge, deviceidentity.Claim, deviceidentity.Challenge, error) {
	deviceID := strings.TrimSpace(request.DeviceID)
	browserFingerprint := strings.TrimSpace(request.BrowserFingerprint)
	if !validID(deviceID) || browserFingerprint == "" || len(browserFingerprint) > 256 {
		return PairingChallenge{}, deviceidentity.Claim{}, deviceidentity.Challenge{}, ErrInvalidRequest
	}
	nonce, err := newPairingNonce()
	if err != nil {
		return PairingChallenge{}, deviceidentity.Claim{}, deviceidentity.Challenge{}, err
	}
	if len(controllerPublicKey) != 0 && len(controllerPublicKey) != ed25519.PublicKeySize {
		return PairingChallenge{}, deviceidentity.Claim{}, deviceidentity.Challenge{}, ErrInvalidRequest
	}
	challenge := PairingChallenge{ID: id, DeviceID: deviceID, BrowserFingerprint: browserFingerprint, Nonce: nonce, TrustSetDigest: "local-device-routing-v1", ControllerPublicKey: append([]byte(nil), controllerPublicKey...), ExpiresAt: now.Add(10 * time.Minute), CreatedAt: now}
	claim := deviceidentity.Claim{ID: id, OrganizationID: "local", DeviceID: deviceID, BrowserFingerprint: browserFingerprint, ExpiresAt: challenge.ExpiresAt}
	proofChallenge := deviceidentity.Challenge{ClaimID: id, OrganizationID: "local", DeviceID: deviceID, Nonce: nonce, TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt}
	return challenge, claim, proofChallenge, nil
}
