package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/events"
	"github.com/berkayahi/agentbridge/internal/execution"
	"github.com/berkayahi/agentbridge/internal/intent"
	"github.com/berkayahi/agentbridge/internal/store"
)

var ErrInvalidCommand = errors.New("kernel: invalid command")

type Config struct {
	Work      store.UnitOfWork
	Clock     func() time.Time
	IntentTTL time.Duration
	Owner     string
}

type Kernel struct {
	work      store.UnitOfWork
	clock     func() time.Time
	intentTTL time.Duration
	owner     string
}

func New(cfg Config) (*Kernel, error) {
	if cfg.Work == nil || strings.TrimSpace(cfg.Owner) == "" {
		return nil, ErrInvalidCommand
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.IntentTTL <= 0 {
		cfg.IntentTTL = 24 * time.Hour
	}
	return &Kernel{work: cfg.Work, clock: cfg.Clock, intentTTL: cfg.IntentTTL, owner: cfg.Owner}, nil
}

func (k *Kernel) Start(ctx context.Context, command StartExecution) error {
	if err := validateStart(command); err != nil {
		return err
	}
	now := k.clock().UTC()
	expires := command.ExpiresAt
	if expires.IsZero() {
		expires = now.Add(k.intentTTL)
	}
	return k.accept(ctx, command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID, "start", expires,
		func(repos store.Repositories) error {
			value := newIntent(command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID, "start", now, expires)
			value.PayloadRef = commandPayload("start", command.CommandID, command.SessionID, command.RepositoryID, command.RuntimeID, command.Model, command.Input.Text, string(command.PolicySnapshot))
			return repos.Intents.Create(ctx, value)
		})
}

func (k *Kernel) Resume(ctx context.Context, command ResumeExecution) error {
	if command.Input.Validate() != nil || !validCommandIDs(command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID) {
		return ErrInvalidCommand
	}
	return k.acceptSimple(ctx, command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID, "resume", command.Input.Text)
}

func (k *Kernel) Steer(ctx context.Context, command SteerExecution) error {
	if command.Input.Validate() != nil || !validCommandIDs(command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID) {
		return ErrInvalidCommand
	}
	return k.acceptSimple(ctx, command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID, "steer", command.Input.Text)
}

func (k *Kernel) Cancel(ctx context.Context, command CancelExecution) error {
	if !validCommandIDs(command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID) {
		return ErrInvalidCommand
	}
	now := k.clock().UTC()
	return k.work.Within(ctx, func(repos store.Repositories) error {
		count, err := repos.Intents.CancelByExecution(ctx, command.ExecutionID, "canceled by operator command", now)
		if err != nil {
			return err
		}
		if count == 0 {
			value := newIntent(command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID, "cancel", now, now.Add(k.intentTTL))
			if err := repos.Intents.Create(ctx, value); err != nil {
				return err
			}
		}
		return repos.Events.Append(ctx, durableEvent(command.CommandID, command.ExecutionID, EventCancellationFenced, "canceled by operator command", now))
	})
}

func (k *Kernel) Close(ctx context.Context, command CloseExecution) error {
	if !validCommandIDs(command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID) {
		return ErrInvalidCommand
	}
	return k.acceptSimple(ctx, command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID, "close")
}

func (k *Kernel) Fork(ctx context.Context, command ForkExecution) error {
	if command.Input.Validate() != nil || !validCommandIDs(command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID, command.SuccessorTaskID) {
		return ErrInvalidCommand
	}
	return k.acceptSimple(ctx, command.CommandID, command.ExecutionID, command.TaskID, command.RuntimeID, "fork")
}

// Retry applies only the domain's evidence-bound, fenced in-place retry.
// Ambiguous provider outcomes never reach this method without a reconciled
// evidence identifier supplied by the caller.
func (k *Kernel) Retry(ctx context.Context, command RetryExecution) error {
	if !validCommandIDs(command.ExecutionID, command.CommandID, command.EvidenceID) || !command.Basis.Valid() || command.FencingEpoch == 0 || command.IssuedAt.IsZero() {
		return ErrInvalidCommand
	}
	now := k.clock().UTC()
	return k.work.Within(ctx, func(repos store.Repositories) error {
		value, err := repos.Executions.Get(ctx, command.ExecutionID)
		if err != nil {
			return err
		}
		updated, err := value.TransientRetry(execution.TransientRetryCommand{ID: command.CommandID, EvidenceID: command.EvidenceID, Basis: command.Basis, FencingEpoch: command.FencingEpoch, IssuedAt: command.IssuedAt})
		if err != nil {
			return err
		}
		if err := repos.Executions.Save(ctx, updated); err != nil {
			return err
		}
		return repos.Events.Append(ctx, durableEvent(command.CommandID, command.ExecutionID, EventIntentAccepted, "transient retry accepted", now))
	})
}

// Successor creates explicit workflow retry lineage after terminal failure.
func (k *Kernel) Successor(ctx context.Context, command SuccessorExecution) error {
	if !validCommandIDs(command.ExecutionID, command.NewID, command.CommandID) || command.FencingEpoch == 0 || command.CreatedAt.IsZero() {
		return ErrInvalidCommand
	}
	return k.work.Within(ctx, func(repos store.Repositories) error {
		value, err := repos.Executions.Get(ctx, command.ExecutionID)
		if err != nil {
			return err
		}
		successor, err := value.Successor(command.NewID, command.CommandID, command.FencingEpoch, command.CreatedAt)
		if err != nil {
			return err
		}
		if err := repos.Executions.Create(ctx, successor); err != nil {
			return err
		}
		return repos.Events.Append(ctx, durableEvent(command.CommandID, command.NewID, EventIntentAccepted, "successor execution created", command.CreatedAt))
	})
}

func (k *Kernel) acceptSimple(ctx context.Context, commandID, executionID, taskID, runtimeID, kind string, payload ...string) error {
	now := k.clock().UTC()
	return k.accept(ctx, commandID, executionID, taskID, runtimeID, kind, now.Add(k.intentTTL), func(repos store.Repositories) error {
		value := newIntent(commandID, executionID, taskID, runtimeID, kind, now, now.Add(k.intentTTL))
		value.PayloadRef = commandPayload(append([]string{kind, commandID, executionID, taskID, runtimeID}, payload...)...)
		return repos.Intents.Create(ctx, value)
	})
}

func (k *Kernel) accept(ctx context.Context, commandID, executionID, taskID, runtimeID, kind string, expires time.Time, extra func(store.Repositories) error) error {
	now := k.clock().UTC()
	value := newIntent(commandID, executionID, taskID, runtimeID, kind, now, expires)
	return k.work.Within(ctx, func(repos store.Repositories) error {
		if extra != nil {
			if err := extra(repos); err != nil {
				return err
			}
		} else if err := repos.Intents.Create(ctx, value); err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(commandID + ":" + kind))
		return repos.Events.Append(ctx, durableEvent(commandID, executionID, EventIntentAccepted, hex.EncodeToString(digest[:]), now))
	})
}

func commandPayload(values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(digest[:])
}

func newIntent(id, executionID, taskID, runtimeID, kind string, now, expires time.Time) intent.Intent {
	return intent.Intent{ID: id, ExecutionID: executionID, Kind: kind, RuntimeID: runtimeID, TargetTaskID: taskID, State: intent.StatePending, CreatedAt: now, ExpiresAt: expires}
}

func durableEvent(id, executionID string, kind EventType, message string, now time.Time) events.Record {
	return events.Record{ID: id + "-event", ExecutionID: executionID, Type: string(kind), Visibility: "internal", Payload: []byte(message), CreatedAt: now}
}

func validateStart(command StartExecution) error {
	if command.Input.Validate() != nil || command.FencingEpoch == 0 || len(command.PolicySnapshot) == 0 ||
		!validCommandIDs(command.CommandID, command.ExecutionID, command.TaskID, command.SessionID, command.RepositoryID, command.RuntimeID) {
		return ErrInvalidCommand
	}
	return nil
}

func validCommandIDs(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > 128 {
			return false
		}
	}
	return true
}

// Handler is the transport-neutral command contract used by controllers.
type Handler[C any] interface {
	Handle(context.Context, C) error
}
