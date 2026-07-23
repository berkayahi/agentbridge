package artifactclient

import (
	"context"
	"sync"
	"time"
)

type UploadStore interface {
	Begin(context.Context, EncryptedArtifact) error
	PutChunk(context.Context, Chunk) error
	Finalize(context.Context, string, string, time.Time) (Receipt, error)
}

type MemoryStore struct {
	mu      sync.Mutex
	objects map[string]*memoryObject
}

type memoryObject struct {
	value      EncryptedArtifact
	chunks     map[int64][]byte
	nextOffset int64
	final      bool
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{objects: make(map[string]*memoryObject)} }

func (s *MemoryStore) Begin(ctx context.Context, value EncryptedArtifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.objects[value.ObjectKey]; ok {
		if existing.value.EnvelopeDigest != value.EnvelopeDigest {
			return ErrConflict
		}
		return nil
	}
	s.objects[value.ObjectKey] = &memoryObject{value: value, chunks: make(map[int64][]byte)}
	return nil
}

func (s *MemoryStore) PutChunk(ctx context.Context, chunk Chunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if chunk.Offset < 0 || chunk.ObjectKey == "" || chunk.ArtifactID == "" || len(chunk.Payload) == 0 {
		return ErrChunkOrder
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.objects[chunk.ObjectKey]
	if !ok || value.value.ArtifactID != chunk.ArtifactID || value.final {
		return ErrChunkOrder
	}
	if existing, exists := value.chunks[chunk.Offset]; exists {
		if string(existing) != string(chunk.Payload) {
			return ErrConflict
		}
		return nil
	}
	if chunk.Offset != value.nextOffset {
		return ErrChunkOrder
	}
	value.chunks[chunk.Offset] = append([]byte(nil), chunk.Payload...)
	value.nextOffset += int64(len(chunk.Payload))
	if chunk.Final {
		value.final = true
	}
	return nil
}

func (s *MemoryStore) Finalize(ctx context.Context, objectKey, envelopeDigest string, now time.Time) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.objects[objectKey]
	if !ok {
		return Receipt{}, ErrChunkOrder
	}
	if value.value.EnvelopeDigest != envelopeDigest {
		return Receipt{}, ErrConflict
	}
	if !value.final {
		return Receipt{}, ErrChunkOrder
	}
	return Receipt{ArtifactID: value.value.ArtifactID, ObjectKey: objectKey, EnvelopeDigest: envelopeDigest, StoredBytes: int64(len(value.value.Ciphertext)), FinalizedAt: now}, nil
}

type Service struct {
	store    UploadStore
	verifier GrantVerifier
	now      func() time.Time
}

func NewService(store UploadStore, now func() time.Time) (*Service, error) {
	if store == nil {
		return nil, ErrChunkOrder
	}
	return newService(store, nil, now), nil
}

func NewServiceWithVerifier(store UploadStore, verifier GrantVerifier, now func() time.Time) (*Service, error) {
	if store == nil || verifier == nil {
		return nil, ErrGrantSignature
	}
	return newService(store, verifier, now), nil
}

func newService(store UploadStore, verifier GrantVerifier, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: store, verifier: verifier, now: now}
}

func (s *Service) Upload(ctx context.Context, grant Grant, key []byte, plaintext []byte) (Receipt, error) {
	if s == nil || s.store == nil {
		return Receipt{}, ErrChunkOrder
	}
	if s.verifier == nil {
		return Receipt{}, ErrGrantSignature
	}
	if err := grant.Verify(s.now().UTC(), s.verifier); err != nil {
		return Receipt{}, err
	}
	value, err := Encrypt(grant, key, plaintext, s.now().UTC())
	if err != nil {
		return Receipt{}, err
	}
	if err := s.store.Begin(ctx, value); err != nil {
		return Receipt{}, err
	}
	if err := s.store.PutChunk(ctx, Chunk{ArtifactID: value.ArtifactID, ObjectKey: value.ObjectKey, Payload: value.Ciphertext, Final: true}); err != nil {
		return Receipt{}, err
	}
	return s.store.Finalize(ctx, value.ObjectKey, value.EnvelopeDigest, s.now().UTC())
}
