package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/berkayahi/agentbridge/internal/operations"
)

func runBackupCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	database := flags.String("database", "", "v2 database path")
	output := flags.String("output", "", "backup directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	result, err := operations.Backup(ctx, operations.BackupOptions{Database: strings.TrimSpace(*database), Output: strings.TrimSpace(*output)})
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: backup failed")
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "Created verified backup: %s\nManifest: %s\n", result.Database, result.Manifest); err != nil {
		return 1
	}
	return 0
}
