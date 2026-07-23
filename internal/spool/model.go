// Package spool defines the durable device-to-platform event spool.
package spool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalid              = errors.New("spool: invalid event")
	ErrSpoolPaused          = errors.New("spool: non-critical event admission paused")
	ErrCriticalReserve      = errors.New("spool: critical reserve exhausted")
	ErrAckGap               = errors.New("spool: acknowledgement is not contiguous")
	ErrReceiptConflict      = errors.New("spool: receipt conflicts with an earlier receipt")
	ErrDuplicatePayload     = errors.New("spool: duplicate event has a different payload")
	ErrInvalidConfiguration = errors.New("spool: invalid quota configuration")
)

type Lane string

const (
	LaneCritical   Lane = "critical"
	LaneStructured Lane = "structured"
	LaneDiagnostic Lane = "diagnostic"
)

const (
	DefaultMaxBytes              int64 = 64 << 20
	DefaultWarningWatermarkBytes int64 = 48 << 20
	DefaultCriticalWatermark     int64 = 56 << 20
	DefaultCriticalReserveBytes  int64 = 8 << 20
	DefaultReplayLimit                 = 128
	MaxReplayLimit                     = 4096
)

// Config controls admission and retention pressure for the local spool.
// Bytes measure encoded event payloads; SQLite page overhead is intentionally
// outside the product quota so an operator can reason about event retention.
type Config struct {
	MaxBytes               int64
	WarningWatermarkBytes  int64
	CriticalWatermarkBytes int64
	CriticalReserveBytes   int64
}

func DefaultConfig() Config {
	return Config{
		MaxBytes:               DefaultMaxBytes,
		WarningWatermarkBytes:  DefaultWarningWatermarkBytes,
		CriticalWatermarkBytes: DefaultCriticalWatermark,
		CriticalReserveBytes:   DefaultCriticalReserveBytes,
	}
}

func (c Config) Normalize() Config {
	defaults := DefaultConfig()
	if c.MaxBytes <= 0 {
		c.MaxBytes = defaults.MaxBytes
	}
	if c.WarningWatermarkBytes <= 0 {
		c.WarningWatermarkBytes = defaults.WarningWatermarkBytes
	}
	if c.CriticalWatermarkBytes <= 0 {
		c.CriticalWatermarkBytes = defaults.CriticalWatermarkBytes
	}
	if c.CriticalReserveBytes <= 0 {
		c.CriticalReserveBytes = defaults.CriticalReserveBytes
	}
	return c
}

func (c Config) Validate() error {
	if c.MaxBytes <= 0 || c.WarningWatermarkBytes <= 0 || c.CriticalWatermarkBytes <= 0 || c.CriticalReserveBytes <= 0 {
		return ErrInvalidConfiguration
	}
	if c.WarningWatermarkBytes >= c.CriticalWatermarkBytes || c.CriticalWatermarkBytes > c.MaxBytes {
		return fmt.Errorf("%w: watermarks must be increasing within max bytes", ErrInvalidConfiguration)
	}
	if c.CriticalReserveBytes >= c.MaxBytes || c.MaxBytes-c.CriticalReserveBytes < c.CriticalWatermarkBytes {
		return fmt.Errorf("%w: critical reserve leaves insufficient non-critical capacity", ErrInvalidConfiguration)
	}
	return nil
}

func (l Lane) Valid() bool {
	return l == LaneCritical || l == LaneStructured || l == LaneDiagnostic
}

// LaneForType supplies a conservative default for events crossing a provider
// or kernel boundary. Callers may always override it explicitly.
func LaneForType(eventType string) Lane {
	typeName := strings.ToLower(strings.TrimSpace(eventType))
	if strings.HasPrefix(typeName, "provider_") {
		typeName = strings.TrimPrefix(typeName, "provider_")
	}
	for _, marker := range []string{"cancel", "revo", "approval", "auth", "receipt", "complete", "final", "git_", "git-", "critical", "reconciliation", "diagnostic_truncated"} {
		if strings.Contains(typeName, marker) {
			return LaneCritical
		}
	}
	for _, marker := range []string{"heartbeat", "terminal", "stdout", "stderr", "output", "chunk", "trace", "debug"} {
		if strings.Contains(typeName, marker) {
			return LaneDiagnostic
		}
	}
	return LaneStructured
}

type Event struct {
	ExecutionID     string
	Sequence        uint64
	Lane            Lane
	Type            string
	ProviderEventID string
	CoalesceKey     string
	Payload         []byte
	CreatedAt       time.Time
}

