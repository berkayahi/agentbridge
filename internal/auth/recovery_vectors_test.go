package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryHPKEFixtureUsesReviewedProfile(t *testing.T) {
	path := filepath.Join("..", "..", "protocol", "fixtures", "v1", "recovery-hpke.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Profile string `json:"profile"`
		KEM     string `json:"kem"`
		KDF     string `json:"kdf"`
		AEAD    string `json:"aead"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Profile != "agentbridge-recovery-hpke-v1" || fixture.KEM != "DHKEM(X25519, HKDF-SHA256)" || fixture.KDF != "HKDF-SHA256" || fixture.AEAD != "ChaCha20Poly1305" {
		t.Fatalf("fixture profile = %#v", fixture)
	}
}
