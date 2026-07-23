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
	identityPath := flags.String("identity-path", "", "existing owner-only device key path for same-device readiness")
	recordPath := flags.String("record-path", "", "owner-only enrollment record path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	result, err := operations.RestoreCheck(ctx, operations.RestoreCheckOptions{Backup: strings.TrimSpace(*backup), WorkDir: strings.TrimSpace(*workDir), IdentityPath: strings.TrimSpace(*identityPath), RecordPath: strings.TrimSpace(*recordPath)})
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: restore check failed")
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "Restore check passed: %s\nRestored: %s\nMode: %s\nManaged ready: %t\nRe-enrollment required: %t\n", result.Backup, result.Restored, result.Mode, result.ManagedReady, result.ReEnrollmentRequired); err != nil {
		return 1
	}
	return 0
}
