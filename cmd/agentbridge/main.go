package main

import (
	"fmt"
	"io"
	"os"

	"github.com/berkayahi/agentbridge/internal/buildinfo"
)

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		if _, err := fmt.Fprintf(stdout, "agentbridge %s (commit %s, built %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date); err != nil {
			fmt.Fprintln(stderr, "agentbridge: failed to write version output")
			return 1
		}
		return 0
	}

	fmt.Fprintln(stderr, "usage: agentbridge version")
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
