package artifactclient

import "context"

func (s *Service) Reconcile(ctx context.Context, objectKey, envelopeDigest string) (Receipt, error) {
	if s == nil || s.store == nil {
		return Receipt{}, ErrChunkOrder
	}
	return s.store.Finalize(ctx, objectKey, envelopeDigest, s.now().UTC())
}