func (e Event) Normalize(now time.Time) (Event, error) {
	e.ExecutionID = strings.TrimSpace(e.ExecutionID)
	e.Type = strings.TrimSpace(e.Type)
	e.ProviderEventID = strings.TrimSpace(e.ProviderEventID)
	e.CoalesceKey = strings.TrimSpace(e.CoalesceKey)
	if e.ExecutionID == "" || e.Type == "" {
		return Event{}, fmt.Errorf("%w: execution and type are required", ErrInvalid)
	}
	if e.Lane == "" {
		e.Lane = LaneForType(e.Type)
	}
	if !e.Lane.Valid() {
		return Event{}, fmt.Errorf("%w: unknown lane", ErrInvalid)
	}
	if e.Sequence != 0 {
		return Event{}, fmt.Errorf("%w: sequence is assigned by the spool", ErrInvalid)
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.CreatedAt = e.CreatedAt.UTC()
	e.Payload = append([]byte(nil), e.Payload...)
	return e, nil
}

type Message struct {
	MessageID       uint64
	ExecutionID     string
	Sequence        uint64
	Lane            Lane
	Type            string
	ProviderEventID string
	Payload         []byte
	PayloadHash     string
	SizeBytes       int64
	CreatedAt       time.Time
}

func (m Message) VerifyPayload() bool {
	digest := sha256.Sum256(m.Payload)
	return strings.EqualFold(m.PayloadHash, hex.EncodeToString(digest[:]))
}

type AppendRequest struct {
	Event  Event
	Config Config
}

type AppendResult struct {
	Message   Message
	Duplicate bool
	Truncated bool
}

type ReplayRequest struct {
	AfterMessageID uint64
	Limit          int
}

type AcknowledgeRequest struct {
	HighestContiguous uint64
	ReceiptID         string
	PayloadHash       string
	ReceivedAt        time.Time
}

type AcknowledgeResult struct {
	HighestContiguous uint64
	Acknowledged      uint64
	Duplicate         bool
}

type Receipt struct {
	ID          string
	MessageID   uint64
	PayloadHash string
	Status      string
	ReceivedAt  time.Time
}

type ReceiptResult struct {
	Receipt   Receipt
	Duplicate bool
}

type Usage struct {
	BytesUsed           int64
	MaxBytes            int64
	WarningWatermark    int64
	CriticalWatermark   int64
	AcknowledgedThrough uint64
}

// ArtifactMetadata is the only artifact shape that belongs in the ordinary
// event spool. It contains ciphertext/object facts and never accepts
// plaintext, encryption keys, grant signatures, or replay nonces.
type ArtifactMetadata struct {
	ExecutionID     string
	ArtifactID      string
	ObjectKey       string
	Algorithm       string
	KeyID           string
	PolicyDigest    string
	MediaType       string
	SizeBytes       int64
	PlaintextDigest string
	EnvelopeDigest  string
	Status          string
	CreatedAt       time.Time
}

func (m ArtifactMetadata) Normalize(now time.Time) (ArtifactMetadata, error) {
	m.ExecutionID = strings.TrimSpace(m.ExecutionID)
	m.ArtifactID = strings.TrimSpace(m.ArtifactID)
	m.ObjectKey = strings.TrimSpace(m.ObjectKey)
	m.Algorithm = strings.TrimSpace(m.Algorithm)
	m.KeyID = strings.TrimSpace(m.KeyID)
	m.PolicyDigest = strings.TrimSpace(m.PolicyDigest)
	m.MediaType = strings.TrimSpace(m.MediaType)
	m.PlaintextDigest = strings.TrimSpace(m.PlaintextDigest)
	m.EnvelopeDigest = strings.TrimSpace(m.EnvelopeDigest)
	m.Status = strings.TrimSpace(m.Status)
	if m.ExecutionID == "" || m.ArtifactID == "" || m.ObjectKey == "" || m.Algorithm != "AES-256-GCM" || m.KeyID == "" || m.PolicyDigest == "" || m.MediaType == "" || m.SizeBytes < 0 || !validDigest(m.PlaintextDigest) || !validDigest(m.EnvelopeDigest) || m.Status == "" {
		return ArtifactMetadata{}, ErrInvalid
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.CreatedAt = m.CreatedAt.UTC()
	return m, nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func (m ArtifactMetadata) MarshalJSON() ([]byte, error) {
	type safeArtifactMetadata struct {
		ExecutionID, ArtifactID, ObjectKey, Algorithm, KeyID, PolicyDigest, MediaType string
		SizeBytes                                                                     int64
		PlaintextDigest, EnvelopeDigest, Status                                       string
		CreatedAt                                                                     time.Time
	}
	return json.Marshal(safeArtifactMetadata{
		ExecutionID: m.ExecutionID, ArtifactID: m.ArtifactID, ObjectKey: m.ObjectKey,
		Algorithm: m.Algorithm, KeyID: m.KeyID, PolicyDigest: m.PolicyDigest,
		MediaType: m.MediaType, SizeBytes: m.SizeBytes, PlaintextDigest: m.PlaintextDigest,
		EnvelopeDigest: m.EnvelopeDigest, Status: m.Status, CreatedAt: m.CreatedAt,
	})
}

// Store is implemented by a durable backend. Append owns quota admission and
// ID allocation in the same transaction as the event insert.
type Store interface {
	Append(context.Context, AppendRequest) (AppendResult, error)
	Replay(context.Context, ReplayRequest) ([]Message, error)
	Acknowledge(context.Context, AcknowledgeRequest) (AcknowledgeResult, error)
	RecordReceipt(context.Context, Receipt) (ReceiptResult, error)
	Usage(context.Context) (Usage, error)
}
