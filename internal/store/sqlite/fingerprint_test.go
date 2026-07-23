package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestFingerprintChangesWithSchemaAndIsDeterministic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE records (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	first, err := SchemaFingerprint(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SchemaFingerprint(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("fingerprints = %q, %q", first, second)
	}
	if _, err := db.Exec("CREATE INDEX records_id_idx ON records(id)"); err != nil {
		t.Fatal(err)
	}
	changed, err := SchemaFingerprint(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("schema fingerprint did not change after index creation")
	}
}
