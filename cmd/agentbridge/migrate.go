package main

import (
	"context"

	"github.com/berkayahi/agentbridge/internal/buildinfo"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

func runMigrate(ctx context.Context, databasePath string) error {
	_, err := sqlite.Cutover(ctx, databasePath, buildinfo.Version)
	return err
}
