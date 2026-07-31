package localcontrol

import (
	"context"

	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
)

func (s *Service) CreateRepositorySnapshot(ctx context.Context, request RepositorySnapshotRequest) (RepositorySnapshotResponse, error) {
	if s == nil || s.snapshots == nil {
		return RepositorySnapshotResponse{}, ErrNotConfigured
	}
	response, err := s.snapshots.Snapshot(ctx, request)
	if err != nil {
		return RepositorySnapshotResponse{}, err
	}
	return response, nil
}

var _ RepositorySnapshotAuthority = (*repositorysnapshot.Service)(nil)
