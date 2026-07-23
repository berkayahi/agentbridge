package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/berkayahi/agentbridge/internal/operations"
)

func runRestoreCheckCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("restore-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	backup := flags.String("backup", "", "backup database or directory")
	workDir := flags.String("work-dir", "", "isolated restore work directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	result, err := operations.RestoreCheck(ctx, operations.RestoreCheckOptions{Backup: strings.TrimSpace(*backup), WorkDir: strings.TrimSpace(*workDir)})
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: restore check failed")
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "Restore check passed: %s\nRestored: %s\n", result.Backup, result.Restored); err != nil {
		return 1
	}
	return 0
}
