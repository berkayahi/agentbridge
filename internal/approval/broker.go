// Package approval relays task-scoped provider decisions through Telegram.
package approval

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/security"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/berkayahi/agentbridge/internal/telegram"
)

const (
	defaultTimeout  = 10 * time.Minute
	maxProviderID   = 256
	maxKindBytes    = 128
	maxSummary      = 4 * 1024
	maxVisibleRunes = 3_000
	persistTimeout  = time.Second
)

var (
	ErrInvalidRequest = errors.New("approval: invalid request")
	ErrNotPending     = errors.New("approval: not pending")
	ErrMismatch       = errors.New("approval: decision does not match request")
	ErrUnauthorized   = errors.New("approval: unauthorized user")
)

type Store interface {
	UpsertApproval(context.Context, task.Approval) error
	AppendEvent(context.Context, task.Event) error
	Events(context.Context, string) ([]task.Event, error)
}

type Messenger interface {
	Send(context.Context, telegram.Message) (telegram.MessageRef, error)
}

type Redactor interface {
	RedactString(string) string
}

type Config struct {
	Store         Store
	Messenger     Messenger
	Signer        *telegram.CallbackSigner
	Redactor      Redactor
	Timeout       time.Duration
	Clock         func() time.Time
	NewID         func() string
	AuthorizeUser func(string) bool
}

type Request struct {
	TaskID            string
	ChatID            int64
	ProviderRequestID string
	Kind              string
	Summary           string
}

type Result struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}

type pending struct {
	record task.Approval
	result chan Result

	mu       sync.Mutex
	canceled bool
	finished bool
}

type Broker struct {
	store         Store
	messenger     Messenger
	signer        *telegram.CallbackSigner
	redactor      Redactor
	timeout       time.Duration
	clock         func() time.Time
	newID         func() string
	authorizeUser func(string) bool

	mu      sync.Mutex
	pending map[string]*pending
}

