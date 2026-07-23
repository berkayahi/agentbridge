package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
)

func TestRecoveryTranscriptBindsDeviceAndContext(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	key, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	transcript := recoveryTranscriptFixture(key, now)
	signed, err := transcript.Sign(key, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRecoveryTranscript(signed, key.PublicKey(), now); err != nil {
		t.Fatal(err)
	}
	if err := signed.VerifyKeyConfirmation(signed.ConfirmationDigest()); err != nil {
		t.Fatal(err)
	}

	tampered := signed
	tampered.Provider = "claude"
	if err := VerifyRecoveryTranscript(tampered, key.PublicKey(), now); !errors.Is(err, ErrInvalidRecoveryTranscript) {
		t.Fatalf("tampered provider error = %v", err)
	}

	other, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRecoveryTranscript(signed, other.PublicKey(), now); !errors.Is(err, ErrInvalidRecoveryTranscript) {
		t.Fatalf("substituted device error = %v", err)
	}
	if err := signed.VerifyKeyConfirmation("00"); !errors.Is(err, ErrInvalidRecoveryTranscript) {
		t.Fatalf("wrong confirmation error = %v", err)
	}
}

func TestRecoveryTranscriptClaimIsAtomicAndOneUse(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	key, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := recoveryTranscriptFixture(key, now).Sign(key, now)
	if err != nil {
		t.Fatal(err)
	}
	claims := NewMemoryRecoveryClaimStore()
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- VerifyRecoveryTranscriptOnce(context.Background(), claims, signed, key.PublicKey(), now)
		}()
	}
	group.Wait()
	close(results)

	var successes, replays int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrRecoveryTranscriptReplay):
			replays++
		default:
			t.Fatalf("claim error = %v", err)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("successes = %d, replays = %d, want one of each", successes, replays)
	}
}

func recoveryTranscriptFixture(key deviceidentity.Key, now time.Time) RecoveryTranscript {
	return RecoveryTranscript{
		RequestID:          "recovery-request-1",
		OrganizationID:     "org-1",
		DeviceID:           "device-1",
		Provider:           "codex",
		BrowserSessionID:   "browser-session-1",
		EphemeralPublicKey: []byte("01234567890123456789012345678901"),
		Challenge:          []byte("challenge-value-1234"),
		KeyConfirmation:    []byte("key-confirmation-1234"),
		ExpiresAt:          now.Add(5 * time.Minute),
		DeviceFingerprint:  deviceidentity.EnrollmentFingerprint(key.PublicKey()),
	}
}
