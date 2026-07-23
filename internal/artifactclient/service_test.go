package artifactclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUploadRequiresVerifiedArtifactGrant(t *testing.T) {
	now := time.Unix(3_000, 0).UTC()
	payload := []byte("artifact payload")
	grant := Grant{
		OrganizationID: "org-1", DeviceID: "device-1", ExecutionID: "execution-1", ArtifactID: "artifact-1",
		ObjectKey: "objects/artifact-1", Algorithm: "AES-256-GCM", KeyID: "key-1", PolicyDigest: "policy-1",
		MediaType: "text/plain", SizeBytes: int64(len(payload)), PlaintextDigest: digestBytes(payload), ExpiresAt: now.Add(time.Minute), Nonce: "nonce-1", Signature: []byte("valid"),
	}
	service, err := NewServiceWithVerifier(NewMemoryStore(), GrantVerifierFunc(func(string, []byte, []byte) error { return errors.New("wrong signer") }), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Upload(context.Background(), grant, make([]byte, 32), payload); !errors.Is(err, ErrGrantSignature) {
		t.Fatalf("Upload() error = %v, want ErrGrantSignature", err)
	}
	service, err = NewServiceWithVerifier(NewMemoryStore(), GrantVerifierFunc(func(_ string, _ []byte, signature []byte) error {
		if string(signature) != "valid" {
			return errors.New("wrong signer")
		}
		return nil
	}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Upload(context.Background(), grant, make([]byte, 32), payload); err != nil {
		t.Fatalf("verified Upload() error = %v", err)
	}
}

func TestMemoryStoreRequiresCompleteCiphertextBeforeFinalize(t *testing.T) {
	now := time.Unix(3_000, 0).UTC()
	payload := []byte("artifact payload")
	grant := testGrant(now, payload)
	value, err := Encrypt(grant, make([]byte, 32), payload, now)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	if err := store.Begin(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	first := len(value.Ciphertext) / 2
	if err := store.PutChunk(context.Background(), Chunk{ArtifactID: value.ArtifactID, ObjectKey: value.ObjectKey, Payload: value.Ciphertext[:first]}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(context.Background(), value.ObjectKey, value.EnvelopeDigest, now); !errors.Is(err, ErrChunkOrder) {
		t.Fatalf("premature finalize error = %v, want ErrChunkOrder", err)
	}
	if err := store.PutChunk(context.Background(), Chunk{ArtifactID: value.ArtifactID, ObjectKey: value.ObjectKey, Offset: int64(first), Payload: value.Ciphertext[first:], Final: true}); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Finalize(context.Background(), value.ObjectKey, value.EnvelopeDigest, now)
	if err != nil || receipt.StoredBytes != int64(len(value.Ciphertext)) {
		t.Fatalf("finalize receipt = %#v err=%v", receipt, err)
	}
}

func TestUploadRejectsOneUseGrantReplay(t *testing.T) {
	now := time.Unix(3_000, 0).UTC()
	payload := []byte("artifact payload")
	grant := testGrant(now, payload)
	service, err := NewServiceWithVerifier(NewMemoryStore(), GrantVerifierFunc(func(string, []byte, []byte) error { return nil }), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Upload(context.Background(), grant, make([]byte, 32), payload); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Upload(context.Background(), grant, make([]byte, 32), payload); !errors.Is(err, ErrGrantReplay) {
		t.Fatalf("replayed upload error = %v, want ErrGrantReplay", err)
	}
}

func TestUploadRejectsOneUseGrantReplayAfterClientRestart(t *testing.T) {
	now := time.Unix(3_000, 0).UTC()
	payload := []byte("artifact payload")
	grant := testGrant(now, payload)
	store := NewMemoryStore()
	verifier := GrantVerifierFunc(func(string, []byte, []byte) error { return nil })
	first, err := NewServiceWithVerifier(store, verifier, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Upload(context.Background(), grant, make([]byte, 32), payload); err != nil {
		t.Fatal(err)
	}
	second, err := NewServiceWithVerifier(store, verifier, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Upload(context.Background(), grant, make([]byte, 32), payload); !errors.Is(err, ErrGrantReplay) {
		t.Fatalf("replayed upload after restart error = %v, want ErrGrantReplay", err)
	}
}

func TestGrantRejectsPathTraversalObjectKey(t *testing.T) {
	now := time.Unix(3_000, 0).UTC()
	grant := testGrant(now, []byte("payload"))
	grant.ObjectKey = "objects/../escape"
	if err := grant.Validate(now); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("path traversal error = %v, want ErrInvalidGrant", err)
	}
}

func testGrant(now time.Time, payload []byte) Grant {
	return Grant{
		OrganizationID: "org-1", DeviceID: "device-1", ExecutionID: "execution-1", ArtifactID: "artifact-1",
		ObjectKey: "objects/artifact-1", Algorithm: "AES-256-GCM", KeyID: "key-1", PolicyDigest: "policy-1",
		MediaType: "text/plain", SizeBytes: int64(len(payload)), PlaintextDigest: digestBytes(payload), ExpiresAt: now.Add(time.Minute), Nonce: "nonce-1", Signature: []byte("valid"),
	}
}

type GrantVerifierFunc func(string, []byte, []byte) error

func (f GrantVerifierFunc) Verify(keyID string, message, signature []byte) error {
	return f(keyID, message, signature)
}
