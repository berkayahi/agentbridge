package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPreflightRecognizesPublicLineageAndRejectsUnknownMigration(t *testing.T) {
	path := migrationFixture(t, "public_v1.db")
	report, err := Preflight(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Lineage != LineagePublicV1 || report.StructuralFingerprint == "" {
		t.Fatalf("report = %#v", report)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE schema_migrations SET name = 'unknown.sql' WHERE version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Preflight(context.Background(), path); !errors.Is(err, ErrUnknownLineage) {
		t.Fatalf("unknown migration error = %v", err)
	}
}

func TestPreflightRecognizesGoldenMigrationFixtures(t *testing.T) {
	tests := []struct {
		name    string
		lineage Lineage
		err     error
	}{
		{name: "public_v1.db", lineage: LineagePublicV1},
		{name: "adopted_attachment_sha256.db", lineage: LineagePublicV1},
		{name: "donor_v1.db", lineage: LineageDonor},
		{name: "empty.db", lineage: LineageEmpty},
		{name: "unknown_migration.db", err: ErrUnknownLineage},
		{name: "corrupt.db", err: ErrCorruptDatabase},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, err := Preflight(context.Background(), migrationFixture(t, test.name))
			if test.err != nil {
				if !errors.Is(err, test.err) {
					t.Fatalf("Preflight() error = %v, want %v", err, test.err)
				}
				return
			}
			if err != nil || report.Lineage != test.lineage {
				t.Fatalf("Preflight() report=%#v error=%v, want lineage %q", report, err, test.lineage)
			}
		})
	}
}

func TestOpenV2LegacyRequiresExplicitMigration(t *testing.T) {
	path := migrationFixture(t, "public_v1.db")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if db, err := OpenV2(context.Background(), path); !errors.Is(err, ErrMigrationRequired) {
		if db != nil {
			_ = db.Close()
		}
		t.Fatalf("OpenV2(legacy) error = %v", err)
	}

	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var legacyTasks int
	if err := check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tasks'").Scan(&legacyTasks); err != nil {
		t.Fatal(err)
	}
	if legacyTasks != 1 {
		t.Fatalf("legacy tasks table count = %d", legacyTasks)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("legacy database changed after rejected v2 open")
	}
}

func TestPreflightRejectsLegacySchemaDrift(t *testing.T) {
	path := migrationFixture(t, "public_v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ALTER TABLE tasks ADD COLUMN unexpected TEXT NOT NULL DEFAULT ''"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Preflight(context.Background(), path); !errors.Is(err, ErrUnknownLineage) {
		t.Fatalf("drifted legacy database error = %v, want ErrUnknownLineage", err)
	}
}

func TestPreflightRejectsLegacyLedgerGap(t *testing.T) {
	path := migrationFixture(t, "public_v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM schema_migrations WHERE version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Preflight(context.Background(), path); !errors.Is(err, ErrUnknownLineage) {
		t.Fatalf("gapped legacy database error = %v, want ErrUnknownLineage", err)
	}
}

func TestPreflightRecognizesAdoptedAttachmentChecksumLineage(t *testing.T) {
	path := migrationFixture(t, "adopted_attachment_sha256.db")
	report, err := Preflight(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Lineage != LineagePublicV1 {
		t.Fatalf("adopted checksum report = %#v", report)
	}
}

func TestPreflightRejectsCorruptSQLite(t *testing.T) {
	path := migrationFixture(t, "corrupt.db")

	if _, err := Preflight(context.Background(), path); !errors.Is(err, ErrCorruptDatabase) {
		t.Fatalf("corrupt database error = %v, want ErrCorruptDatabase", err)
	}
}

func migrationFixture(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", "migration", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
