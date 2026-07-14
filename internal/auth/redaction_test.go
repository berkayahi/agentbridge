package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	storesqlite "github.com/berkayahi/agentbridge/internal/store/sqlite"
	"github.com/berkayahi/agentbridge/internal/task"
)

func TestProviderSecretsStayOutOfDurableAndOrdinarySurfaces(t *testing.T) {
	t.Parallel()
	const (
		code  = "ZXCV-ASDF"
		token = "recognizable-oauth-token"
		url   = "https://auth.example/device/private"
	)
	var logs bytes.Buffer
	tasks := &fakeTaskStore{tasks: []task.Task{{ID: "task-1", Provider: task.ProviderCodex, State: task.Running}}}
	incidents := &fakeIncidents{}
	notifier := &fakeNotifier{}
	svc, err := NewService(Options{
		Commands: &fakeCommands{responses: []commandResponse{{output: []byte("401 " + token + " " + url + " " + code), err: errors.New("exit 1")}}},
		Tasks:    tasks, Incidents: incidents, Notifier: notifier, Resumer: &fakeResumer{},
		PTY: &fakePTY{release: closedChannel()}, Authorizer: fakeAuthorizer{"operator": true},
		Logger: slog.New(slog.NewTextHandler(&logs, nil)), NewID: sequenceIDs("event", "incident"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	incident, err := svc.CheckProvider(context.Background(), task.ProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryDashboardJSON := fmt.Sprintf(`{"provider":%q,"kind":%q,"affected":%d}`, incident.Provider, incident.Kind, len(incident.TaskIDs))
	telegramText := notifier.text()
	durableEvents := tasks.text() + incidents.text()
	for _, secret := range []string{code, token, url} {
		assertNoSecret(t, secret, logs.String(), ordinaryDashboardJSON, telegramText, durableEvents)
	}
	if strings.Contains(ordinaryDashboardJSON, "Message") {
		t.Fatalf("ordinary view includes provider output: %s", ordinaryDashboardJSON)
	}
}

func TestAuthCommandSecretsStayOutOfSQLiteEvents(t *testing.T) {
	t.Parallel()
	const secret = "sqlite-oauth-secret"
	ctx := context.Background()
	db, err := storesqlite.Open(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	value := task.Task{ID: "task-1", RepoProfileID: "repo", Title: "task", Prompt: "safe", State: task.Running, Provider: task.ProviderCodex, CreatedAt: now, UpdatedAt: now}
	initial := task.Event{ID: "created", TaskID: value.ID, Type: task.EventTaskCreated, Visibility: task.VisibilityUser, Payload: []byte(`{"message":"safe"}`), CreatedAt: now}
	if err := db.CreateTask(ctx, value, initial); err != nil {
		t.Fatal(err)
	}
	incidentStore := NewDurableIncidentStore(db)
	svc, err := NewService(Options{
		Commands: &fakeCommands{responses: []commandResponse{{output: []byte("401 " + secret + " https://auth.openai.com/device CODE-1234"), err: errors.New("exit 1")}}},
		Tasks:    db, Incidents: incidentStore, Notifier: &fakeNotifier{}, Resumer: &fakeResumer{},
		PTY: &fakePTY{release: closedChannel()}, Authorizer: fakeAuthorizer{"operator": true},
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), Now: func() time.Time { return now }, NewID: sequenceIDs("auth-event", "incident"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if _, err := svc.CheckProvider(ctx, task.ProviderCodex); err != nil {
		t.Fatal(err)
	}
	events, err := db.Events(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, secret, fmt.Sprint(events))
	assertNoSecret(t, "https://auth.openai.com/device", fmt.Sprint(events))
	assertNoSecret(t, "CODE-1234", fmt.Sprint(events))
	persisted, err := incidentStore.OpenIncident(ctx, task.ProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, secret, fmt.Sprint(persisted))
	assertNoSecret(t, "https://auth.openai.com/device", fmt.Sprint(persisted))
	assertNoSecret(t, "CODE-1234", fmt.Sprint(persisted))
}
