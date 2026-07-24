package localcontrol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestLocalAPIRequiresSecretAndAllocatesProjectID(t *testing.T) {
	data, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(localcontrol.CreateProjectRequest{Name: "API project", IdempotencyKey: "api-project"})
	request := httptest.NewRequest(http.MethodPost, "/v1/projects", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/projects", bytes.NewReader(body))
	request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("authenticated status = %d body=%s", response.Code, response.Body.String())
	}
	var value localcontrol.ProjectResponse
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil || value.Project.ID == "" || value.Project.ID == "api-project" {
		t.Fatalf("project response = %#v err=%v", value, err)
	}
}

func TestLocalAPIHonorsIfMatchRevisionBeforeMutation(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "if-match.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "If-Match project", IdempotencyKey: "if-match-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "if-match-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Main", IdempotencyKey: "if-match-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		Provider: "codex", Title: "Original", Prompt: "original prompt", IdempotencyKey: "if-match-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"title":"Updated","prompt":"updated prompt","idempotency_key":"if-match-update"}`
	request := httptest.NewRequest(http.MethodPatch, "/v1/tasks/"+task.Task.ID, strings.NewReader(body))
	request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	request.Header.Set("If-Match", `"2"`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("conflicting If-Match status = %d body=%s", response.Code, response.Body.String())
	}
	unchanged, err := data.Task(ctx, task.Task.ID)
	if err != nil || unchanged.Revision != 1 || unchanged.Title != "Original" {
		t.Fatalf("conflicting If-Match mutated task = %#v err=%v", unchanged, err)
	}

	request = httptest.NewRequest(http.MethodPatch, "/v1/tasks/"+task.Task.ID, strings.NewReader(body))
	request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	request.Header.Set("If-Match", `"1"`)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("valid If-Match status = %d body=%s", response.Code, response.Body.String())
	}
	updated, err := data.Task(ctx, task.Task.ID)
	if err != nil || updated.Revision != 2 || updated.Title != "Updated" {
		t.Fatalf("valid If-Match task = %#v err=%v", updated, err)
	}
}

func TestLocalAPIUsesAuthenticatedAuthorityForApproval(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "approval-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	executor := &fakeExecutor{}
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, Executor: executor, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Approval API", IdempotencyKey: "approval-api-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "approval-api-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Main", IdempotencyKey: "approval-api-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		Provider: "codex", Prompt: "approval", IdempotencyKey: "approval-api-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localcontrol.StartRequest{TaskID: task.Task.ID, Revision: task.Task.Revision, IdempotencyKey: "approval-api-start"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := data.UpsertApproval(ctx, workmodel.Approval{
		ID: "approval-api", TaskID: task.Task.ID, Kind: "command", Status: workmodel.ApprovalPending,
		RequestPayload: []byte(`{"summary":"approval"}`), RequestedAt: now, ExpiresAt: timePtr(now.Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(localcontrol.ApproveRequest{
		ApprovalID: "approval-api", Revision: started.Task.Revision, UserID: "attacker", Allow: true, IdempotencyKey: "approval-api-approve",
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.Task.ID+"/approve", bytes.NewReader(body))
	request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("approval status = %d body=%s", response.Code, response.Body.String())
	}
	if executor.approvedUser != localcontrol.LocalAuthorityUserID {
		t.Fatalf("approval user = %q, want authenticated local authority", executor.approvedUser)
	}
}

func TestLocalAPIRejectsTrailingJSONAndOversizedRequests(t *testing.T) {
	data, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"trailing JSON":  `{"name":"project","idempotency_key":"trailing"}{}`,
		"oversized JSON": `{"name":"` + strings.Repeat("x", (1<<20)+1) + `","idempotency_key":"large"}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader(body))
			request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestLocalTransportAuthOnOwnerUnixSocket(t *testing.T) {
	data, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	socketRoot, err := os.MkdirTemp("/tmp", "ab-local-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	socket := filepath.Join(socketRoot, "local-api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- localcontrol.ServeUnix(ctx, socket, handler) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		select {
		case serveErr := <-done:
			t.Fatalf("local API stopped before readiness: %v", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("local API socket did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	response, err := client.Get("http://agentbridge/healthz")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("health response = %v, err=%v", response, err)
	}
	_ = response.Body.Close()
	body, _ := json.Marshal(localcontrol.CreateProjectRequest{Name: "unauthenticated", IdempotencyKey: "unix-project-1"})
	response, err = client.Post("http://agentbridge/v1/projects", "application/json", bytes.NewReader(body))
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated response = %v, err=%v", response, err)
	}
	_ = response.Body.Close()
	request, err := http.NewRequest(http.MethodPost, "http://agentbridge/v1/projects", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-AgentBridge-Local-Auth", string(secret))
	response, err = client.Do(request)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("authenticated response = %v, err=%v", response, err)
	}
	_ = response.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local API did not stop")
	}
}
