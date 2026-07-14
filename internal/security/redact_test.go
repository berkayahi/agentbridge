package security

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestRedactorRedactsKnownCredentialShapes(t *testing.T) {
	r := NewRedactor(Config{})
	input := strings.Join([]string{
		"Authorization: Bearer top-secret-bearer",
		"X: 123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi",
		"classic=ghp_012345678901234567890123456789012345",
		"fine=github_pat_0123456789_ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"Cookie: session=secret-cookie",
		"Set-Cookie: session=secret-set-cookie; HttpOnly",
		"OPENAI_API_KEY=sk-secret",
		"ANTHROPIC_API_KEY='anthropic-secret'",
		"export ANTHROPIC_AUTH_TOKEN=auth-secret",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-secret",
		"OTHER_SECRET=generic-env-secret",
		"lowercase_secret=lowercase-env-secret",
		"-----BEGIN PRIVATE KEY-----\nvery-secret-key-material\n-----END PRIVATE KEY-----",
	}, "\n")

	got := r.RedactString(input)
	for _, secret := range []string{
		"top-secret-bearer", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi",
		"012345678901234567890123456789012345", "secret-cookie",
		"secret-set-cookie", "sk-secret", "anthropic-secret", "auth-secret",
		"oauth-secret", "generic-env-secret", "very-secret-key-material",
		"lowercase-env-secret",
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("output leaked %q: %s", secret, got)
		}
	}
	for _, label := range []string{
		"[REDACTED:authorization]", "[REDACTED:telegram-token]",
		"[REDACTED:github-token]", "[REDACTED:cookie]",
		"[REDACTED:set-cookie]", "[REDACTED:openai-api-key]",
		"[REDACTED:anthropic-api-key]", "[REDACTED:anthropic-auth-token]",
		"[REDACTED:claude-code-oauth-token]", "[REDACTED:env]",
		"[REDACTED:private-key]",
	} {
		if !strings.Contains(got, label) {
			t.Errorf("output missing %q: %s", label, got)
		}
	}
}

func TestRedactorConfiguredSecretsAreCopiedAndEmptyValuesIgnored(t *testing.T) {
	secrets := []string{"literal-secret", "", "longer-literal-secret"}
	r := NewRedactor(Config{Secrets: secrets})
	secrets[0] = "changed"

	got := r.RedactString("literal-secret longer-literal-secret benign")
	if strings.Contains(got, "literal-secret") {
		t.Fatalf("configured secret leaked: %s", got)
	}
	if got != "[REDACTED:configured] [REDACTED:configured] benign" {
		t.Fatalf("unexpected redaction: %q", got)
	}
}

func TestRedactorPreservesBenignTextAndCommitSHAs(t *testing.T) {
	const input = "commit 285b4bb47bbf983442fe5d36adc5491cf337db65 is healthy"
	if got := NewRedactor(Config{}).RedactString(input); got != input {
		t.Fatalf("benign text changed: %q", got)
	}
}

func TestRedactorPreservesJSONAndDoesNotMutateInput(t *testing.T) {
	r := NewRedactor(Config{Secrets: []string{"configured-value"}, MaxFieldRunes: 24, MaxPayloadRunes: 512})
	input := []byte(`{"message":"🔐 configured-value and a long suffix that must be cut","authorization":"Bearer hidden","nested":{"cookie":"session=hidden"}}`)
	original := append([]byte(nil), input...)

	got := r.RedactBytes(input)
	if !json.Valid(got) {
		t.Fatalf("redacted JSON is invalid: %s", got)
	}
	if string(input) != string(original) {
		t.Fatalf("input mutated: %s", input)
	}
	if strings.Contains(string(got), "configured-value") || strings.Contains(string(got), "Bearer hidden") || strings.Contains(string(got), "session=hidden") {
		t.Fatalf("JSON leaked a secret: %s", got)
	}
	var value map[string]any
	if err := json.Unmarshal(got, &value); err != nil {
		t.Fatal(err)
	}
	if value["authorization"] != "[REDACTED:authorization]" {
		t.Fatalf("authorization value = %#v", value["authorization"])
	}
}

func TestRedactorMalformedInputIsSafeRuneBoundedOpaqueOutput(t *testing.T) {
	r := NewRedactor(Config{Secrets: []string{"super-secret"}, MaxPayloadRunes: 18})
	input := []byte("🔐super-secret malformed {")
	got := r.RedactBytes(input)
	if !utf8.Valid(got) {
		t.Fatalf("output is not UTF-8: %x", got)
	}
	if utf8.RuneCount(got) > 18 {
		t.Fatalf("output has %d runes: %q", utf8.RuneCount(got), got)
	}
	if strings.Contains(string(got), "secret") {
		t.Fatalf("output leaked secret: %q", got)
	}
}

func TestRedactorRedactsBeforeTruncationAndIsConcurrent(t *testing.T) {
	r := NewRedactor(Config{Secrets: []string{"secret-at-boundary"}, MaxFieldRunes: 20, MaxPayloadRunes: 64})
	const input = `{"value":"prefix secret-at-boundary suffix"}`

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 100 {
				got := r.RedactBytes([]byte(input))
				if strings.Contains(string(got), "secret-at-boundary") || !utf8.Valid(got) {
					t.Errorf("unsafe output: %q", got)
					return
				}
			}
		})
	}
	wg.Wait()
}
