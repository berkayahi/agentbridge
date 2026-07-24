package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestMigrationLedgerRecordsV2SchemaVerification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	store, err := OpenV2(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version int
	var name, checksum, fingerprint string
	if err := check.QueryRow("SELECT version, name, checksum, structural_fingerprint FROM migration_ledger WHERE version = ?", executionKernelVersion).Scan(&version, &name, &checksum, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if version != executionKernelVersion || name != executionKernelName || checksum == "" || fingerprint == "" {
		t.Fatalf("ledger = version=%d name=%q checksum=%q fingerprint=%q", version, name, checksum, fingerprint)
	}
}

func TestOpenV2RejectsTamperedMigrationLedger(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "checksum", query: "UPDATE migration_ledger SET checksum = 'tampered'"},
		{name: "applied time", query: "UPDATE migration_ledger SET applied_at = 'not-a-timestamp'"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v2.db")
			store, err := OpenV2(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			check, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := check.Exec(test.query); err != nil {
				_ = check.Close()
				t.Fatal(err)
			}
			if err := check.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := OpenV2(context.Background(), path); !errors.Is(err, ErrUnknownLineage) {
				t.Fatalf("OpenV2() error = %v, want ErrUnknownLineage", err)
			}
		})
	}
}
