package localcontrol

import (
	"context"
	"strings"

	"github.com/berkayahi/agentbridge/internal/executioncontract"
)

type ExecutionResultRequest struct {
	ExpectedRevision int64                             `json:"expected_revision"`
	Result           executioncontract.ExecutionResult `json:"result"`
}

type ResourceLeaseHeartbeatRequest struct {
	OwnerExecutionID string `json:"owner_execution_id"`
	TTLSeconds       int    `json:"ttl_seconds"`
}

type ResourceLeaseReleaseRequest struct {
	OwnerExecutionID string `json:"owner_execution_id"`
}

func (s *Service) executionStore() (executioncontract.Store, error) {
	if s == nil || s.executions == nil {
		return nil, ErrNotConfigured
	}
	return s.executions, nil
}

func (s *Service) CreateExecution(ctx context.Context, request executioncontract.ExecutionRequest) (executioncontract.ExecutionRecord, error) {
	if err := executioncontract.ValidateRequest(request); err != nil {
		return executioncontract.ExecutionRecord{}, err
	}
	store, err := s.executionStore()
	if err != nil {
		return executioncontract.ExecutionRecord{}, err
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	return store.CreateExecution(ctx, request, s.clock())
}

func (s *Service) GetExecution(ctx context.Context, executionID string) (executioncontract.ExecutionRecord, error) {
	if strings.TrimSpace(executionID) == "" {
		return executioncontract.ExecutionRecord{}, ErrInvalidRequest
	}
	store, err := s.executionStore()
	if err != nil {
		return executioncontract.ExecutionRecord{}, err
	}
	return store.GetExecution(ctx, executionID)
}

func (s *Service) SaveExecutionResult(ctx context.Context, executionID string, request ExecutionResultRequest) (executioncontract.ExecutionRecord, error) {
	if strings.TrimSpace(executionID) == "" || request.ExpectedRevision < 1 {
		return executioncontract.ExecutionRecord{}, ErrInvalidRequest
	}
	store, err := s.executionStore()
	if err != nil {
		return executioncontract.ExecutionRecord{}, err
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	return store.SaveExecutionResult(ctx, executionID, request.ExpectedRevision, request.Result, s.clock())
}

func (s *Service) RecoverExecutions(ctx context.Context) ([]executioncontract.ExecutionRecord, error) {
	store, err := s.executionStore()
	if err != nil {
		return nil, err
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	return store.RecoverExecutions(ctx, s.clock())
}

func (s *Service) AcquireResourceLease(ctx context.Context, request executioncontract.ResourceLeaseRequest) (executioncontract.ResourceLease, error) {
	if strings.TrimSpace(request.LeaseID) == "" {
		request.LeaseID = s.newID("lease")
	}
	store, err := s.executionStore()
	if err != nil {
		return executioncontract.ResourceLease{}, err
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	return store.AcquireResourceLease(ctx, request, s.clock())
}

func (s *Service) HeartbeatResourceLease(ctx context.Context, leaseID string, request ResourceLeaseHeartbeatRequest) (executioncontract.ResourceLease, error) {
	if strings.TrimSpace(leaseID) == "" || strings.TrimSpace(request.OwnerExecutionID) == "" || request.TTLSeconds < 1 {
		return executioncontract.ResourceLease{}, ErrInvalidRequest
	}
	store, err := s.executionStore()
	if err != nil {
		return executioncontract.ResourceLease{}, err
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	return store.HeartbeatResourceLease(ctx, leaseID, request.OwnerExecutionID, request.TTLSeconds, s.clock())
}

func (s *Service) ReleaseResourceLease(ctx context.Context, leaseID string, request ResourceLeaseReleaseRequest) error {
	if strings.TrimSpace(leaseID) == "" || strings.TrimSpace(request.OwnerExecutionID) == "" {
		return ErrInvalidRequest
	}
	store, err := s.executionStore()
	if err != nil {
		return err
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	return store.ReleaseResourceLease(ctx, leaseID, request.OwnerExecutionID, s.clock())
}

func (s *Service) ExpiredResourceLeases(ctx context.Context) ([]executioncontract.ResourceLease, error) {
	store, err := s.executionStore()
	if err != nil {
		return nil, err
	}
	return store.ExpiredResourceLeases(ctx, s.clock())
}
