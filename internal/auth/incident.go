package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

type durableIncidentRepository interface {
	UpsertAuthIncident(context.Context, workmodel.AuthIncident) error
	OpenAuthIncident(context.Context, workmodel.Provider) (workmodel.AuthIncident, error)
}

// DurableIncidentStore maps auth incidents onto the daemon's SQLite store.
// Only classification state and timestamps are serialized.
type DurableIncidentStore struct {
	repository durableIncidentRepository
}

func NewDurableIncidentStore(repository durableIncidentRepository) *DurableIncidentStore {
	return &DurableIncidentStore{repository: repository}
}

func (s *DurableIncidentStore) SaveIncident(ctx context.Context, value Incident) error {
	detail, err := json.Marshal(struct {
		Kind HealthKind `json:"kind"`
	}{value.Kind})
	if err != nil {
		return fmt.Errorf("encode auth incident: %w", err)
	}
	return s.repository.UpsertAuthIncident(ctx, workmodel.AuthIncident{
		ID: value.ID, Provider: value.Provider, Status: string(value.Status), Detail: detail,
		DetectedAt: value.OpenedAt, ResolvedAt: value.ResolvedAt,
	})
}

func (s *DurableIncidentStore) OpenIncident(ctx context.Context, provider workmodel.Provider) (Incident, error) {
	value, err := s.repository.OpenAuthIncident(ctx, provider)
	if errors.Is(err, store.ErrNotFound) {
		return Incident{}, ErrIncidentNotFound
	}
	if err != nil {
		return Incident{}, err
	}
	var detail struct {
		Kind HealthKind `json:"kind"`
	}
	if err := json.Unmarshal(value.Detail, &detail); err != nil {
		return Incident{}, fmt.Errorf("decode auth incident: %w", err)
	}
	return Incident{
		ID: value.ID, Provider: value.Provider, Kind: detail.Kind,
		Status: IncidentStatus(value.Status), OpenedAt: value.DetectedAt, ResolvedAt: value.ResolvedAt,
	}, nil
}
