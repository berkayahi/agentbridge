package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/berkayahi/agentbridge/internal/operations"
)

func runDatabaseDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	database := flags.String("database", "", "v2 database path")
	jsonOutput := flags.Bool("json", false, "write a machine-readable report")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if !*jsonOutput {
		fmt.Fprintln(stderr, "agentbridge: database doctor requires --json")
		return 2
	}
	report, err := operations.Doctor(ctx, operations.DoctorOptions{Database: strings.TrimSpace(*database)})
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: database doctor failed")
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		return 1
	}
	return 0
}
