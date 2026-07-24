// Package obsidian owns the standalone Obsidian projection boundary. It
// renders local-control state into managed Markdown metadata without becoming
// an authority or importing provider, repository, or SQLite concerns.
package obsidian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

const (
	SchemaVersion = 1
	managedStart  = "<!-- kovan:managed:v1 -->"
	managedEnd    = "<!-- /kovan:managed -->"
	taskViewStart = "<!-- kovan:task:v1 -->"
	taskViewEnd   = "<!-- /kovan:task -->"
)

var (
	ErrInvalidNote   = errors.New("obsidian: invalid managed note")
	ErrStaleRevision = errors.New("obsidian: stale canonical revision")
	ErrCursorGap     = errors.New("obsidian: event cursor gap")
	ErrConflict      = errors.New("obsidian: canonical task conflict")
)

type SyncState string

const (
	SyncLocal       SyncState = "local"
	SyncImported    SyncState = "imported"
	SyncNeedsReplay SyncState = "needs_replay"
	SyncConflict    SyncState = "conflict"
)

type Metadata struct {
	SchemaVersion         int       `json:"schema_version"`
	CanonicalTaskID       string    `json:"task_id"`
	ProjectID             string    `json:"project_id"`
	BoardID               string    `json:"board_id"`
	Title                 string    `json:"title,omitempty"`
	State                 string    `json:"state,omitempty"`
	LastAppliedRevision   int64     `json:"last_applied_revision"`
	LastAppliedCursor     uint64    `json:"last_applied_cursor"`
	LastAppliedTaskCursor uint64    `json:"last_applied_task_cursor,omitempty"`
	SyncState             SyncState `json:"sync_state"`
	SourceImported        bool      `json:"source_imported,omitempty"`
	TemplateID            string    `json:"template_id,omitempty"`
}

type ParsedNote struct {
	Metadata    Metadata
	Body        string
	TaskBody    string
	Managed     bool
	HasTaskView bool
}

type Template struct {
	ID      string
	Title   string
	Prompt  string
	Project string
	Board   string
}

type ControlClient interface {
	CreateProject(context.Context, localcontrol.CreateProjectRequest) (localcontrol.ProjectResponse, error)
	CreateBoard(context.Context, localcontrol.CreateBoardRequest) (localcontrol.BoardResponse, error)
	CreateTask(context.Context, localcontrol.CreateTaskRequest) (localcontrol.TaskResponse, error)
}

func Parse(content string) (ParsedNote, error) {
	start := strings.Index(content, managedStart)
	if start < 0 {
		return ParsedNote{Body: content}, nil
	}
	endOffset := strings.Index(content[start+len(managedStart):], managedEnd)
	if endOffset < 0 {
		return ParsedNote{}, ErrInvalidNote
	}
	end := start + len(managedStart) + endOffset
	encoded := strings.TrimSpace(content[start+len(managedStart) : end])
	var metadata Metadata
	if err := json.Unmarshal([]byte(encoded), &metadata); err != nil || metadata.SchemaVersion != SchemaVersion || metadata.CanonicalTaskID == "" {
		return ParsedNote{}, ErrInvalidNote
	}
	body := content[:start] + content[end+len(managedEnd):]
	taskStart := strings.Index(body, taskViewStart)
	if taskStart < 0 {
		return ParsedNote{Metadata: metadata, Body: body, Managed: true}, nil
	}
	taskOffset := taskStart + len(taskViewStart)
	taskEndOffset := strings.Index(body[taskOffset:], taskViewEnd)
	if taskEndOffset < 0 {
		return ParsedNote{}, ErrInvalidNote
	}
	taskEnd := taskOffset + taskEndOffset
	taskBody := body[taskOffset:taskEnd]
	body = body[:taskStart] + body[taskEnd+len(taskViewEnd):]
	return ParsedNote{Metadata: metadata, Body: body, TaskBody: taskBody, Managed: true, HasTaskView: true}, nil
}

func ApplyTask(content string, task localcontrol.TaskView, cursor uint64, state SyncState) (string, error) {
	return applyTaskWithCursors(content, task, cursor, cursor, state)
}

