package advisory_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/berkayahi/agentbridge/internal/advisory"
	"github.com/berkayahi/agentbridge/internal/security"
)

func TestSafetyPipelineCoversEncodingChunkAndTruncationBoundaries(t *testing.T) {
	const secret = "configured-secret-ß"
	redactor := security.NewRedactor(security.Config{Secrets: []string{secret}})
	pipeline := advisory.NewSafetyPipeline(redactor, advisory.SafetyConfig{MaxInputBytes: advisory.MaxContextTotalBytes})
	encoded, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "raw", payload: []byte(`{"message":"configured-secret-ß"}`)},
		{name: "escaped", payload: []byte(`{"message":"configured-secret-\u00df"}`)},
		{name: "once encoded", payload: encoded},
	} {
		t.Run(test.name, func(t *testing.T) {
			safe, err := pipeline.SanitizeJSON(test.payload)
			if err != nil || strings.Contains(string(safe), secret) {
				t.Fatalf("safe output err=%v contains_original=%v", err, strings.Contains(string(safe), secret))
			}
		})
	}
	twice, err := json.Marshal(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if safe, err := pipeline.SanitizeText(string(twice), 0); err != nil || strings.Contains(safe, secret) {
		t.Fatalf("twice encoded text err=%v contains_original=%v", err, strings.Contains(safe, secret))
	}

	guard := pipeline.NewStream()
	knownToken := "ghp_123456789012345678901234"
	if err := guard.Feed([]byte(`{"message":"` + knownToken[:8])); err != nil {
		t.Fatal(err)
	}
	if err := guard.Feed([]byte(knownToken[8:] + `"}`)); !errors.Is(err, advisory.ErrUnsafeContent) {
		t.Fatalf("split token err=%v", err)
	}

	lateSecret := strings.Repeat("x", 10000) + " password=bounded-secret-value"
	if _, err := advisory.NewSafetyPipeline(nil, advisory.SafetyConfig{}).SanitizeText(lateSecret, 32); !errors.Is(err, advisory.ErrUnsafeContent) {
		t.Fatalf("late secret err=%v", err)
	}
	if _, err := pipeline.RejectJSON([]byte(`{"token_budget":128,"message":"ordinary JSON"}`)); err != nil {
		t.Fatalf("ordinary JSON was rejected: %v", err)
	}
	if _, err := pipeline.RejectJSON([]byte(`{"message":`)); !errors.Is(err, advisory.ErrMalformedPayload) {
		t.Fatalf("malformed JSON err=%v", err)
	}
}

func TestAdvisoryServiceGuardsProviderDiagnosticsAndErrors(t *testing.T) {
	const secret = "ghp_123456789012345678901234"
	provider := newTestProvider(`{"answer":"ok"}`)
	provider.result.Stdout = []byte("provider stdout " + secret)
	provider.result.Stderr = []byte("provider stderr " + secret)
	service := newService(t, provider)
	response, err := service.ExecuteAdvisorySession(context.Background(), testRequest())
	if err != nil || strings.Contains(string(response.Output), secret) {
		t.Fatalf("diagnostic response=%#v err=%v contains_original=%v", response, err, strings.Contains(string(response.Output), secret))
	}

	provider = newTestProvider(`{"answer":"ok"}`)
	provider.err = errors.New("provider failed password=bounded-secret-value")
	service = newService(t, provider)
	response, err = service.ExecuteAdvisorySession(context.Background(), testRequest())
	if !errors.Is(err, advisory.ErrProviderExecution) || len(response.Output) != 0 || strings.Contains(err.Error(), "bounded-secret-value") {
		t.Fatalf("provider error response=%#v err=%v", response, err)
	}
}
