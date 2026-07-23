package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	storesqlite "github.com/berkayahi/agentbridge/internal/store/sqlite"
)

func TestMigrateCommandRegistration(t *testing.T) {
	var gotPath string
	deps := commandDeps{runMigrate: func(ctx context.Context, path string) error {
		if ctx == nil {
			t.Fatal("migrate context is nil")
		}
		gotPath = path
		return nil
	}}
	var stdout, stderr bytes.Buffer
	code := runWithDeps(context.Background(), []string{"migrate", "--database", "/private/agentbridge.db"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != 0 || gotPath != "/private/agentbridge.db" || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d path=%q stdout=%q stderr=%q", code, gotPath, stdout.String(), stderr.String())
	}
}

func TestMigrateCommandReportsConciseFailure(t *testing.T) {
	deps := commandDeps{runMigrate: func(context.Context, string) error { return errors.New("open /private/agentbridge.db: locked") }}
	var stdout, stderr bytes.Buffer
	code := runWithDeps(context.Background(), []string{"migrate", "--database", "/private/agentbridge.db"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), "/private/") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestMigrateCommandRefusesRunningDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentbridge.db")
	release, err := storesqlite.AcquireDatabaseRuntimeLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := runMigrate(context.Background(), path); !errors.Is(err, storesqlite.ErrDatabaseInUse) {
		t.Fatalf("runMigrate() error = %v, want ErrDatabaseInUse", err)
	}
}
