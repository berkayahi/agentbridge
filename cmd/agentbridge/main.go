package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/berkayahi/agentbridge/internal/buildinfo"
	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/controlsocket"
	"github.com/berkayahi/agentbridge/internal/mcpserver"
	"github.com/berkayahi/agentbridge/internal/provider/claude"
	"github.com/berkayahi/agentbridge/internal/telegram"
)

type pairAttempt interface {
	Nonce() string
	Wait(context.Context) (telegram.Pairing, error)
}

type pairer interface {
	Begin() (pairAttempt, error)
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithDepsAndPairer(context.Background(), args, os.Stdin, stdout, stderr, defaultCommandDeps(), nil)
}

func runWithPairer(args []string, stdout, stderr io.Writer, pairing pairer) int {
	return runWithDepsAndPairer(context.Background(), args, os.Stdin, stdout, stderr, defaultCommandDeps(), pairing)
}

type commandDeps struct {
	getenv         func(string) string
	readCapability func() ([]byte, error)
	newPairer      func(context.Context, string) (pairer, error)
	runMCP         func(context.Context, mcpserver.RunOptions) error
	runStatusline  func(context.Context, io.Reader, claude.StatuslineCaller, claude.StatuslineScope, func() time.Time) error
	runServe       func(context.Context, string) error
	runMigrate     func(context.Context, string) error
}

func defaultCommandDeps() commandDeps {
	return commandDeps{
		getenv:    os.Getenv,
		newPairer: newTelegramPairer,
		readCapability: func() ([]byte, error) {
			fd := 3
			if value := os.Getenv("AGENTBRIDGE_CAPABILITY_FD"); value != "" {
				parsed, err := strconv.Atoi(value)
				if err != nil || parsed < 3 {
					return nil, fmt.Errorf("invalid capability descriptor")
				}
				fd = parsed
			}
			return mcpserver.ReadCapability(fd, nil, nil)
		},
		runMCP:        mcpserver.Run,
		runStatusline: claude.CaptureStatusline,
		runServe:      serveDaemon,
		runMigrate:    runMigrate,
	}
}

func runWithDeps(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps commandDeps) int {
	return runWithDepsAndPairer(ctx, args, stdin, stdout, stderr, deps, nil)
}

func runWithDepsAndPairer(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps commandDeps, pairing pairer) int {
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
			if deps.newPairer == nil {
				fmt.Fprintln(stderr, "agentbridge: Telegram pairing unavailable")
				return 1
			}
			var err error
			pairing, err = deps.newPairer(ctx, args[3])
			if err != nil {
				fmt.Fprintln(stderr, "agentbridge: Telegram pairing unavailable")
				return 1
			}
		}
		attempt, err := pairing.Begin()
		if err != nil {
			fmt.Fprintln(stderr, "agentbridge: Telegram pairing failed")
			return 1
		}
		if _, err := fmt.Fprintf(stdout, "send_to_bot: /pair %s\n", attempt.Nonce()); err != nil {
			fmt.Fprintln(stderr, "agentbridge: failed to write pairing instruction")
			return 1
		}
		result, err := attempt.Wait(ctx)
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
	if len(args) == 1 && args[0] == "mcp" {
		return runMCPCommand(ctx, stdin, stdout, stderr, deps)
	}
	if len(args) == 1 && args[0] == "claude-statusline" {
		return runClaudeStatusline(ctx, stdin, stderr, deps)
	}
	if len(args) == 3 && args[0] == "serve" && args[1] == "--config" && strings.TrimSpace(args[2]) != "" {
		if deps.runServe == nil {
			fmt.Fprintln(stderr, "agentbridge: daemon runtime is unavailable")
			return 1
		}
		if err := deps.runServe(ctx, args[2]); err != nil {
			fmt.Fprintln(stderr, "agentbridge: daemon stopped; inspect the service journal")
			return 1
		}
		return 0
	}
	if len(args) == 3 && args[0] == "migrate" && args[1] == "--database" && strings.TrimSpace(args[2]) != "" {
		if deps.runMigrate == nil {
			fmt.Fprintln(stderr, "agentbridge: migration is unavailable")
			return 1
		}
		if err := deps.runMigrate(ctx, args[2]); err != nil {
			fmt.Fprintln(stderr, "agentbridge: migration failed; database was not activated")
			return 1
		}
		return 0
	}

	fmt.Fprintln(stderr, "usage: agentbridge version | agentbridge doctor --config <path> | agentbridge pair telegram --config <path> | agentbridge serve --config <path> | agentbridge migrate --database <path> | agentbridge mcp | agentbridge claude-statusline")
	return 2
}

func runClaudeStatusline(ctx context.Context, stdin io.Reader, stderr io.Writer, deps commandDeps) int {
	if deps.getenv == nil || deps.readCapability == nil || deps.runStatusline == nil {
		fmt.Fprintln(stderr, "agentbridge: status-line dependencies unavailable")
		return 1
	}
	socketPath := strings.TrimSpace(deps.getenv("AGENTBRIDGE_CONTROL_SOCKET"))
	taskID := strings.TrimSpace(deps.getenv("AGENTBRIDGE_TASK_ID"))
	if socketPath == "" || taskID == "" {
		fmt.Fprintln(stderr, "agentbridge: incomplete status-line task scope")
		return 1
	}
	capability, err := deps.readCapability()
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: status-line capability unavailable")
		return 1
	}
	scope := claude.StatuslineScope{TaskID: taskID, Provider: "claude", Capability: capability}
	if err := deps.runStatusline(ctx, stdin, controlsocket.Client{Path: socketPath}, scope, time.Now); err != nil {
		fmt.Fprintln(stderr, "agentbridge: status-line capture failed")
		return 1
	}
	return 0
}

func runMCPCommand(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, deps commandDeps) int {
	if deps.getenv == nil || deps.readCapability == nil || deps.runMCP == nil {
		fmt.Fprintln(stderr, "agentbridge: MCP dependencies unavailable")
		return 1
	}
	socketPath := strings.TrimSpace(deps.getenv("AGENTBRIDGE_CONTROL_SOCKET"))
	taskID := strings.TrimSpace(deps.getenv("AGENTBRIDGE_TASK_ID"))
	providerName := strings.TrimSpace(deps.getenv("AGENTBRIDGE_PROVIDER"))
	if socketPath == "" || taskID == "" || providerName == "" {
		fmt.Fprintln(stderr, "agentbridge: incomplete MCP task scope")
		return 1
	}
	capability, err := deps.readCapability()
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: MCP capability unavailable")
		return 1
	}
	options := mcpserver.RunOptions{
		Caller: controlsocket.Client{Path: socketPath},
		Scope:  mcpserver.Scope{TaskID: taskID, Provider: providerName, Capability: capability},
		Input:  io.NopCloser(stdin), Output: nopWriteCloser{stdout},
	}
	if err := deps.runMCP(ctx, options); err != nil {
		fmt.Fprintln(stderr, "agentbridge: MCP server stopped")
		return 1
	}
	return 0
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runWithDepsAndPairer(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, defaultCommandDeps(), nil))
}
