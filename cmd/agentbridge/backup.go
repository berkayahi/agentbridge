package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/berkayahi/agentbridge/internal/operations"
)

func runBackupCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	database := flags.String("database", "", "v2 database path")
	output := flags.String("output", "", "backup directory")
	identityPath := flags.String("identity-path", "", "owner-only device key path")
	recordPath := flags.String("record-path", "", "owner-only enrollment record path")
	modePath := flags.String("mode-path", "", "durable mode state path")
	managedStatePath := flags.String("managed-state-path", "", "durable managed cursor/inbox state path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	databasePath := strings.TrimSpace(*database)
	dataDir := strings.TrimSpace(os.Getenv("AGENTBRIDGE_DATA_DIR"))
	if dataDir == "" && databasePath != "" {
		dataDir = filepath.Dir(databasePath)
	}
	if strings.TrimSpace(*identityPath) == "" {
		*identityPath = filepath.Join(dataDir, "device-key.json")
	}
	if strings.TrimSpace(*recordPath) == "" {
		*recordPath = filepath.Join(dataDir, "enrollment.json")
	}
	if strings.TrimSpace(*modePath) == "" {
		*modePath = filepath.Join(dataDir, "mode.json")
	}
	if strings.TrimSpace(*managedStatePath) == "" {
		*managedStatePath = filepath.Join(dataDir, "managed-state.json")
	}
	result, err := operations.Backup(ctx, operations.BackupOptions{Database: databasePath, Output: strings.TrimSpace(*output), IdentityPath: strings.TrimSpace(*identityPath), RecordPath: strings.TrimSpace(*recordPath), ModePath: strings.TrimSpace(*modePath), ManagedStatePath: strings.TrimSpace(*managedStatePath)})
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: backup failed")
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "Created verified backup: %s\nManifest: %s\n", result.Database, result.Manifest); err != nil {
		return 1
	}
	return 0
}
