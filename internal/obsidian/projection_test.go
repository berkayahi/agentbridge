package obsidian_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/obsidian"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestProjectionPreservesPersonalNotesAndReplaysOrderedEvents(t *testing.T) {
	task := projectionTask(3, workmodel.Running)
	events := []localcontrol.Event{
		{Cursor: 2, ID: "event-2", Revision: 2, Type: "started"},
		{Cursor: 1, ID: "event-1", Revision: 1, Type: "task_created"},
		{Cursor: 2, ID: "event-2", Revision: 2, Type: "started"},
	}
	content, err := obsidian.ApplyObserved("# Local task\n\nPersonal notes stay here.\n", localcontrol.ObserveResponse{Task: task, Events: events}, obsidian.SyncLocal)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := obsidian.Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Metadata.CanonicalTaskID != task.ID || parsed.Metadata.LastAppliedCursor != 2 || !contains(parsed.Body, "Personal notes stay here.") {
		t.Fatalf("parsed projection = %#v body=%q", parsed.Metadata, parsed.Body)
	}
}

func TestProjectionRepairsCursorGapAndRejectsStaleRevision(t *testing.T) {
	task := projectionTask(4, workmodel.Completed)
	if _, err := obsidian.ApplyObserved("", localcontrol.ObserveResponse{Task: task, Events: []localcontrol.Event{{Cursor: 1}, {Cursor: 3}}}, obsidian.SyncLocal); !errors.Is(err, obsidian.ErrCursorGap) {
		t.Fatalf("gap error = %v, want ErrCursorGap", err)
	}
	content, err := obsidian.ApplyObserved("", localcontrol.ObserveResponse{Task: task, Events: []localcontrol.Event{{Cursor: 1}, {Cursor: 2}, {Cursor: 3}}}, obsidian.SyncLocal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := obsidian.ApplyTask(content, projectionTask(3, workmodel.Running), 2, obsidian.SyncLocal); !errors.Is(err, obsidian.ErrStaleRevision) {
		t.Fatalf("stale projection error = %v, want ErrStaleRevision", err)
	}
}

func TestProjectionUsesTaskCursorWhenGlobalEventsAreInterleaved(t *testing.T) {
	task := projectionTask(3, workmodel.Running)
	content, err := obsidian.ApplyObserved("", localcontrol.ObserveResponse{Task: task, Events: []localcontrol.Event{
		{Cursor: 1, TaskCursor: 1, ID: "event-1"},
		{Cursor: 3, TaskCursor: 2, ID: "event-2"},
		{Cursor: 5, TaskCursor: 3, ID: "event-3"},
	}}, obsidian.SyncLocal)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := obsidian.Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Metadata.LastAppliedCursor != 5 || parsed.Metadata.LastAppliedTaskCursor != 3 {
		t.Fatalf("interleaved projection cursors = %#v", parsed.Metadata)
	}
}

func TestProjectionPreservesImportedTemplateMetadata(t *testing.T) {
	content := "<!-- kovan:managed:v1 -->\n{\"schema_version\":1,\"task_id\":\"task-1\",\"project_id\":\"project-1\",\"board_id\":\"board-1\",\"last_applied_revision\":1,\"last_applied_cursor\":1,\"sync_state\":\"imported\",\"source_imported\":true,\"template_id\":\"kovan.task/v1\"}\n<!-- /kovan:managed -->\n\nPersonal notes.\n"
	updated, err := obsidian.ApplyTask(content, projectionTask(2, workmodel.Running), 2, obsidian.SyncLocal)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := obsidian.Parse(updated)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Metadata.SourceImported || parsed.Metadata.TemplateID != "kovan.task/v1" || !contains(parsed.Body, "Personal notes.") {
		t.Fatalf("preserved metadata = %#v body=%q", parsed.Metadata, parsed.Body)
	}
}

func TestProjectionTemplateImportUsesCanonicalAPIIdempotencyKeys(t *testing.T) {
	client := &templateClient{}
	response, err := obsidian.ImportTemplate(context.Background(), client, obsidian.Template{ID: "kovan.task.v1", Title: "Imported task", Prompt: "Keep the task canonical.", Project: "Kovan", Board: "Inbox"}, "repo-1", "local-mac")
	if err != nil || response.Task.ID != "task-1" {
		t.Fatalf("template response = %#v err=%v", response, err)
	}
	if client.projectKey != "template-project:kovan.task.v1" || client.boardKey != "template-board:kovan.task.v1" || client.taskKey != "template-task:kovan.task.v1" {
		t.Fatalf("template idempotency keys = %#v", client)
	}
}

func projectionTask(revision int64, state workmodel.State) localcontrol.TaskView {
	now := time.Unix(1_700_000_000, 0).UTC()
	return localcontrol.TaskView{ID: "task-1", ProjectID: "project-1", BoardID: "board-1", RepositoryID: "repo-1", Title: "Projection", Prompt: "Observe", State: state, Revision: revision, CreatedAt: now, UpdatedAt: now}
}

func contains(value, needle string) bool {
	return len(value) >= len(needle) && strings.Contains(value, needle)
}

type templateClient struct {
	projectKey, boardKey, taskKey string
}

func (c *templateClient) CreateProject(_ context.Context, request localcontrol.CreateProjectRequest) (localcontrol.ProjectResponse, error) {
	c.projectKey = request.IdempotencyKey
	return localcontrol.ProjectResponse{Project: localcontrol.Project{ID: "project-1"}}, nil
}
func (c *templateClient) CreateBoard(_ context.Context, request localcontrol.CreateBoardRequest) (localcontrol.BoardResponse, error) {
	c.boardKey = request.IdempotencyKey
	return localcontrol.BoardResponse{Board: localcontrol.Board{ID: "board-1"}}, nil
}
func (c *templateClient) CreateTask(_ context.Context, request localcontrol.CreateTaskRequest) (localcontrol.TaskResponse, error) {
	c.taskKey = request.IdempotencyKey
	return localcontrol.TaskResponse{Task: localcontrol.TaskView{ID: "task-1"}}, nil
}
