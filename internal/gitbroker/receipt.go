package gitbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

var (
	ErrNotFound               = errors.New("gitbroker: not found")
	ErrIdempotencyConflict    = errors.New("gitbroker: idempotency key conflict")
	ErrReconciliationRequired = errors.New("gitbroker: reconciliation required")
)

type ReceiptStatus string

const (
	ReceiptSucceeded ReceiptStatus = "succeeded"
	ReceiptFailed    ReceiptStatus = "failed"
	ReceiptReconcile ReceiptStatus = "reconciliation_required"
)

// Receipt is immutable evidence for one operation attempt. Signature is over
// the same value with Signature cleared, so changing any observed field fails
// verification.
type Receipt struct {
	ID                 string
	OperationID        string
	OperationDigest    string
	IdempotencyKey     string
	Kind               Kind
	Status             ReceiptStatus
	BeforeSHA          string
	AfterSHA           string
	ExpectedOldSHA     string
	TargetRef          string
	ProviderResponseID string
	StartedAt          time.Time
	FinishedAt         time.Time
	ClaimEpoch         uint64
	ControllerEpoch    uint64
	OutputDigest       string
	ErrorCode          string
	Reconciliation     bool
	Signature          []byte
}

func (r Receipt) SigningBytes() []byte {
	copyValue := r
	copyValue.Signature = nil
	value, _ := json.Marshal(copyValue)
	return value
}

func (r Receipt) Digest() [32]byte { return sha256.Sum256(r.SigningBytes()) }

func (r Receipt) DigestHex() string {
	digest := r.Digest()
	return hex.EncodeToString(digest[:])
}

type Journal interface {
	CreateIntent(context.Context, Operation) error
	FindByIdempotencyKey(context.Context, string) (Operation, error)
	GetReceipt(context.Context, string) (Receipt, error)
	SaveReceipt(context.Context, Receipt) error
}

// MemoryJournal is useful for standalone operation and focused integration;
// managed composition must provide a durable Journal implementation.
type MemoryJournal struct {
	mu         sync.Mutex
	operations map[string]Operation
	byKey      map[string]string
	receipts   map[string]Receipt
}

func NewMemoryJournal() *MemoryJournal {
	return &MemoryJournal{operations: make(map[string]Operation), byKey: make(map[string]string), receipts: make(map[string]Receipt)}
}

func (j *MemoryJournal) CreateIntent(ctx context.Context, value Operation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := value.Validate(); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if existingID, ok := j.byKey[value.IdempotencyKey]; ok {
		existing := j.operations[existingID]
		if existing.Digest() != value.Digest() {
			return ErrIdempotencyConflict
		}
		return nil
	}
	if existing, ok := j.operations[value.ID]; ok && existing.Digest() != value.Digest() {
		return ErrOperationConflict
	}
	j.operations[value.ID] = value
	j.byKey[value.IdempotencyKey] = value.ID
	return nil
}

func (j *MemoryJournal) FindByIdempotencyKey(ctx context.Context, key string) (Operation, error) {
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	id, ok := j.byKey[key]
	if !ok {
		return Operation{}, ErrNotFound
	}
	return j.operations[id], nil
}

func (j *MemoryJournal) GetReceipt(ctx context.Context, id string) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	value, ok := j.receipts[id]
	if !ok {
		return Receipt{}, ErrNotFound
	}
	value.Signature = append([]byte(nil), value.Signature...)
	return value, nil
}

func (j *MemoryJournal) SaveReceipt(ctx context.Context, value Receipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if value.ID == "" || value.OperationID == "" || value.OperationDigest == "" {
		return ErrInvalidOperation
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	operation, ok := j.operations[value.OperationID]
	if !ok {
		return ErrNotFound
	}
	if operation.DigestHex() != value.OperationDigest {
		return ErrOperationConflict
	}
	if existing, ok := j.receipts[value.OperationID]; ok {
		if existing.DigestHex() != value.DigestHex() {
			return ErrOperationConflict
		}
		return nil
	}
	value.Signature = append([]byte(nil), value.Signature...)
	j.receipts[value.OperationID] = value
	return nil
}

var _ Journal = (*MemoryJournal)(nil)