func New(config Config) (*Broker, error) {
	if config.Store == nil || config.Messenger == nil || config.Signer == nil || config.AuthorizeUser == nil {
		return nil, errors.New("approval: incomplete configuration")
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultTimeout
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.NewID == nil {
		config.NewID = randomID
	}
	if config.Redactor == nil {
		config.Redactor = security.NewRedactor(security.Config{MaxPayloadRunes: maxVisibleRunes})
	}
	return &Broker{
		store: config.Store, messenger: config.Messenger, signer: config.Signer,
		redactor: config.Redactor, timeout: config.Timeout, clock: config.Clock,
		newID: config.NewID, authorizeUser: config.AuthorizeUser,
		pending: make(map[string]*pending),
	}, nil
}

// Request persists and sends an approval, then blocks until one decision,
// cancellation, or the configured timeout. Cancellation and timeout fail closed.
func (b *Broker) Request(ctx context.Context, request Request) (Result, error) {
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	approvalID, keyboard, err := b.newApproval(request.TaskID)
	if err != nil {
		return Result{}, err
	}
	now := b.clock().UTC()
	expires := now.Add(b.timeout)
	summary := truncateRunes(b.redactor.RedactString(request.Summary), maxVisibleRunes)
	kind := truncateRunes(b.redactor.RedactString(request.Kind), maxKindBytes)
	payload, err := json.Marshal(requestPayload{
		ProviderRequestID: truncateRunes(b.redactor.RedactString(request.ProviderRequestID), maxProviderID),
		ChatID:            request.ChatID,
		Summary:           summary,
	})
	if err != nil {
		return Result{}, fmt.Errorf("approval: encode request: %w", err)
	}
	value := task.Approval{
		ID: approvalID, TaskID: request.TaskID, Kind: kind,
		Status: task.ApprovalPending, RequestPayload: payload,
		RequestedAt: now, ExpiresAt: &expires,
	}
	waiter := &pending{record: value, result: make(chan Result, 1)}
	if !b.reserve(approvalID, waiter) {
		return Result{}, errors.New("approval: identifier collision")
	}
	if err := b.store.UpsertApproval(ctx, value); err != nil {
		b.remove(approvalID, waiter)
		return Result{}, fmt.Errorf("approval: persist request: %w", err)
	}
	message := telegram.Message{
		ChatID:         request.ChatID,
		Text:           fmt.Sprintf("Approval required (%s): %s", kind, summary),
		InlineKeyboard: keyboard,
	}
	if _, err := b.messenger.Send(ctx, message); err != nil {
		b.cancel(waiter)
		b.remove(approvalID, waiter)
		_ = b.persistExpiredDetached(ctx, waiter, "approval delivery failed")
		return Result{}, fmt.Errorf("approval: send Telegram request: %w", err)
	}

	remaining := expires.Sub(b.clock().UTC())
	if remaining <= 0 {
		return b.expire(ctx, approvalID, waiter, "approval timed out")
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case result := <-waiter.result:
		return result, nil
	case <-ctx.Done():
		return b.expire(ctx, approvalID, waiter, "approval canceled")
	case <-timer.C:
		return b.expire(ctx, approvalID, waiter, "approval timed out")
	}
}

// HandleDecision consumes one matching, authorized callback. The durable
// decision is written before an approval is released to the waiting provider.
func (b *Broker) HandleDecision(ctx context.Context, taskID, approvalID, userID string, allow bool) error {
	if !validUserID(userID) || !b.authorizeUser(userID) {
		return ErrUnauthorized
	}
	waiter, err := b.take(taskID, approvalID)
	if err != nil {
		return err
	}
	waiter.mu.Lock()
	if waiter.canceled || waiter.finished {
		waiter.mu.Unlock()
		return ErrNotPending
	}

	now := b.clock().UTC()
	value := waiter.record
	value.Status = task.ApprovalRejected
	reason := "rejected by Telegram operator"
	if allow {
		value.Status = task.ApprovalApproved
		reason = "approved by Telegram operator"
	}
	value.ResolvedAt = &now
	value.DecisionPayload, _ = json.Marshal(decisionPayload{Approved: allow, UserID: userID, Reason: reason})
	if err := b.store.UpsertApproval(ctx, value); err != nil {
		b.restore(approvalID, waiter)
		waiter.mu.Unlock()
		return fmt.Errorf("approval: persist decision: %w", err)
	}
	eventPayload, _ := json.Marshal(map[string]any{"approved": allow})
	event := task.Event{
		ID: approvalID + "-resolved", TaskID: taskID,
		Type: task.EventApprovalResolved, Visibility: task.VisibilityUser,
		Payload: eventPayload, CreatedAt: now,
	}
	if err := b.appendDecisionEvent(ctx, event); err != nil {
		restoreErr := b.store.UpsertApproval(ctx, waiter.record)
		b.restore(approvalID, waiter)
		waiter.mu.Unlock()
		return errors.Join(fmt.Errorf("approval: persist decision event: %w", err), restoreErr)
	}

	waiter.finished = true
	waiter.result <- Result{Approved: allow, Reason: reason}
	waiter.mu.Unlock()
	return nil
}

func (b *Broker) appendDecisionEvent(ctx context.Context, event task.Event) error {
	err := b.store.AppendEvent(ctx, event)
	if err == nil || !errors.Is(err, store.ErrDuplicateEvent) {
		return err
	}
	events, readErr := b.store.Events(ctx, event.TaskID)
	if readErr != nil {
		return errors.Join(err, fmt.Errorf("approval: verify duplicate decision event: %w", readErr))
	}
	for _, existing := range events {
		if existing.ID != event.ID {
			continue
		}
		if existing.TaskID == event.TaskID &&
			existing.Type == event.Type &&
			existing.Visibility == event.Visibility &&
			existing.ProviderEventID == event.ProviderEventID &&
			string(existing.Payload) == string(event.Payload) {
			return nil
		}
		return errors.Join(err, errors.New("approval: duplicate decision event conflicts with persisted event"))
	}
	return errors.Join(err, errors.New("approval: duplicate decision event is not readable"))
}

// Owns reports whether this broker currently owns the exact pending approval.
// It performs no user authorization and does not consume the decision.
func (b *Broker) Owns(taskID, approvalID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	waiter, ok := b.pending[approvalID]
	return ok && waiter.record.TaskID == taskID
}

func (b *Broker) newApproval(taskID string) (string, telegram.InlineKeyboard, error) {
	for range 8 {
		id := strings.TrimSpace(b.newID())
		keyboard, err := telegram.ApprovalKeyboard(b.signer, taskID, id, b.timeout)
		if err != nil {
			return "", nil, fmt.Errorf("approval: create callback: %w", err)
		}
		b.mu.Lock()
		_, exists := b.pending[id]
		b.mu.Unlock()
		if !exists {
			return id, keyboard, nil
		}
	}
	return "", nil, errors.New("approval: could not allocate identifier")
}

func (b *Broker) reserve(id string, waiter *pending) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.pending[id]; exists {
		return false
	}
	b.pending[id] = waiter
	return true
}

