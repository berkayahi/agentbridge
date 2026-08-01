package localcontrol_test

import (
	"bytes"
	"context"
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

func TestAdvisorySessionIsDurablyIdempotentAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "advisory.db")
	data, err := sqlite.OpenV2Runtime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	authority := &advisoryAuthority{response: advisory.SessionResponse{
		ContractVersion: advisory.ContractVersion,
		Output:          json.RawMessage(`{"answer":"ok"}`),
		Receipt:         advisory.ExecutionReceipt{ReceiptID: "receipt-1", ExecutionSessionID: "session-1", ProviderID: "provider-a", ModelID: "model-a", SchemaVersion: "schema-1", Status: "completed", StartedAt: now, CompletedAt: now},
	}}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Advisory: authority, Clock: func() time.Time { return now }, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	request := advisoryRequest()
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

func TestAdvisorySessionRouteUsesLocalAuthAndDoesNotExposeMutationFields(t *testing.T) {
	data, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "advisory-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	authority := &advisoryAuthority{response: advisory.SessionResponse{ContractVersion: advisory.ContractVersion, Output: json.RawMessage(`{"answer":"ok"}`)}}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Advisory: authority})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(advisoryRequest())
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
