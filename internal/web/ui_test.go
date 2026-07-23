package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestOverviewPageIsPhoneFirstSemanticAndOperational(t *testing.T) {
	srv := newTestServer(t)
	response := request(t, srv, http.MethodGet, "/", nil, nil)
	body := readBody(t, response)
	for _, fragment := range []string{
		`<main`, `aria-label="Agent tasks"`, `data-state="running"`,
		`Fix UI`, `codex`, `1m 00s`, `Sistem durumu`, `Kuyruk`,
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("overview missing %q\n%s", fragment, body)
		}
	}
	if response.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("overview lacks CSP")
	}
}

func TestTaskPageEscapesProviderOutputAndLabelsTimeline(t *testing.T) {
	srv := newTestServer(t)
	srv.deps.Store.(*fakeReadStore).events["task-1"] = append(srv.deps.Store.(*fakeReadStore).events["task-1"], providerMessage("event-x", `<script>alert("x")</script>`, true))
	response := request(t, srv, http.MethodGet, "/tasks/task-1", nil, nil)
	body := readBody(t, response)
	for _, fragment := range []string{`<main`, `aria-label="Task timeline"`, `data-task-id="task-1"`, `Canlı bağlantı`, `screen.png`, `Kontroller`} {
		if !strings.Contains(body, fragment) {
			t.Errorf("task page missing %q\n%s", fragment, body)
		}
	}
	if strings.Contains(body, `<script>alert`) || !strings.Contains(body, `&lt;script&gt;alert`) {
		t.Fatalf("provider output was not escaped: %s", body)
	}
}

func TestAuthPageIsNoStoreAccessibleAndContainsNoRecoverySecret(t *testing.T) {
	srv := newTestServer(t)
	response := request(t, srv, http.MethodGet, "/auth/codex", nil, nil)
	body := readBody(t, response)
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("auth page may be cached")
	}
	for _, fragment := range []string{`<main`, `Codex authentication`, `aria-live="polite"`, `Kimlik doğrulamayı başlat`, `type="button"`} {
		if !strings.Contains(body, fragment) {
			t.Errorf("auth page missing %q\n%s", fragment, body)
		}
	}
	for _, forbidden := range []string{"device-code-secret", "oauth-token-secret", "setup-token"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("auth HTML contains secret marker %q", forbidden)
		}
	}
}

func TestEmbeddedAssetsImplementReconnectReducedMotionAndFocus(t *testing.T) {
	srv := newTestServer(t)
	js := readBody(t, request(t, srv, http.MethodGet, "/assets/app.js", nil, nil))
	for _, fragment := range []string{"EventSource", "last_event_id", "projection_version", "reconnect", "reset"} {
		if !strings.Contains(js, fragment) {
			t.Errorf("app.js missing %q", fragment)
		}
	}
	css := readBody(t, request(t, srv, http.MethodGet, "/assets/styles.css", nil, nil))
	for _, fragment := range []string{"@media (max-width: 44rem)", "prefers-reduced-motion", ":focus-visible", "overflow-wrap"} {
		if !strings.Contains(css, fragment) {
			t.Errorf("styles.css missing %q", fragment)
		}
	}
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.StatusCode, data)
	}
	return string(data)
}

func providerMessage(id, message string, visible bool) workmodel.Event {
	visibility := workmodel.VisibilityInternal
	if visible {
		visibility = workmodel.VisibilityUser
	}
	payload, _ := json.Marshal(map[string]string{"message": message})
	return workmodel.Event{ID: id, TaskID: "task-1", Type: workmodel.EventProviderMessage, Visibility: visibility, Payload: payload, CreatedAt: time.Unix(201, 0).UTC()}
}
