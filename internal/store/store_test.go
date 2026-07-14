package store_test

import (
	"context"

	"github.com/berkayahi/agentbridge/internal/store"
)

// compileStorePort keeps recovery and related-record operations available to
// consumers without depending on the SQLite implementation.
func compileStorePort(ctx context.Context, db store.Store) {
	_, _ = db.Events(ctx, "task")
	_, _ = db.NonterminalTasks(ctx)
	_, _ = db.ExpiredLeases(ctx)
	_, _ = db.PendingApprovals(ctx)
	_, _ = db.ResumableSessions(ctx)
	_, _ = db.Attachments(ctx, "task")
}
