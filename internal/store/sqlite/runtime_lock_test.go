package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestV2RuntimeLockBlocksSecondOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentbridge.db")
	data, err := OpenV2(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}

	release, err := AcquireDatabaseRuntimeLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := OpenV2(ctx, path); !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("second OpenV2() error = %v, want ErrDatabaseInUse", err)
	}
}
