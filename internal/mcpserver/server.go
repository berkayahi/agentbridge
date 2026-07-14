// Package mcpserver exposes the deliberately small agent permission surface.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/controlsocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxSummaryBytes        = 4 * 1024
	maxMessageBytes        = 16 * 1024
	maxArtifactBytes       = 4 * 1024
	maxCapabilityBytes     = 4 * 1024
	testCapabilityEnv      = "AGENTBRIDGE_TEST_CAPABILITY"
	testCapabilityOptInEnv = "AGENTBRIDGE_ALLOW_TEST_CAPABILITY_ENV"
)

type Caller interface {
	Call(context.Context, controlsocket.Request, any) error
}

type Scope struct {
	TaskID          string
	Provider        string
	Capability      []byte
	ApprovalTimeout time.Duration
}

type ApprovalInput struct {
	Kind    string `json:"kind" jsonschema:"approval category"`
	Summary string `json:"summary" jsonschema:"visible operation summary"`
}

type ApprovalOutput struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}

type NotifyInput struct {
	Message string `json:"message" jsonschema:"message to send to the task owner"`
}

type NotifyOutput struct {
	Sent bool `json:"sent"`
}

type ArtifactInput struct {
	Path string `json:"path" jsonschema:"absolute path to a task artifact"`
	Name string `json:"name,omitempty" jsonschema:"display name"`
}

type ArtifactOutput struct {
	Sent bool `json:"sent"`
}

type ContextInput struct{}

type ContextOutput struct {
	TaskID     string `json:"task_id"`
	Repository string `json:"repository,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

func New(caller Caller, scope Scope) *mcp.Server {
	if scope.ApprovalTimeout <= 0 {
		scope.ApprovalTimeout = 10 * time.Minute
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "agentbridge", Version: "dev"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "request_telegram_approval", Description: "Request a task-scoped decision from the Telegram operator."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input ApprovalInput) (*mcp.CallToolResult, ApprovalOutput, error) {
			if input.Kind == "" || input.Summary == "" || len(input.Kind) > 128 || len(input.Summary) > maxSummaryBytes {
				return nil, ApprovalOutput{}, errors.New("invalid bounded approval request")
			}
			approvalCtx, cancel := context.WithTimeout(ctx, scope.ApprovalTimeout)
			defer cancel()
			var output ApprovalOutput
			err := call(approvalCtx, caller, scope, "request_telegram_approval", input, &output)
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return nil, ApprovalOutput{Approved: false, Reason: "approval timed out"}, nil
			}
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "notify_telegram", Description: "Send a bounded task update to the Telegram operator."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input NotifyInput) (*mcp.CallToolResult, NotifyOutput, error) {
			if input.Message == "" || len(input.Message) > maxMessageBytes {
				return nil, NotifyOutput{}, errors.New("invalid bounded notification")
			}
			var output NotifyOutput
			return nil, output, call(ctx, caller, scope, "notify_telegram", input, &output)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "send_artifact", Description: "Send a task-owned artifact to the Telegram operator."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input ArtifactInput) (*mcp.CallToolResult, ArtifactOutput, error) {
			if !filepath.IsAbs(input.Path) || len(input.Path)+len(input.Name) > maxArtifactBytes {
				return nil, ArtifactOutput{}, errors.New("invalid bounded artifact")
			}
			var output ArtifactOutput
			return nil, output, call(ctx, caller, scope, "send_artifact", input, &output)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "get_task_context", Description: "Read the current task's safe execution context."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input ContextInput) (*mcp.CallToolResult, ContextOutput, error) {
			var output ContextOutput
			return nil, output, call(ctx, caller, scope, "get_task_context", input, &output)
		})
	return server
}

func call(ctx context.Context, caller Caller, scope Scope, tool string, input, output any) error {
	params, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request := controlsocket.Request{
		TaskID: scope.TaskID, Provider: scope.Provider, Capability: append([]byte(nil), scope.Capability...),
		Tool: tool, Params: params,
	}
	return caller.Call(ctx, request, output)
}

type RunOptions struct {
	Caller Caller
	Scope  Scope
	Input  io.ReadCloser
	Output io.WriteCloser
}

func Run(ctx context.Context, options RunOptions) error {
	if options.Caller == nil || options.Input == nil || options.Output == nil {
		return errors.New("incomplete MCP stdio configuration")
	}
	server := New(options.Caller, options.Scope)
	return server.Run(ctx, &mcp.IOTransport{Reader: options.Input, Writer: options.Output})
}

func ReadCapability(fd int, readFD func(int) ([]byte, error), getenv func(string) string) ([]byte, error) {
	if readFD == nil {
		readFD = func(fd int) ([]byte, error) {
			file := os.NewFile(uintptr(fd), "agentbridge-capability")
			if file == nil {
				return nil, errors.New("invalid capability fd")
			}
			// Do not consume the shared open-file offset. Claude may spawn MCP and
			// statusline children from the same inherited regular-file descriptor.
			return io.ReadAll(io.NewSectionReader(file, 0, maxCapabilityBytes+1))
		}
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	value, err := readFD(fd)
	value = bytes.TrimSpace(value)
	if err == nil && len(value) > 0 && len(value) <= maxCapabilityBytes {
		return append([]byte(nil), value...), nil
	}
	if getenv(testCapabilityOptInEnv) == "1" {
		value = []byte(strings.TrimSpace(getenv(testCapabilityEnv)))
		if len(value) > 0 && len(value) <= maxCapabilityBytes {
			return value, nil
		}
	}
	return nil, fmt.Errorf("task capability unavailable")
}