func applyTaskWithCursors(content string, task localcontrol.TaskView, cursor, taskCursor uint64, state SyncState) (string, error) {
	if task.ID == "" || task.ProjectID == "" || task.BoardID == "" || task.Revision <= 0 {
		return "", ErrInvalidNote
	}
	parsed, err := Parse(content)
	if err != nil {
		return "", err
	}
	if parsed.Managed && parsed.Metadata.CanonicalTaskID != task.ID {
		return "", fmt.Errorf("note task %q, response task %q: %w", parsed.Metadata.CanonicalTaskID, task.ID, ErrConflict)
	}
	if parsed.Managed && (task.Revision < parsed.Metadata.LastAppliedRevision ||
		(task.Revision == parsed.Metadata.LastAppliedRevision && cursor < parsed.Metadata.LastAppliedCursor) ||
		(task.Revision == parsed.Metadata.LastAppliedRevision && taskCursor < parsed.Metadata.LastAppliedTaskCursor)) {
		return "", ErrStaleRevision
	}
	metadata := Metadata{
		SchemaVersion: SchemaVersion, CanonicalTaskID: task.ID, ProjectID: task.ProjectID, BoardID: task.BoardID,
		Title: task.Title, State: string(task.State), LastAppliedRevision: task.Revision, LastAppliedCursor: cursor, LastAppliedTaskCursor: taskCursor,
		SyncState: state, SourceImported: parsed.Managed && parsed.Metadata.SourceImported,
		TemplateID: func() string {
			if parsed.Managed {
				return parsed.Metadata.TemplateID
			}
			return ""
		}(),
	}
	return replaceManaged(content, parsed, metadata, task.Prompt), nil
}

func ApplyObserved(content string, observed localcontrol.ObserveResponse, state SyncState) (string, error) {
	parsed, err := Parse(content)
	if err != nil {
		return "", err
	}
	current := uint64(0)
	taskCursor := uint64(0)
	if parsed.Managed {
		current = parsed.Metadata.LastAppliedCursor
		taskCursor = parsed.Metadata.LastAppliedTaskCursor
	}
	events := append([]localcontrol.Event(nil), observed.Events...)
	if hasTaskCursors(events) {
		sort.Slice(events, func(i, j int) bool { return events[i].TaskCursor < events[j].TaskCursor })
		// Notes written before task-scoped cursors were introduced only have a
		// global cursor. A first full replay starts at task cursor one; an
		// incremental response cannot safely infer the missing task cursor.
		if parsed.Managed && taskCursor == 0 && current > 0 {
			if len(events) == 0 || events[0].TaskCursor != 1 {
				return "", ErrCursorGap
			}
			current = parsed.Metadata.LastAppliedCursor
		}
		for _, event := range events {
			if event.TaskCursor <= taskCursor {
				continue
			}
			if event.TaskCursor != taskCursor+1 {
				return "", ErrCursorGap
			}
			taskCursor = event.TaskCursor
			if event.Cursor > current {
				current = event.Cursor
			}
		}
	} else {
		sort.Slice(events, func(i, j int) bool { return events[i].Cursor < events[j].Cursor })
		for _, event := range events {
			if event.Cursor <= current {
				continue
			}
			if event.Cursor != current+1 {
				return "", ErrCursorGap
			}
			current = event.Cursor
		}
	}
	return applyTaskWithCursors(content, observed.Task, current, taskCursor, state)
}

func hasTaskCursors(events []localcontrol.Event) bool {
	if len(events) == 0 {
		return false
	}
	for _, event := range events {
		if event.TaskCursor == 0 {
			return false
		}
	}
	return true
}

func ImportTemplate(ctx context.Context, client ControlClient, template Template, repositoryID, targetDeviceID string) (localcontrol.TaskResponse, error) {
	if client == nil || strings.TrimSpace(template.ID) == "" || strings.TrimSpace(template.Title) == "" || strings.TrimSpace(template.Prompt) == "" || strings.TrimSpace(repositoryID) == "" {
		return localcontrol.TaskResponse{}, ErrInvalidNote
	}
	project, err := client.CreateProject(ctx, localcontrol.CreateProjectRequest{Name: template.Project, IdempotencyKey: "template-project:" + template.ID})
	if err != nil {
		return localcontrol.TaskResponse{}, err
	}
	board, err := client.CreateBoard(ctx, localcontrol.CreateBoardRequest{ProjectID: project.Project.ID, Name: template.Board, IdempotencyKey: "template-board:" + template.ID})
	if err != nil {
		return localcontrol.TaskResponse{}, err
	}
	return client.CreateTask(ctx, localcontrol.CreateTaskRequest{
		ProjectID: project.Project.ID, BoardID: board.Board.ID, RepositoryID: repositoryID, TargetDeviceID: targetDeviceID,
		Provider: "codex", Title: template.Title, Prompt: template.Prompt, IdempotencyKey: "template-task:" + template.ID,
	})
}

func replaceManaged(original string, parsed ParsedNote, metadata Metadata, taskBody string) string {
	encoded, _ := json.MarshalIndent(metadata, "", "  ")
	managedBlock := managedStart + "\n" + string(encoded) + "\n" + managedEnd
	taskBlock := taskViewStart + "\n" + strings.TrimSpace(taskBody) + "\n" + taskViewEnd
	block := managedBlock + "\n\n" + taskBlock
	if !parsed.Managed {
		if strings.TrimSpace(original) == "" {
			return block + "\n"
		}
		return block + "\n\n" + original
	}
	start := strings.Index(original, managedStart)
	endOffset := strings.Index(original[start+len(managedStart):], managedEnd)
	end := start + len(managedStart) + endOffset + len(managedEnd)
	return original[:start] + block + original[end:]
}
