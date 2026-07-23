package terminalupload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/artifactclient"
	"github.com/berkayahi/agentbridge/internal/egressguard"
)

func TestTerminalUploadIsDisabledWithoutExplicitPolicy(t *testing.T) {
	uploader := &recordingUploader{}
	service, err := NewService(uploader, egressguard.New(egressguard.Config{}), func() time.Time { return time.Unix(1_000, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Upload(context.Background(), Request{Payload: []byte("output")})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Upload() error = %v, want ErrDisabled", err)
	}
	if uploader.called {
		t.Fatal("disabled terminal upload reached artifact service")
	}
}

func TestTerminalUploadRequiresQuotaExpiryAndEgressClearance(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	payload := []byte("safe output")
	grant := artifactclient.Grant{SizeBytes: int64(len(payload)), PlaintextDigest: digest(payload)}
	service, err := NewService(&recordingUploader{}, egressguard.New(egressguard.Config{Secrets: []string{"credential"}}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Upload(context.Background(), Request{Policy: Policy{Enabled: true, MaxBytes: int64(len(payload)), ExpiresAt: now.Add(time.Minute)}, Grant: grant, Payload: payload}); err == nil {
		t.Fatal("invalid artifact grant reached upload")
	}
	secret := append([]byte("api_key="), []byte("value")...)
	grant.OrganizationID, grant.DeviceID, grant.ExecutionID, grant.ArtifactID = "org-1", "device-1", "execution-1", "artifact-1"
	grant.ObjectKey, grant.Algorithm, grant.KeyID, grant.PolicyDigest = "objects/artifact-1", "AES-256-GCM", "key-1", "policy-1"
	grant.MediaType, grant.ExpiresAt, grant.Nonce = "text/plain", now.Add(time.Minute), "nonce-1"
	grant.SizeBytes, grant.PlaintextDigest = int64(len(secret)), digest(secret)
	if _, err := service.Upload(context.Background(), Request{Policy: Policy{Enabled: true, MaxBytes: int64(len(secret)), ExpiresAt: now.Add(time.Minute)}, Grant: grant, Payload: secret}); !errors.Is(err, egressguard.ErrQuarantined) {
		t.Fatalf("secret upload error = %v, want ErrQuarantined", err)
	}
}

type recordingUploader struct{ called bool }

func (u *recordingUploader) Upload(context.Context, artifactclient.Grant, []byte, []byte) (artifactclient.Receipt, error) {
	u.called = true
	return artifactclient.Receipt{}, nil
}

func digest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