func (b *Broker) take(taskID, approvalID string) (*pending, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	waiter, ok := b.pending[approvalID]
	if !ok {
		return nil, ErrNotPending
	}
	if waiter.record.TaskID != taskID {
		return nil, ErrMismatch
	}
	delete(b.pending, approvalID)
	return waiter, nil
}

func (b *Broker) remove(id string, waiter *pending) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending[id] == waiter {
		delete(b.pending, id)
	}
}

func (b *Broker) restore(id string, waiter *pending) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.pending[id]; !exists {
		b.pending[id] = waiter
	}
}

func (b *Broker) cancel(waiter *pending) {
	waiter.mu.Lock()
	waiter.canceled = true
	waiter.mu.Unlock()
}

func (b *Broker) finish(waiter *pending, result Result) {
	waiter.mu.Lock()
	defer waiter.mu.Unlock()
	if waiter.canceled || waiter.finished {
		return
	}
	waiter.finished = true
	waiter.result <- result
}

func (b *Broker) expire(ctx context.Context, approvalID string, waiter *pending, reason string) (Result, error) {
	waiter.mu.Lock()
	if waiter.finished {
		result := <-waiter.result
		waiter.mu.Unlock()
		return result, nil
	}
	waiter.canceled = true
	waiter.mu.Unlock()
	b.remove(approvalID, waiter)
	if err := b.persistExpiredDetached(ctx, waiter, reason); err != nil {
		return Result{Reason: reason}, err
	}
	return Result{Reason: reason}, nil
}

func (b *Broker) persistExpiredDetached(parent context.Context, waiter *pending, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), persistTimeout)
	defer cancel()
	return b.persistExpired(ctx, waiter, reason)
}

func (b *Broker) persistExpired(ctx context.Context, waiter *pending, reason string) error {
	now := b.clock().UTC()
	value := waiter.record
	value.Status = task.ApprovalExpired
	value.ResolvedAt = &now
	value.DecisionPayload, _ = json.Marshal(decisionPayload{Approved: false, Reason: reason})
	if err := b.store.UpsertApproval(ctx, value); err != nil {
		return fmt.Errorf("approval: persist expiration: %w", err)
	}
	return nil
}

func validateRequest(request Request) error {
	if strings.TrimSpace(request.TaskID) == "" || request.ChatID == 0 ||
		strings.TrimSpace(request.ProviderRequestID) == "" || len(request.ProviderRequestID) > maxProviderID ||
		strings.TrimSpace(request.Kind) == "" || len(request.Kind) > maxKindBytes ||
		strings.TrimSpace(request.Summary) == "" || len(request.Summary) > maxSummary {
		return ErrInvalidRequest
	}
	return nil
}

func randomID() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(value[:])
}

func validUserID(value string) bool {
	id, err := strconv.ParseInt(value, 10, 64)
	return err == nil && id > 0
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit < 2 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}

type requestPayload struct {
	ProviderRequestID string `json:"provider_request_id"`
	ChatID            int64  `json:"chat_id"`
	Summary           string `json:"summary"`
}

type decisionPayload struct {
	Approved bool   `json:"approved"`
	UserID   string `json:"user_id,omitempty"`
	Reason   string `json:"reason"`
}
