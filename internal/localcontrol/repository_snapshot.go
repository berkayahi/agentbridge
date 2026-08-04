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
var _ RepositoryKnowledgeAuthority = (*repositorysnapshot.Service)(nil)

func (s *Service) ReadRepositoryKnowledge(ctx context.Context, request RepositoryKnowledgeRequest) (RepositoryKnowledgeResponse, error) {
	if s == nil || s.knowledge == nil {
		return RepositoryKnowledgeResponse{}, ErrNotConfigured
	}
	return s.knowledge.ReadKnowledge(ctx, request)
}
