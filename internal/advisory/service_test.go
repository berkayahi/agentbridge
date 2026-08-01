package advisory_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/advisory"
)

const testSchema = `{"type":"object","properties":{"answer":{"type":"string"},"note":{"type":"string"}},"required":["answer"],"additionalProperties":false}`

type testCatalog struct {
	profiles []advisory.ProviderProfile
}

func (c testCatalog) ProviderProfiles(context.Context) ([]advisory.ProviderProfile, error) {
	return c.profiles, nil
}

type testProvider struct {
	capability advisory.ProviderCapability
	result     advisory.ExecutionResult
	request    advisory.ExecutionRequest
	calls      int
}

func (p *testProvider) Capability() advisory.ProviderCapability { return p.capability }

func (p *testProvider) Execute(_ context.Context, request advisory.ExecutionRequest) (advisory.ExecutionResult, error) {
	p.calls++
	p.request = request
	return p.result, nil
}

func newTestProvider(output string) *testProvider {
	return &testProvider{
		capability: advisory.ProviderCapability{ID: "provider-a", AdvisorySessions: true, ReadOnly: true, StructuredOutput: true},
		result:     advisory.ExecutionResult{ProviderID: "provider-a", ModelID: "model-a", Output: []byte(output)},
	}
}

func newService(t *testing.T, provider *testProvider) *advisory.Service {
	t.Helper()
	service, err := advisory.New(advisory.Config{
		Catalog: testCatalog{profiles: []advisory.ProviderProfile{{
			ID: "provider-a", ModelID: "model-a", Available: true, Capability: provider.Capability(),
		}}},
		Providers: map[string]advisory.Provider{"provider-a": provider},
		Clock:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		NewID:     func(prefix string) string { return prefix + "-1" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testRequest() advisory.SessionRequest {
	return advisory.SessionRequest{
		ProviderID: "provider-a", ModelID: "model-a", Prompt: "summarize the bounded input",
		Context:      advisory.ContextBundle{Items: []advisory.ContextItem{{Key: "source", Value: "bounded text"}}},
		OutputSchema: json.RawMessage(testSchema), SchemaVersion: "schema-1", IdempotencyKey: "request-1",
	}
}

func TestExecuteAdvisorySessionEnforcesReadOnlyPolicyAndReceiptProvenance(t *testing.T) {
	provider := newTestProvider(`{"answer":"ok"}`)
	service := newService(t, provider)
	response, err := service.ExecuteAdvisorySession(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || provider.request.Policy.ReadOnly != true || provider.request.Policy.RepositoryWrites || provider.request.Policy.BranchMutation ||
		provider.request.Policy.WorktreeMutation || provider.request.Policy.GitIntegration || provider.request.Policy.SecretValueAccess ||
		provider.request.Policy.DecisionMutation || provider.request.Policy.HumanApproval || provider.request.Policy.WebResearchAllowed {
		t.Fatalf("provider request policy = %#v", provider.request.Policy)
	}
	if response.Receipt.ProviderID != "provider-a" || response.Receipt.ModelID != "model-a" || response.Receipt.SchemaVersion != "schema-1" ||
		response.Receipt.ExecutionSessionID == "" || response.Receipt.ReceiptID == "" || response.Receipt.Status != "completed" {
		t.Fatalf("receipt = %#v", response.Receipt)
	}
	if response.Receipt.ContextDigest == "" || response.Receipt.PromptDigest == "" || response.Receipt.OutputDigest == "" || response.Receipt.StartedAt.IsZero() || response.Receipt.CompletedAt.IsZero() {
		t.Fatalf("receipt provenance = %#v", response.Receipt)
	}
}

func TestExecuteAdvisorySessionRedactsSecretsBeforeProviderAndResponse(t *testing.T) {
	provider := newTestProvider(`{"answer":"ok","note":"ghp_123456789012345678901234"}`)
	service := newService(t, provider)
	request := testRequest()
	request.Context.Items[0].Value = "Authorization: Bearer ghp_123456789012345678901234"
	response, err := service.ExecuteAdvisorySession(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(provider.request.Prompt, "ghp_123456789012345678901234") || strings.Contains(provider.request.Context.Items[0].Value, "ghp_123456789012345678901234") {
		t.Fatalf("secret reached provider request: %#v", provider.request)
	}
	if strings.Contains(string(response.Output), "ghp_123456789012345678901234") {
		t.Fatalf("secret reached response: %s", response.Output)
	}
}

func TestExecuteAdvisorySessionFailsClosedForMalformedStructuredOutput(t *testing.T) {
	service := newService(t, newTestProvider(`{"answer":`))
	_, err := service.ExecuteAdvisorySession(context.Background(), testRequest())
	if !errors.Is(err, advisory.ErrStructuredOutput) {
		t.Fatalf("err = %v, want ErrStructuredOutput", err)
	}
}

func TestExecuteAdvisorySessionRejectsSecretNamedContextAndIneligibleProvider(t *testing.T) {
	provider := newTestProvider(`{"answer":"ok"}`)
	service := newService(t, provider)
	request := testRequest()
	request.Context.Items[0].Key = "api_key"
	if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, advisory.ErrPolicyViolation) {
		t.Fatalf("secret context err = %v", err)
	}
	provider.capability.RepositoryWrites = true
	if _, err := service.ExecuteAdvisorySession(context.Background(), testRequest()); !errors.Is(err, advisory.ErrNotConfigured) {
		t.Fatalf("ineligible provider err = %v", err)
	}
}

func TestExecuteAdvisorySessionRequiresStrictSchema(t *testing.T) {
	provider := newTestProvider(`{"answer":"ok","extra":true}`)
	service := newService(t, provider)
	if _, err := service.ExecuteAdvisorySession(context.Background(), testRequest()); !errors.Is(err, advisory.ErrStructuredOutput) {
		t.Fatalf("additional output err = %v", err)
	}
	request := testRequest()
	request.OutputSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"additionalProperties":true}`)
	if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, advisory.ErrInvalidRequest) {
		t.Fatalf("permissive schema err = %v", err)
	}
}

func TestExecuteAdvisorySessionAllowsOnlyAttestedReadOnlyWebResearch(t *testing.T) {
	provider := newTestProvider(`{"answer":"researched"}`)
	provider.capability.WebResearch = true
	service := newService(t, provider)
	request := testRequest()
	request.WebResearch = advisory.WebResearchPolicy{Enabled: true, MaxSources: 2, MaxBytes: 1024}
	response, err := service.ExecuteAdvisorySession(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Receipt.Status != "completed" || !provider.request.Policy.WebResearchAllowed || provider.request.Policy.RepositoryWrites || provider.request.Policy.HumanApproval {
		t.Fatalf("web research policy/receipt = %#v/%#v", provider.request.Policy, response.Receipt)
	}
}
