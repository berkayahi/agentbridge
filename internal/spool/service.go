package spool

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Service is the policy boundary for durable device events. The backend owns
// the transaction so quota checks and sequence allocation cannot be separated
// from persistence.
type Service struct {
	store  Store
	config Config
	clock  func() time.Time
}

func New(config Config, store Store) (*Service, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	config = config.Normalize()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Service{store: store, config: config, clock: time.Now}, nil
}

func NewService(store Store, config Config) (*Service, error) {
	return New(config, store)
}

func (s *Service) Config() Config {
	if s == nil {
		return Config{}
	}
	return s.config
}

func (s *Service) Append(ctx context.Context, event Event) (AppendResult, error) {
	if s == nil || s.store == nil {
		return AppendResult{}, ErrInvalid
	}
	now := time.Now().UTC()
	if s.clock != nil {
		now = s.clock().UTC()
	}
	value, err := event.Normalize(now)
	if err != nil {
		return AppendResult{}, err
	}
	return s.store.Append(ctx, AppendRequest{Event: value, Config: s.config})
}

func (s *Service) Enqueue(ctx context.Context, event Event) (AppendResult, error) {
	return s.Append(ctx, event)
}

func (s *Service) Replay(ctx context.Context, afterMessageID uint64, limit int) ([]Message, error) {
	if s == nil || s.store == nil {
		return nil, ErrInvalid
	}
	return s.store.Replay(ctx, ReplayRequest{AfterMessageID: afterMessageID, Limit: limit})
}

func (s *Service) ReplayRequest(ctx context.Context, request ReplayRequest) ([]Message, error) {
	if s == nil || s.store == nil {
		return nil, ErrInvalid
	}
	return s.store.Replay(ctx, request)
}

func (s *Service) Acknowledge(ctx context.Context, request AcknowledgeRequest) (AcknowledgeResult, error) {
	if s == nil || s.store == nil {
		return AcknowledgeResult{}, ErrInvalid
	}
	if request.ReceiptID = strings.TrimSpace(request.ReceiptID); request.ReceiptID != "" && len(request.ReceiptID) > 256 {
		return AcknowledgeResult{}, ErrInvalid
	}
	return s.store.Acknowledge(ctx, request)
}

func (s *Service) Ack(ctx context.Context, request AcknowledgeRequest) (AcknowledgeResult, error) {
	return s.Acknowledge(ctx, request)
}

func (s *Service) RecordReceipt(ctx context.Context, receipt Receipt) (ReceiptResult, error) {
	if s == nil || s.store == nil {
		return ReceiptResult{}, ErrInvalid
	}
	if strings.TrimSpace(receipt.ID) == "" || receipt.MessageID == 0 {
		return ReceiptResult{}, ErrInvalid
	}
	return s.store.RecordReceipt(ctx, receipt)
}

func (s *Service) ReceiveReceipt(ctx context.Context, receipt Receipt) (ReceiptResult, error) {
	return s.RecordReceipt(ctx, receipt)
}

func (s *Service) Usage(ctx context.Context) (Usage, error) {
	if s == nil || s.store == nil {
		return Usage{}, ErrInvalid
	}
	value, err := s.store.Usage(ctx)
	if err != nil {
		return Usage{}, err
	}
	value.MaxBytes = s.config.MaxBytes
	value.WarningWatermark = s.config.WarningWatermarkBytes
	value.CriticalWatermark = s.config.CriticalWatermarkBytes
	return value, nil
}

func IsPause(err error) bool {
	return errors.Is(err, ErrSpoolPaused) || errors.Is(err, ErrCriticalReserve)
}
