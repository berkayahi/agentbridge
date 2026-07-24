package localcontrol_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestPendingApprovalsExposeRealTaskScopedIDsThroughClient(t *testing.T) {
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "approvals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: "Approval project", IdempotencyKey: "approval-project"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "approval-repository"})
	if err != nil {
		t.Fatal(err)
	}
	board, err := service.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: "Build", IdempotencyKey: "approval-board"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repository.Repository.ID,
		Provider: workmodel.CodexSubscription, Prompt: "show the approval", IdempotencyKey: "approval-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	if err := data.UpsertApproval(ctx, workmodel.Approval{
		ID: "provider-approval-42", TaskID: task.Task.ID, Kind: "command", Status: workmodel.ApprovalPending,
		RequestPayload: []byte(`{"summary":"run the verified command"}`), RequestedAt: now, ExpiresAt: &expires,
	}); err != nil {
		t.Fatal(err)
	}
	expired := now.Add(-time.Minute)
	if err := data.UpsertApproval(ctx, workmodel.Approval{
		ID: "expired-approval", TaskID: task.Task.ID, Kind: "command", Status: workmodel.ApprovalPending,
		RequestPayload: []byte(`{"summary":"do not show"}`), RequestedAt: expired, ExpiresAt: &expired,
	}); err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := localcontrol.NewClient(server.URL, secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.PendingApprovals(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Approvals) != 1 || response.Approvals[0].ID != "provider-approval-42" || response.Approvals[0].TaskID != task.Task.ID {
		t.Fatalf("pending approvals = %#v", response.Approvals)
	}
	if string(response.Approvals[0].RequestPayload) != `{"summary":"run the verified command"}` {
		t.Fatalf("approval payload = %s", response.Approvals[0].RequestPayload)
	}
}
