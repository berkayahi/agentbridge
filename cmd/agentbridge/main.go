package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/berkayahi/agentbridge/internal/buildinfo"
	"github.com/berkayahi/agentbridge/internal/config"
)

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		if _, err := fmt.Fprintf(stdout, "agentbridge %s (commit %s, built %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date); err != nil {
			fmt.Fprintln(stderr, "agentbridge: failed to write version output")
			return 1
		}
		return 0
	}
	if len(args) == 3 && args[0] == "doctor" && args[1] == "--config" && strings.TrimSpace(args[2]) != "" {
		return runDoctor(args[2], stdout, stderr)
	}

	fmt.Fprintln(stderr, "usage: agentbridge version | agentbridge doctor --config <path>")
	return 2
}

func runDoctor(path string, stdout, stderr io.Writer) int {
	if err := config.RejectAPIKeyEnvironment(); err != nil {
		fmt.Fprintln(stderr, "agentbridge: invalid environment: API-key authentication is not supported")
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: invalid configuration: check the configuration file")
		return 1
	}

	profiles := make([]string, 0, len(cfg.Repositories))
	enabled := 0
	for name, profile := range cfg.Repositories {
		profiles = append(profiles, name)
		if profile.Delivery.Enabled {
			enabled++
		}
	}
	sort.Strings(profiles)
	if _, err := fmt.Fprintf(stdout, "configuration valid\nprofiles (%d): %s\ndelivery: %d enabled, %d disabled\n", len(profiles), strings.Join(profiles, ", "), enabled, len(profiles)-enabled); err != nil {
		fmt.Fprintln(stderr, "agentbridge: failed to write doctor output")
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
