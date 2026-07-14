package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/buildinfo"
	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/controlsocket"
	"github.com/berkayahi/agentbridge/internal/mcpserver"
	"github.com/berkayahi/agentbridge/internal/provider/claude"
)

func run(args []string, stdout, stderr io.Writer) int {
	return runWithDeps(context.Background(), args, os.Stdin, stdout, stderr, defaultCommandDeps())
}

type commandDeps struct {
	getenv         func(string) string
	readCapability func() ([]byte, error)
	runMCP         func(context.Context, mcpserver.RunOptions) error
	runStatusline  func(context.Context, io.Reader, claude.StatuslineCaller, claude.StatuslineScope, func() time.Time) error
}

func defaultCommandDeps() commandDeps {
	return commandDeps{
		getenv: os.Getenv,
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
	}
}

func runWithDeps(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps commandDeps) int {
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
	if len(args) == 1 && args[0] == "mcp" {
		return runMCPCommand(ctx, stdin, stdout, stderr, deps)
	}
	if len(args) == 1 && args[0] == "claude-statusline" {
		return runClaudeStatusline(ctx, stdin, stderr, deps)
	}

	fmt.Fprintln(stderr, "usage: agentbridge version | agentbridge doctor --config <path> | agentbridge mcp | agentbridge claude-statusline")
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
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
