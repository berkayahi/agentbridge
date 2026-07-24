package obsidian_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/obsidian"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestSyncTaskFileRepairsCursorGapAndPreservesPersonalMarkdown(t *testing.T) {
	task := projectionTask(1, workmodel.Running)
	initial, err := obsidian.ApplyTask("# Personal heading\n\nKeep this paragraph.\n", task, 1, obsidian.SyncLocal)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "task.md")
	if err := os.WriteFile(path, []byte(initial), 0o640); err != nil {
		t.Fatal(err)
	}
	observer := &gapObserver{task: projectionTask(4, workmodel.Completed)}
	parsed, err := obsidian.SyncTaskFile(context.Background(), observer, task.ID, path, obsidian.SyncLocal)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Metadata.LastAppliedCursor != 3 || parsed.Metadata.LastAppliedRevision != 4 || !contains(parsed.Body, "Keep this paragraph.") {
		t.Fatalf("synced note = %#v body=%q", parsed.Metadata, parsed.Body)
	}
	if len(observer.requests) != 2 || observer.requests[0].AfterCursor != 1 || observer.requests[1].AfterCursor != 0 {
		t.Fatalf("observe requests = %#v, want cursor repair", observer.requests)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("projected note mode = %o, want 640", info.Mode().Perm())
	}
}

func TestSyncTaskFileRefusesConcurrentOverwrite(t *testing.T) {
	task := projectionTask(1, workmodel.Running)
	path := filepath.Join(t.TempDir(), "task.md")
	if err := os.WriteFile(path, []byte("local edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	observer := &mutatingObserver{path: path, task: projectionTask(2, workmodel.Completed)}
	if _, err := obsidian.SyncTaskFile(context.Background(), observer, task.ID, path, obsidian.SyncLocal); !errors.Is(err, obsidian.ErrNoteChanged) {
		t.Fatalf("concurrent projection error = %v, want ErrNoteChanged", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "operator edit\n" {
		t.Fatalf("concurrent note contents = %q", contents)
	}
}

type gapObserver struct {
	task     localcontrol.TaskView
	requests []localcontrol.ObserveRequest
}

func (o *gapObserver) Observe(_ context.Context, request localcontrol.ObserveRequest) (localcontrol.ObserveResponse, error) {
	o.requests = append(o.requests, request)
	if len(o.requests) == 1 {
		return localcontrol.ObserveResponse{Task: o.task, Events: []localcontrol.Event{{Cursor: 3, ID: "event-3"}}}, nil
	}
	return localcontrol.ObserveResponse{Task: o.task, Events: []localcontrol.Event{
		{Cursor: 1, ID: "event-1"}, {Cursor: 2, ID: "event-2"}, {Cursor: 3, ID: "event-3"},
	}}, nil
}

type mutatingObserver struct {
	path string
	task localcontrol.TaskView
}

func (o *mutatingObserver) Observe(_ context.Context, _ localcontrol.ObserveRequest) (localcontrol.ObserveResponse, error) {
	if err := os.WriteFile(o.path, []byte("operator edit\n"), 0o600); err != nil {
		return localcontrol.ObserveResponse{}, err
	}
	return localcontrol.ObserveResponse{Task: o.task, Events: []localcontrol.Event{{Cursor: 1, ID: "event-1"}}}, nil
}
