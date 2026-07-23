package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/events"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestAuthorizeServeRequestRequiresLoopbackHTTPSAndExactIdentity(t *testing.T) {
	allowed := map[string]struct{}{"operator@example.invalid": {}}
	tests := []struct {
		name, peer, scheme, identity string
		serve                        bool
		want                         bool
	}{
		{"allowed", "127.0.0.1", "https", "operator@example.invalid", true, true},
		{"ipv6 loopback", "::1", "https", "operator@example.invalid", true, true},
		{"non loopback", "100.64.0.1", "https", "operator@example.invalid", true, false},
		{"insecure", "127.0.0.1", "http", "operator@example.invalid", true, false},
		{"wrong identity", "127.0.0.1", "https", "other@example.invalid", true, false},
		{"spoofed outside serve", "127.0.0.1", "https", "operator@example.invalid", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authorizeServeRequest(tt.peer, tt.scheme, tt.identity, tt.serve, allowed); got != tt.want {
				t.Fatalf("authorizeServeRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServerAppliesIdentityRequestIDAndSecurityHeaders(t *testing.T) {
	srv := newTestServer(t)
	response := request(t, srv, http.MethodGet, "/api/v1/health", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
	} {
		if got := response.Header.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if response.Header.Get("X-Request-ID") == "" {
		t.Fatal("missing request ID")
	}
}

func TestHealthzIsAvailableToTheLoopbackServiceManagerWithoutServeHeaders(t *testing.T) {
	srv := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response, err := srv.App().Test(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "ok\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestReadAPIsRedactInternalEventsAndPaginate(t *testing.T) {
	srv := newTestServer(t)
	response := request(t, srv, http.MethodGet, "/api/v1/tasks?limit=1", nil, nil)
	body := decodeJSON(t, response)
	if response.StatusCode != http.StatusOK || len(body["items"].([]any)) != 1 || body["next_cursor"] == "" {
		t.Fatalf("tasks response = %#v, status %d", body, response.StatusCode)
	}

	response = request(t, srv, http.MethodGet, "/api/v1/tasks/task-1", nil, nil)
	body = decodeJSON(t, response)
	results, _ := body["results"].([]any)
	if body["id"] != "task-1" || body["commit_sha"] != "abc123" || len(results) != 2 {
		t.Fatalf("task detail = %#v", body)
	}

	response = request(t, srv, http.MethodGet, "/api/v1/tasks/task-1/events", nil, nil)
	raw, _ := io.ReadAll(response.Body)
	if bytes.Contains(raw, []byte("internal secret")) || !bytes.Contains(raw, []byte("visible update")) {
		t.Fatalf("event projection leaked or omitted data: %s", raw)
	}

	response = request(t, srv, http.MethodGet, "/api/v1/tasks/task-1/attachments", nil, nil)
	raw, _ = io.ReadAll(response.Body)
	if bytes.Contains(raw, []byte("/private/inbox/screen.png")) || !bytes.Contains(raw, []byte("screen.png")) {
		t.Fatalf("attachment metadata = %s", raw)
	}

	response = request(t, srv, http.MethodGet, "/api/v1/usage", nil, nil)
	body = decodeJSON(t, response)
	if len(body["providers"].([]any)) != 1 {
		t.Fatalf("usage response = %#v", body)
	}
}

func TestUnknownTaskAndMethodRejection(t *testing.T) {
	srv := newTestServer(t)
	response := request(t, srv, http.MethodGet, "/api/v1/tasks/missing", nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status = %d", response.StatusCode)
	}
	response = request(t, srv, http.MethodPost, "/api/v1/tasks", strings.NewReader(`{}`), nil)
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", response.StatusCode)
	}
}

func TestRecoveryRequiresFreshCSRFCookieAndHeader(t *testing.T) {
	srv := newTestServer(t)
	csrf := request(t, srv, http.MethodGet, "/api/v1/csrf", nil, nil)
	var cookie *http.Cookie
	for _, value := range csrf.Cookies() {
		if value.Name == csrfCookieName {
			cookie = value
		}
	}
	body := decodeJSON(t, csrf)
	token, _ := body["token"].(string)
	if cookie == nil || token == "" || token != cookie.Value || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("invalid CSRF state: cookie=%#v body=%#v", cookie, body)
	}

	response := request(t, srv, http.MethodPost, "/api/v1/auth/codex/recovery", strings.NewReader(`{}`), nil)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d", response.StatusCode)
	}
	headers := http.Header{"X-CSRF-Token": []string{token}, "Cookie": []string{cookie.String()}}
	response = request(t, srv, http.MethodPost, "/api/v1/auth/codex/recovery", strings.NewReader(`{}`), headers)
	if response.StatusCode != http.StatusCreated || response.Header.Get("Cache-Control") != "no-store" {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("recovery status = %d, body=%s", response.StatusCode, raw)
	}
	if srv.csrf.consume("operator@example.invalid", token) {
		t.Fatal("direct CSRF replay was accepted")
	}

	response = request(t, srv, http.MethodPost, "/api/v1/auth/codex/recovery", strings.NewReader(`{}`), headers)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("replayed CSRF status = %d", response.StatusCode)
	}
}

func TestReplayAfterEventIDIsOrderedDeduplicatedAndDetectsGap(t *testing.T) {
	replay := []workmodel.Event{{ID: "e1"}, {ID: "e2"}, {ID: "e3"}}
	live := []events.Delivery{
		{Event: events.Event{ID: "e3"}},
		{Event: events.Event{ID: "e4"}},
		{Event: events.Event{ID: "e5"}, Dropped: 2},
	}
	got, gap := mergeReplay(replay, live, "e1")
	if strings.Join(got, ",") != "e2,e3,e4" || !gap {
		t.Fatalf("mergeReplay() = %v, gap=%v", got, gap)
	}
	if got, gap := mergeReplay(replay, nil, "expired-event"); len(got) != 0 || !gap {
		t.Fatalf("unknown cursor mergeReplay() = %v, gap=%v", got, gap)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	now := time.Unix(200, 0).UTC()
	started := now.Add(-time.Minute)
	tasks := []workmodel.Task{
		{ID: "task-1", RepoProfileID: "demo", Title: "Fix UI", State: workmodel.Running, Provider: workmodel.CodexSubscription, CommitSHA: "abc123", CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now, StartedAt: &started},
		{ID: "task-0", RepoProfileID: "demo", Title: "Older", State: workmodel.Completed, Provider: workmodel.ClaudeSubscription, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
	}
	data := &fakeReadStore{tasks: tasks, events: map[string][]workmodel.Event{"task-1": {
		{ID: "event-1", TaskID: "task-1", Type: workmodel.EventProviderMessage, Visibility: workmodel.VisibilityUser, Payload: json.RawMessage(`{"message":"visible update"}`), CreatedAt: now},
		{ID: "event-2", TaskID: "task-1", Type: workmodel.EventProviderMessage, Visibility: workmodel.VisibilityInternal, Payload: json.RawMessage(`{"message":"internal secret"}`), CreatedAt: now},
		{ID: "event-3", TaskID: "task-1", Type: workmodel.EventVerification, Visibility: workmodel.VisibilityUser, Payload: json.RawMessage(`{"status":"passed"}`), CreatedAt: now},
		{ID: "event-4", TaskID: "task-1", Type: workmodel.EventType("diff_summary"), Visibility: workmodel.VisibilityUser, Payload: json.RawMessage(`{"files":2}`), CreatedAt: now},
	}}, attachments: map[string][]workmodel.Attachment{"task-1": {{ID: "a1", TaskID: "task-1", Name: "screen.png", MediaType: "image/png", StoragePath: "/private/inbox/screen.png", SizeBytes: 42, CreatedAt: now}}}}
	srv, err := New(Config{AllowedIdentities: []string{"operator@example.invalid"}, ServeMode: true, CSRFSecret: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return now }}, Dependencies{
		Store:  data,
		Health: healthFunc(func(context.Context) (Health, error) { return Health{Status: "ok"}, nil }),
		Usage: usageFunc(func(context.Context) ([]ProviderUsage, error) {
			return []ProviderUsage{{Provider: "codex", UsedPercent: 25}}, nil
		}),
		Recovery: &fakeRecovery{},
		Live:     events.NewBus(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	// app.Test uses a synthetic 0.0.0.0 peer and no TLS. Keep production
	// authorization logic intact while making transport facts deterministic.
	srv.peerIP = func(any) string { return "127.0.0.1" }
	srv.scheme = func(any) string { return "https" }
	return srv
}

func request(t *testing.T, srv *Server, method, path string, body io.Reader, headers http.Header) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, "https://bridge.test"+path, body)
	req.Header.Set(tailscaleLoginHeader, "operator@example.invalid")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	response, err := srv.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeJSON(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()
	var value map[string]any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

type fakeReadStore struct {
	tasks       []workmodel.Task
	events      map[string][]workmodel.Event
	attachments map[string][]workmodel.Attachment
}

func (f *fakeReadStore) Task(_ context.Context, id string) (workmodel.Task, error) {
	for _, value := range f.tasks {
		if value.ID == id {
			return value, nil
		}
	}
	return workmodel.Task{}, store.ErrNotFound
}
func (f *fakeReadStore) ListTasks(context.Context, store.ListFilter) ([]workmodel.Task, error) {
	return append([]workmodel.Task(nil), f.tasks...), nil
}
func (f *fakeReadStore) Events(_ context.Context, taskID string) ([]workmodel.Event, error) {
	return append([]workmodel.Event(nil), f.events[taskID]...), nil
}
func (f *fakeReadStore) Attachments(_ context.Context, taskID string) ([]workmodel.Attachment, error) {
	return append([]workmodel.Attachment(nil), f.attachments[taskID]...), nil
}

type healthFunc func(context.Context) (Health, error)

func (f healthFunc) Health(ctx context.Context) (Health, error) { return f(ctx) }

type usageFunc func(context.Context) ([]ProviderUsage, error)

func (f usageFunc) Usage(ctx context.Context) ([]ProviderUsage, error) { return f(ctx) }

type fakeRecovery struct{ started bool }

func (f *fakeRecovery) Start(context.Context, string, string) (RecoveryView, error) {
	f.started = true
	return RecoveryView{ID: "recovery-1", Provider: "codex", State: "waiting"}, nil
}
func (f *fakeRecovery) Inspect(context.Context, string, string, string) (RecoveryView, error) {
	return RecoveryView{ID: "recovery-1", Provider: "codex", State: "waiting"}, nil
}
func (f *fakeRecovery) Submit(context.Context, string, string, string, string) (RecoveryView, error) {
	return RecoveryView{ID: "recovery-1", Provider: "codex", State: "running"}, nil
}
func (f *fakeRecovery) Cancel(context.Context, string, string, string) error { return nil }
