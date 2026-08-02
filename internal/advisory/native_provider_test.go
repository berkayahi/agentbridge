package advisory_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/berkayahi/agentbridge/internal/advisory"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

type attestedAnalysisProvider struct {
	request provider.AnalysisRequest
}

func (p *attestedAnalysisProvider) AnalysisIsolationAttestation() provider.AnalysisIsolationAttestation {
	return provider.AnalysisIsolationAttestation{
		Mechanism: "test-sandbox", FilesystemReadsWorkspaceOnly: true, HostEnvironmentExcluded: true,
		NetworkDenied: true, ProductionDataDenied: true, DestructiveActionsDenied: true,
	}
}

func (p *attestedAnalysisProvider) AnalyzeReadOnly(_ context.Context, request provider.AnalysisRequest) (provider.AnalysisResult, error) {
	p.request = request
	return provider.AnalysisResult{ProviderID: workmodel.Provider("provider-a"), Model: request.Model, Output: []byte(`{"answer":"native"}`)}, nil
}

func TestNativeProviderReceivesStrictSchemaAndPreservesReadOnlyBoundary(t *testing.T) {
	native := &attestedAnalysisProvider{}
	wrapped := advisory.NativeProvider{Provider: native, ProviderID: "provider-a", ModelID: "model-a"}
	service, err := advisory.New(advisory.Config{
		Catalog: testCatalog{profiles: []advisory.ProviderProfile{
			{ID: "provider-a", ModelID: "model-a", Available: true, Capabilities: wrapped.Capability()},
		}},
		Providers: map[string]advisory.Provider{"provider-a": wrapped},
	})
	if err != nil {
		t.Fatal(err)
	}
	schema := `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`
	request := advisory.SessionRequest{
		ProviderID: "provider-a", ModelID: "model-a", Prompt: "return the bounded answer",
		Context:      advisory.ContextBundle{Items: []advisory.ContextItem{{Key: "source", Value: "bounded"}}},
		OutputSchema: json.RawMessage(schema), SchemaVersion: "schema-1", IdempotencyKey: "native-request-1",
	}
	if _, err := service.ExecuteAdvisorySession(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(native.request.Input.Text, schema) {
		t.Fatalf("native input omitted schema: %s", native.request.Input.Text)
	}
	if native.request.Policy.WorkspacePath == "" || len(native.request.Policy.WritablePaths) != 0 || native.request.Policy.NetworkAccess || native.request.Policy.ApprovalAllowed || native.request.Policy.DeliveryAllowed || native.request.Policy.HostEnvironmentAllowed || native.request.Policy.ProductionDataAllowed || native.request.Policy.CredentialsAllowed || native.request.Policy.DestructiveActionsAllow || !native.request.Policy.RequireOSIsolation {
		t.Fatalf("native policy = %#v", native.request.Policy)
	}
}
