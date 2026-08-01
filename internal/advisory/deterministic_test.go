package advisory_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/berkayahi/agentbridge/internal/advisory"
)

func TestDeterministicProviderBuildsSchemaCompatibleJSON(t *testing.T) {
	provider := advisory.DeterministicProvider{ProviderID: "fixture", ModelID: "deterministic-v1"}
	service, err := advisory.New(advisory.Config{
		Catalog: testCatalog{profiles: []advisory.ProviderProfile{
			{ID: "fixture", ModelID: "deterministic-v1", Available: true, Capability: provider.Capability()},
		}},
		Providers: map[string]advisory.Provider{"fixture": provider},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := advisory.SessionRequest{
		ProviderID: "fixture", ModelID: "deterministic-v1", Prompt: "return the bounded fixture",
		OutputSchema:  json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"},"items":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}}},"required":["answer","items"],"additionalProperties":false}`),
		SchemaVersion: "fixture-schema-v1", IdempotencyKey: "fixture-request-1",
	}
	response, err := service.ExecuteAdvisorySession(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.Unmarshal(response.Output, &output); err != nil {
		t.Fatal(err)
	}
	if output["answer"] != "deterministic-fixture" {
		t.Fatalf("fixture output = %#v", output)
	}
	if _, ok := output["items"].([]any); !ok {
		t.Fatalf("fixture items = %#v", output["items"])
	}
}

func TestDeterministicProviderRejectsProseFixtureOutput(t *testing.T) {
	provider := advisory.DeterministicProvider{
		ProviderID: "fixture", ModelID: "deterministic-v1", Output: []byte("deterministic fixture prose"),
	}
	service, err := advisory.New(advisory.Config{
		Catalog: testCatalog{profiles: []advisory.ProviderProfile{
			{ID: "fixture", ModelID: "deterministic-v1", Available: true, Capability: provider.Capability()},
		}},
		Providers: map[string]advisory.Provider{"fixture": provider},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := advisory.SessionRequest{
		ProviderID: "fixture", ModelID: "deterministic-v1", Prompt: "return JSON",
		OutputSchema:  json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`),
		SchemaVersion: "fixture-schema-v1", IdempotencyKey: "fixture-request-2",
	}
	if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, advisory.ErrStructuredOutput) {
		t.Fatalf("prose output err = %v", err)
	}
}
