package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/berkayahi/agentbridge/internal/buildinfo"
	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/telegram"
)

type pairer interface {
	Pair(context.Context) (telegram.Pairing, string, error)
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithPairer(args, stdout, stderr, nil)
}

func runWithPairer(args []string, stdout, stderr io.Writer, pairing pairer) int {
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
	if len(args) == 4 && args[0] == "pair" && args[1] == "telegram" && args[2] == "--config" && strings.TrimSpace(args[3]) != "" {
		if pairing == nil {
			fmt.Fprintln(stderr, "agentbridge: Telegram transport is not configured; install the live adapter first")
			return 1
		}
		result, _, err := pairing.Pair(context.Background())
		if err != nil {
			fmt.Fprintln(stderr, "agentbridge: Telegram pairing failed")
			return 1
		}
		if _, err := fmt.Fprintf(stdout, "telegram_user_id: %d\ntelegram_chat_id: %d\n", result.UserID, result.ChatID); err != nil {
			fmt.Fprintln(stderr, "agentbridge: failed to write pairing output")
			return 1
		}
		return 0
	}

	fmt.Fprintln(stderr, "usage: agentbridge version | agentbridge doctor --config <path> | agentbridge pair telegram --config <path>")
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
