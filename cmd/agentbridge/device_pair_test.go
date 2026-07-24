package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

func TestDevicePairCommandEmitsProofAndPinsControllerKey(t *testing.T) {
	ctx := context.Background()
	controller, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	challenge := localcontrol.PairingChallenge{
		ID: "challenge-pi", DeviceID: "build-pi", BrowserFingerprint: "desktop-browser",
		Nonce: "nonce-pi", TrustSetDigest: "local-device-routing-v1", ControllerPublicKey: controller.PublicKey(),
		ExpiresAt: now.Add(10 * time.Minute), CreatedAt: now,
	}
	challengePath := filepath.Join(t.TempDir(), "challenge.json")
	encoded, err := json.Marshal(challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(challengePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "agentbridge")
	var stdout, stderr bytes.Buffer
	if code := runDevicePairCommand(ctx, []string{
		"--challenge", challengePath, "--data-dir", dataDir, "--name", "Build Pi",
		"--endpoint", "wss://build-pi.tailnet/agentbridge",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var result devicePairProofOutput
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.DeviceID != challenge.DeviceID || result.ChallengeID != challenge.ID || result.Fingerprint == "" || len(result.PublicKey) != 32 || len(result.Signature) != 64 || result.IdempotencyKey == "" {
		t.Fatalf("pair result = %#v", result)
	}
	claim := deviceidentity.Claim{ID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID, BrowserFingerprint: challenge.BrowserFingerprint, ExpiresAt: challenge.ExpiresAt}
	proofChallenge := deviceidentity.Challenge{ClaimID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID, Nonce: challenge.Nonce, TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt}
	proof := deviceidentity.Proof{ClaimID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID, Nonce: challenge.Nonce, PublicKey: result.PublicKey, Signature: result.Signature, TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt}
	if err := deviceidentity.VerifyProof(claim, proofChallenge, proof, time.Now().UTC()); err != nil {
		t.Fatalf("verify emitted proof: %v", err)
	}
	controllerPath := filepath.Join(dataDir, "controller-public-key.bin")
	pinned, err := os.ReadFile(controllerPath)
	if err != nil || string(pinned) != string(controller.PublicKey()) {
		t.Fatalf("pinned controller key = %x err=%v", pinned, err)
	}
	if info, err := os.Stat(filepath.Join(dataDir, "device-key.json")); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("device identity permissions = %v err=%v", info.Mode().Perm(), err)
	}
}

func TestLoadLocalPairingChallengeRejectsTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "challenge.json")
	if err := os.WriteFile(path, []byte(`{"id":"challenge","device_id":"pi","browser_fingerprint":"browser","nonce":"nonce","trust_set_digest":"digest","expires_at":"2030-01-01T00:00:00Z","created_at":"2029-01-01T00:00:00Z"} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLocalPairingChallenge(path); err == nil {
		t.Fatal("trailing JSON pairing challenge was accepted")
	}
}
