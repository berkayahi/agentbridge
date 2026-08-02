package advisory_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/advisory"
	"github.com/berkayahi/agentbridge/internal/security"
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
	err        error
	request    advisory.ExecutionRequest
	calls      int
}

func (p *testProvider) Capability() advisory.ProviderCapability { return p.capability }

func (p *testProvider) Execute(_ context.Context, request advisory.ExecutionRequest) (advisory.ExecutionResult, error) {
	p.calls++
	p.request = request
	return p.result, p.err
}

func newTestProvider(output string) *testProvider {
	return &testProvider{
		capability: advisory.ReadOnlyCapability("provider-a"),
		result:     advisory.ExecutionResult{ProviderID: "provider-a", ModelID: "model-a", Output: []byte(output)},
	}
}

func newService(t *testing.T, provider *testProvider) *advisory.Service {
	return newServiceWithRedactor(t, provider, nil)
}

func newServiceWithRedactor(t *testing.T, provider *testProvider, redactor *security.Redactor) *advisory.Service {
	t.Helper()
	service, err := advisory.New(advisory.Config{
		Catalog: testCatalog{profiles: []advisory.ProviderProfile{{
			ID: "provider-a", ModelID: "model-a", Available: true, Capabilities: provider.Capability(),
		}}},
		Providers: map[string]advisory.Provider{"provider-a": provider},
		Clock:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		NewID:     func(prefix string) string { return prefix + "-1" },
		Redactor:  redactor,
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
		provider.request.Policy.ExternalStateMutation || provider.request.Policy.HumanApproval || provider.request.Policy.WebResearchAllowed {
		t.Fatalf("provider request policy = %#v", provider.request.Policy)
	}
	if response.Receipt.ProviderID != "provider-a" || response.Receipt.ModelID != "model-a" || response.Receipt.SchemaVersion != "schema-1" ||
		response.Receipt.ExecutionSessionID == "" || response.Receipt.ReceiptID == "" || response.Receipt.Status != "completed" {
		t.Fatalf("receipt = %#v", response.Receipt)
	}
	if response.Receipt.ContextDigest == "" || response.Receipt.PromptDigest == "" || response.Receipt.SchemaDigest == "" || response.Receipt.PolicyDigest == "" || response.Receipt.OutputDigest == "" || response.Receipt.StartedAt.IsZero() || response.Receipt.CompletedAt.IsZero() {
		t.Fatalf("receipt provenance = %#v", response.Receipt)
	}
	if provider.request.SchemaDigest != response.Receipt.SchemaDigest || provider.request.PolicyDigest != response.Receipt.PolicyDigest {
		t.Fatalf("execution binding/receipt mismatch = %#v/%#v", provider.request, response.Receipt)
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
	provider.capability.EffectivePolicy.RepositoryWrite = true
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

func TestExecuteAdvisorySessionRejectsSecretSchemaAndOutputFields(t *testing.T) {
	provider := newTestProvider(`{"answer":"ok"}`)
	service := newService(t, provider)
	request := testRequest()
	request.OutputSchema = json.RawMessage(`{"type":"object","properties":{"password":{"type":"string"}},"required":["password"],"additionalProperties":false}`)
	if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, advisory.ErrPolicyViolation) {
		t.Fatalf("secret schema err = %v", err)
	}
	provider.result.Output = []byte(`{"answer":"ok","api_key":"secret-value"}`)
	if _, err := service.ExecuteAdvisorySession(context.Background(), testRequest()); !errors.Is(err, advisory.ErrPolicyViolation) {
		t.Fatalf("secret output err = %v", err)
	}
}

func TestExecuteAdvisorySessionRejectsNestedSecretsInSchemaValuesAndContextBeforeProvider(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*advisory.SessionRequest)
	}{
		{
			name: "enum",
			mutate: func(request *advisory.SessionRequest) {
				request.OutputSchema = json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"kind":{"type":"string","enum":["safe",{"nested":[{"token":"enum-secret"}]}]}},"required":["kind"],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`)
			},
		},
		{
			name: "const",
			mutate: func(request *advisory.SessionRequest) {
				request.OutputSchema = json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"payload":{"type":"object","const":{"nested":[{"password":"const-secret"}]} }},"required":["payload"],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`)
			},
		},
		{
			name: "context",
			mutate: func(request *advisory.SessionRequest) {
				request.Context.Items[0].Value = `{"nested":[{"password":"context-secret"}]}`
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := newTestProvider(`{"answer":"ok"}`)
			service := newService(t, provider)
			request := testRequest()
			test.mutate(&request)
			if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, advisory.ErrPolicyViolation) {
				t.Fatalf("nested secret err = %v", err)
			}
			if provider.calls != 0 {
				t.Fatalf("nested secret reached provider: calls=%d request=%#v", provider.calls, provider.request)
			}
		})
	}
}

