package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

func TestConnectionPragmas(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "pragmas.db"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer db.Close()

	conn, err := db.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn(): %v", err)
	}
	defer conn.Close()

	checks := []struct {
		pragma string
		want   any
	}{
		{pragma: "journal_mode", want: "wal"},
		{pragma: "foreign_keys", want: 1},
		{pragma: "busy_timeout", want: busyTimeoutMillis},
	}
	for _, check := range checks {
		t.Run(check.pragma, func(t *testing.T) {
			switch want := check.want.(type) {
			case string:
				var got string
				if err := conn.QueryRowContext(context.Background(), "PRAGMA "+check.pragma).Scan(&got); err != nil || got != want {
					t.Fatalf("PRAGMA %s = %q, %v; want %q", check.pragma, got, err, want)
				}
			case int:
				var got int
				if err := conn.QueryRowContext(context.Background(), "PRAGMA "+check.pragma).Scan(&got); err != nil || got != want {
					t.Fatalf("PRAGMA %s = %d, %v; want %d", check.pragma, got, err, want)
				}
			}
		})
	}
}
