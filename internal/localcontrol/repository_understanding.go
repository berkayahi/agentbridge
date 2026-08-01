package localcontrol

import (
	"context"

	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
)

func (s *Service) UnderstandRepository(ctx context.Context, request RepositoryUnderstandingRequest) (RepositoryUnderstandingResponse, error) {
	if s == nil || s.understanding == nil {
		return RepositoryUnderstandingResponse{}, ErrNotConfigured
	}
	return s.understanding.Understand(ctx, request)
}

var _ RepositoryUnderstandingAuthority = (*repositorysnapshot.UnderstandingService)(nil)