func TestExecuteAdvisorySessionRejectsSchemaScalarSecretsAndDuplicateKeysBeforeProvider(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		want   error
	}{
		{
			name:   "description scalar",
			schema: `{"type":"object","description":"schema-secret-value","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`,
			want:   advisory.ErrPolicyViolation,
		},
		{
			name:   "nested enum scalar",
			schema: `{"type":"object","properties":{"answer":{"type":"string","enum":["safe",{"nested":[{"value":"enum-secret-value"}]}]}},"required":["answer"],"additionalProperties":false}`,
			want:   advisory.ErrPolicyViolation,
		},
		{
			name:   "nested const scalar",
			schema: `{"type":"object","properties":{"answer":{"type":"string","const":{"nested":[{"value":"password=const-secret"}]} }},"required":["answer"],"additionalProperties":false}`,
			want:   advisory.ErrPolicyViolation,
		},
		{
			name:   "duplicate root key",
			schema: `{"type":"object","type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`,
			want:   advisory.ErrInvalidRequest,
		},
		{
			name:   "duplicate nested key",
			schema: `{"type":"object","properties":{"answer":{"type":"string","enum":[{"nested":{"value":"safe","value":"duplicate"}}]}},"required":["answer"],"additionalProperties":false}`,
			want:   advisory.ErrInvalidRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := newTestProvider(`{"answer":"ok"}`)
			service := newService(t, provider)
			request := testRequest()
			request.OutputSchema = json.RawMessage(test.schema)
			if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, test.want) {
				t.Fatalf("schema err = %v, want %v", err, test.want)
			}
			if provider.calls != 0 {
				t.Fatalf("invalid schema reached provider: calls=%d request=%#v", provider.calls, provider.request)
			}
		})
	}
}

func TestExecuteAdvisorySessionRecursivelyRedactsConfiguredContextScalars(t *testing.T) {
	const secret = "configured-context-secret"
	provider := newTestProvider(`{"answer":"ok"}`)
	service := newServiceWithRedactor(t, provider, security.NewRedactor(security.Config{Secrets: []string{secret}}))
	request := testRequest()
	request.Context.Items[0].Value = `{"nested":[{"message":"configured-context-secret"}]}`
	response, err := service.ExecuteAdvisorySession(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(provider.request.Context.Items[0].Value, secret) || strings.Contains(string(response.Output), secret) {
		t.Fatalf("configured context secret crossed boundary: request=%#v response=%s", provider.request.Context, response.Output)
	}
	if !strings.Contains(provider.request.Context.Items[0].Value, "[REDACTED:configured]") {
		t.Fatalf("nested configured context was not redacted: %#v", provider.request.Context)
	}
}

func TestExecuteAdvisorySessionRejectsUnredactableNestedContextScalar(t *testing.T) {
	provider := newTestProvider(`{"answer":"ok"}`)
	service := newService(t, provider)
	request := testRequest()
	request.Context.Items[0].Value = `{"nested":[{"message":"secret-value"}]}`
	if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, advisory.ErrPolicyViolation) {
		t.Fatalf("nested scalar err = %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("unredactable nested scalar reached provider: %d", provider.calls)
	}
}

func TestExecuteAdvisorySessionUsesConfiguredRedactorForSchemaValues(t *testing.T) {
	const secret = "opaque-configured-schema-secret"
	provider := newTestProvider(`{"answer":"ok"}`)
	service := newServiceWithRedactor(t, provider, security.NewRedactor(security.Config{Secrets: []string{secret}}))
	request := testRequest()
	request.OutputSchema = json.RawMessage(`{"type":"object","description":"opaque-configured-schema-secret","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, advisory.ErrPolicyViolation) {
		t.Fatalf("configured schema secret err = %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("configured schema secret reached provider: %d", provider.calls)
	}
}

func TestExecuteAdvisorySessionFailsClosedWithoutWebAdapter(t *testing.T) {
	provider := newTestProvider(`{"answer":"researched"}`)
	provider.capability.WebResearch = advisory.WebResearchCapability{Available: true, Enforced: true, MaxSources: 4, MaxBytes: 1024}
	service := newService(t, provider)
	request := testRequest()
	request.WebResearch = advisory.WebResearchPolicy{Enabled: true, MaxSources: 2, MaxBytes: 1024}
	if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, advisory.ErrPolicyViolation) {
		t.Fatalf("web research err = %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("web research reached provider: %d calls", provider.calls)
	}
}
