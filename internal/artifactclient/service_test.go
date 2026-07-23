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

type GrantVerifierFunc func(string, []byte, []byte) error

func (f GrantVerifierFunc) Verify(keyID string, message, signature []byte) error {
	return f(keyID, message, signature)
}
