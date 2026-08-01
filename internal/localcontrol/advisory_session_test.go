package localcontrol_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/advisory"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

type advisoryAuthority struct {
	calls    int
	response advisory.SessionResponse
}

func (a *advisoryAuthority) ExecuteAdvisorySession(context.Context, advisory.SessionRequest) (advisory.SessionResponse, error) {
	a.calls++
	return a.response, nil
}

func advisoryRequest() advisory.SessionRequest {
	return advisory.SessionRequest{
		Prompt:        "read only",
		Context:       advisory.ContextBundle{Items: []advisory.ContextItem{{Key: "source", Value: "bounded"}}},
		OutputSchema:  json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`),
		SchemaVersion: "schema-1", IdempotencyKey: "advisory-retry-1",
	}
}

func validAdvisoryResponse(request advisory.SessionRequest, now time.Time) advisory.SessionResponse {
	output := json.RawMessage(`{"answer":"ok"}`)
	policy := struct {
		Policy      advisory.ExecutionPolicy   `json:"policy"`
		WebResearch advisory.WebResearchPolicy `json:"web_research"`
	}{Policy: advisory.ExecutionPolicy{ReadOnly: true}, WebResearch: request.WebResearch}
	policyJSON, _ := json.Marshal(policy)
	contextJSON, _ := json.Marshal(request.Context)
	digest := func(value []byte) string {
		sum := sha256.Sum256(value)
		return hex.EncodeToString(sum[:])
	}
	return advisory.SessionResponse{
		ContractVersion: advisory.ContractVersion,
		Output:          output,
		Receipt: advisory.ExecutionReceipt{
			ReceiptID:          "receipt-1",
			ExecutionSessionID: "session-1",
			ProviderID:         "provider-a",
			ModelID:            "model-a",
			ContextDigest:      digest(contextJSON),
			PromptDigest:       digest([]byte(request.Prompt)),
			SchemaDigest:       digest(request.OutputSchema),
			PolicyDigest:       digest(policyJSON),
			OutputDigest:       digest(output),
			SchemaVersion:      request.SchemaVersion,
			ContractVersion:    advisory.ContractVersion,
			StartedAt:          now,
			CompletedAt:        now,
			Status:             "completed",
		},
	}
}

func TestAdvisorySessionIsDurablyIdempotentAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "advisory.db")
	data, err := sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	request := advisoryRequest()
	authority := &advisoryAuthority{response: validAdvisoryResponse(request, now)}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Advisory: authority, Clock: func() time.Time { return now }, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ExecuteAdvisorySession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ExecuteAdvisorySession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if authority.calls != 1 || first.Receipt.ReceiptID != second.Receipt.ReceiptID {
		t.Fatalf("calls/responses = %d/%#v/%#v", authority.calls, first, second)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	data, err = sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	restarted, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Advisory: authority, Clock: func() time.Time { return now }, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.ExecuteAdvisorySession(ctx, request)
	if err != nil || authority.calls != 1 || replayed.Receipt.ReceiptID != first.Receipt.ReceiptID {
		t.Fatalf("replayed = %#v err=%v calls=%d", replayed, err, authority.calls)
	}
}

func TestAdvisorySessionRejectsReceiptBindingBeforePersistence(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "receipt-binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	now := time.Unix(1_700_000_000, 0).UTC()
	request := advisoryRequest()
	response := validAdvisoryResponse(request, now)
	response.Receipt.PromptDigest = strings.Repeat("0", sha256.Size*2)
	authority := &advisoryAuthority{response: response}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Advisory: authority, Clock: func() time.Time { return now }, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.ExecuteAdvisorySession(ctx, request)
	if !errors.Is(err, advisory.ErrReceiptIntegrity) {
		t.Fatalf("receipt binding err = %v", err)
	}
	if authority.calls != 1 || len(got.Output) != 0 {
		t.Fatalf("invalid receipt crossed boundary: calls=%d response=%#v", authority.calls, got)
	}
	if _, err := data.LoadIdempotency(ctx, request.IdempotencyKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalid receipt was persisted: %v", err)
	}
}

func TestAdvisorySessionRejectsCorruptCachedReceiptInsteadOfReplaying(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "corrupt-receipt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	now := time.Unix(1_700_000_000, 0).UTC()
	request := advisoryRequest()
	authority := &advisoryAuthority{response: validAdvisoryResponse(request, now)}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Advisory: authority, Clock: func() time.Time { return now }, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteAdvisorySession(ctx, request); err != nil {
		t.Fatal(err)
	}
	record, err := data.LoadIdempotency(ctx, request.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	var cached advisory.SessionResponse
	if err := json.Unmarshal(record.ResponseBytes, &cached); err != nil {
		t.Fatal(err)
	}
	cached.Receipt.OutputDigest = strings.Repeat("0", sha256.Size*2)
	record.ResponseBytes, err = json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.SaveIdempotency(ctx, record); err != nil {
		t.Fatal(err)
	}
	replayed, err := service.ExecuteAdvisorySession(ctx, request)
	if !errors.Is(err, advisory.ErrReceiptIntegrity) {
		t.Fatalf("corrupt replay err = %v", err)
	}
	if authority.calls != 1 || len(replayed.Output) != 0 {
		t.Fatalf("corrupt receipt was replayed: calls=%d response=%#v", authority.calls, replayed)
	}
}

func TestAdvisorySessionRouteUsesLocalAuthAndDoesNotExposeMutationFields(t *testing.T) {
	data, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "advisory-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	requestBody := advisoryRequest()
	authority := &advisoryAuthority{response: validAdvisoryResponse(requestBody, time.Now().UTC())}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Advisory: authority})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(requestBody)
	request := httptest.NewRequest(http.MethodPost, "/v1/advisory-sessions", bytes.NewReader(body))
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized || authority.calls != 0 {
		t.Fatalf("unauthorized status/calls = %d/%d", unauthorized.Code, authority.calls)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/advisory-sessions", bytes.NewReader(body))
	request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || authority.calls != 1 {
		t.Fatalf("authorized status/calls = %d/%d body=%s", response.Code, authority.calls, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "repository") || strings.Contains(response.Body.String(), "branch") || strings.Contains(response.Body.String(), "approval") {
		t.Fatalf("response exposed forbidden domain fields: %s", response.Body.String())
	}
}

func TestAdvisorySessionRejectsSecretOutputBeforePersistence(t *testing.T) {
	data, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "secret-output.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	authority := &advisoryAuthority{response: advisory.SessionResponse{
		ContractVersion: advisory.ContractVersion,
		Output:          json.RawMessage(`{"answer":"ok","token":"secret-value"}`),
	}}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Advisory: authority})
	if err != nil {
		t.Fatal(err)
	}
	request := advisoryRequest()
	if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, advisory.ErrPolicyViolation) {
		t.Fatalf("secret output err = %v", err)
	}
	if _, err := service.ExecuteAdvisorySession(context.Background(), request); !errors.Is(err, advisory.ErrPolicyViolation) || authority.calls != 2 {
		t.Fatalf("secret output replay = err %v calls %d", err, authority.calls)
	}
}

func TestAdvisorySessionRejectsNestedSecretsBeforeAuthorityOrPersistence(t *testing.T) {
	data, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "nested-secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	authority := &advisoryAuthority{response: advisory.SessionResponse{
		ContractVersion: advisory.ContractVersion,
		Output:          json.RawMessage(`{"answer":"ok"}`),
	}}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Advisory: authority})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		key    string
		mutate func(*advisory.SessionRequest)
	}{
		{
			key: "nested-enum",
			mutate: func(request *advisory.SessionRequest) {
				request.OutputSchema = json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"kind":{"type":"string","enum":[{"nested":{"token":"enum-secret"}}]}},"required":["kind"],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`)
			},
		},
		{
			key: "nested-const",
			mutate: func(request *advisory.SessionRequest) {
				request.OutputSchema = json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"payload":{"type":"object","const":{"nested":{"password":"const-secret"}}}},"required":["payload"],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`)
			},
		},
		{
			key: "nested-context",
			mutate: func(request *advisory.SessionRequest) {
				request.Context.Items[0].Value = `{"nested":[{"password":"context-secret"}]}`
			},
		},
	}

	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			request := advisoryRequest()
			request.IdempotencyKey = test.key
			test.mutate(&request)
			response, err := service.ExecuteAdvisorySession(context.Background(), request)
			if !errors.Is(err, advisory.ErrPolicyViolation) {
				t.Fatalf("nested secret err = %v", err)
			}
			if authority.calls != 0 || len(response.Output) != 0 || strings.Contains(string(response.Output), "secret") {
				t.Fatalf("nested secret crossed local boundary: calls=%d response=%#v", authority.calls, response)
			}
			if _, loadErr := data.LoadIdempotency(context.Background(), test.key); !errors.Is(loadErr, store.ErrNotFound) {
				t.Fatalf("nested secret was persisted: %v", loadErr)
			}
		})
	}
}
