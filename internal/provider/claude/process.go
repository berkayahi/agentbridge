package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/egressguard"
	"github.com/berkayahi/agentbridge/internal/isolation"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/security"
)

const (
	defaultMaxStreamLine = 1 << 20
	processEventBuffer   = 256
	maxProcessStderr     = 64 * 1024
	maxCapabilityBytes   = 4 * 1024
)

type ProcessConfig struct {
	Executable      string
	MCPConfigPath   string
	ClaudeConfigDir string
	Model           string
	ControlSocket   string
	Capability      []byte
	Environment     []string
	Dir             string
	Isolation       *isolation.Policy
	EgressGuard     *egressguard.Guard
	ResumeSession   string
	InitialInput    provider.Input
	TaskID          provider.ID
	testArgs        []string
}

type ParsedEvent struct {
	SessionID string
	Event     provider.Event
	Paused    bool
}

type Runner interface {
	SessionID() string
	Events() <-chan provider.Event
	Send(context.Context, provider.Input) error
	Close() error
}

type Spawner interface {
	Spawn(context.Context, ProcessConfig) (Runner, error)
}

type OSSpawner struct{}

func (OSSpawner) Spawn(ctx context.Context, cfg ProcessConfig) (Runner, error) {
	return StartProcess(ctx, cfg)
}

func CommandArgs(mcpConfigPath, resumeSession, model string) []string {
	args := []string{
		"-p", "--verbose", "--input-format", "stream-json", "--output-format", "stream-json",
		"--permission-prompt-tool", "mcp__agentbridge__request_telegram_approval",
		"--mcp-config", mcpConfigPath,
		"--model", model,
	}
	if resumeSession != "" {
		args = append(args, "--resume", resumeSession)
	}
	return args
}

func ChildEnvironment(base []string, configDir, taskID, controlSocket string) []string {
	return isolation.FilterEnvironment(base, isolation.EnvironmentPolicy{Extra: map[string]string{
		"CLAUDE_CONFIG_DIR":          configDir,
		"AGENTBRIDGE_CONTROL_SOCKET": controlSocket,
		"AGENTBRIDGE_TASK_ID":        taskID,
		"AGENTBRIDGE_PROVIDER":       "claude",
	}})
}

func WriteMCPConfig(dir, executable string) (string, error) {
	if executable == "" {
		return "", errors.New("agentbridge executable is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mcp.json")
	config := map[string]any{"mcpServers": map[string]any{"agentbridge": map[string]any{
		"type": "stdio", "command": executable, "args": []string{"mcp"},
	}}}
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func InputLine(input provider.Input) ([]byte, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	text := input.Text
	if len(input.Attachments) > 0 {
		var paths strings.Builder
		paths.WriteString("\n\nInspect each local attachment with the appropriate tool before answering:\n")
		for _, attachment := range input.Attachments {
			fmt.Fprintf(&paths, "- %q\n", attachment.Path())
		}
		text += paths.String()
	}
	message := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}},
	}
	data, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan provider.Event
	ready  chan string
	done   chan struct{}
	taskID provider.ID

	sessionMu sync.RWMutex
	sessionID string
	writeMu   sync.Mutex
	closeOnce sync.Once
	stderrMu  sync.Mutex
	stderr    string
	egress    *egressguard.Guard
}

func StartProcess(ctx context.Context, cfg ProcessConfig) (*Process, error) {
	if cfg.Executable == "" {
		cfg.Executable = "claude"
	}
	if !cfg.TaskID.Valid() || cfg.Model == "" || !filepath.IsAbs(cfg.ControlSocket) || len(cfg.Capability) == 0 || len(cfg.Capability) > maxCapabilityBytes {
		return nil, errors.New("incomplete task-scoped Claude process configuration")
	}
	args := CommandArgs(cfg.MCPConfigPath, cfg.ResumeSession, cfg.Model)
	if cfg.testArgs != nil {
		args = cfg.testArgs
	}
	if cfg.Environment == nil {
		cfg.Environment = os.Environ()
	}
	cmd := exec.Command(cfg.Executable, args...)
	if cfg.Environment == nil {
		cfg.Environment = os.Environ()
	}
	cmd.Dir = cfg.Dir
	cmd.Env = ChildEnvironment(cfg.Environment, cfg.ClaudeConfigDir, cfg.TaskID.String(), cfg.ControlSocket)
	provider.ConfigureProcessGroup(cmd)
	if cfg.Isolation != nil {
		if err := isolation.PrepareCommand(cmd, *cfg.Isolation); err != nil {
			return nil, fmt.Errorf("Claude isolation: %w", err)
		}
	}
	capabilityFile, err := capabilityFile(cfg.Capability)
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{capabilityFile}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = capabilityFile.Close()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = capabilityFile.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = capabilityFile.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = capabilityFile.Close()
		return nil, fmt.Errorf("start Claude Code: %w", err)
	}
	if cfg.Isolation != nil {
		if err := isolation.ApplyStartedProcess(cmd.Process, *cfg.Isolation); err != nil {
			_ = provider.SweepProcessGroup(cmd.Process)
			_, _ = cmd.Process.Wait()
			return nil, fmt.Errorf("Claude isolation limits: %w", err)
		}
	}
	_ = capabilityFile.Close()
	p := &Process{cmd: cmd, stdin: stdin, events: make(chan provider.Event, processEventBuffer), ready: make(chan string, 1), done: make(chan struct{}), taskID: cfg.TaskID, egress: cfg.EgressGuard}
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Close()
		case <-p.done:
		}
	}()
	var parserWG, stderrWG sync.WaitGroup
	parserWG.Add(1)
	go func() {
		defer parserWG.Done()
		for parsed := range ParseStream(stdout, defaultMaxStreamLine) {
			if parsed.SessionID != "" {
				p.setSession(parsed.SessionID)
			}
			if parsed.Event.Type != "" {
				parsed.Event.TaskID = cfg.TaskID
				p.emit(parsed.Event)
			}
		}
	}()
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		data, _ := io.ReadAll(io.LimitReader(stderr, maxProcessStderr+1))
		p.stderrMu.Lock()
		p.stderr = security.NewRedactor(security.Config{MaxPayloadRunes: maxProcessStderr}).RedactString(string(data))
		if p.egress != nil {
			if guarded, _ := p.egress.Check(egressguard.ClassTerminalOutput, []byte(p.stderr)); guarded != nil {
				p.stderr = string(guarded)
			}
		}
		p.stderrMu.Unlock()
	}()
	go func() {
		err := cmd.Wait()
		_ = provider.SweepProcessGroup(cmd.Process)
		parserWG.Wait()
		stderrWG.Wait()
		if err != nil {
			message := p.Stderr()
			typeOfEvent := provider.EventError
			lower := strings.ToLower(message)
			if strings.Contains(lower, "login") || strings.Contains(lower, "oauth") || strings.Contains(lower, "authentication") {
				typeOfEvent = provider.EventAuthRequired
			}
			p.emit(provider.Event{TaskID: cfg.TaskID, Type: typeOfEvent, Message: message})
		}
		close(p.events)
		close(p.done)
	}()
	if err := p.Send(ctx, cfg.InitialInput); err != nil {
		_ = p.Close()
		return nil, err
	}
	select {
	case <-ctx.Done():
		_ = p.Close()
		return nil, ctx.Err()
	case <-p.done:
		return nil, errors.New("Claude exited before session initialization")
	case <-p.ready:
		return p, nil
	}
}

func capabilityFile(capability []byte) (*os.File, error) {
	file, err := os.CreateTemp("", ".agentbridge-capability-*")
	if err != nil {
		return nil, fmt.Errorf("create Claude capability file: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	if err := file.Chmod(0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("protect Claude capability file: %w", err)
	}
	if _, err := file.Write(capability); err != nil {
		cleanup()
		return nil, fmt.Errorf("write Claude capability file: %w", err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("sync Claude capability file: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, fmt.Errorf("rewind Claude capability file: %w", err)
	}
	if err := os.Remove(file.Name()); err != nil {
		cleanup()
		return nil, fmt.Errorf("unlink Claude capability file: %w", err)
	}
	return file, nil
}

func (p *Process) SessionID() string {
	p.sessionMu.RLock()
	defer p.sessionMu.RUnlock()
	return p.sessionID
}
func (p *Process) Events() <-chan provider.Event { return p.events }
func (p *Process) Stderr() string                { p.stderrMu.Lock(); defer p.stderrMu.Unlock(); return p.stderr }

func (p *Process) Send(ctx context.Context, input provider.Input) error {
	line, err := InputLine(input)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if _, err := p.stdin.Write(line); err != nil {
		return fmt.Errorf("write Claude input: %w", err)
	}
	return nil
}

func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		_ = provider.StopProcessGroup(p.cmd.Process, p.done, 5*time.Second)
	})
	return nil
}

func (p *Process) setSession(sessionID string) {
	p.sessionMu.Lock()
	first := p.sessionID == ""
	if first {
		p.sessionID = sessionID
	}
	p.sessionMu.Unlock()
	if first {
		p.ready <- sessionID
	}
}

func (p *Process) emit(event provider.Event) {
	if p.egress != nil {
		if guarded, err := p.egress.Check(egressguard.ClassStructuredMessage, []byte(event.Message)); guarded != nil {
			event.Message = string(guarded)
			if err != nil && event.Path != "" {
				event.Path = "[QUARANTINED]"
			}
		}
		if event.Path != "" {
			if guarded, err := p.egress.Check(egressguard.ClassStructuredMessage, []byte(event.Path)); guarded != nil {
				event.Path = string(guarded)
				if err != nil {
					event.Path = "[QUARANTINED]"
				}
			}
		}
	}
	select {
	case p.events <- event:
	default:
	}
}

func ParseStream(reader io.Reader, maxLine int) <-chan ParsedEvent {
	output := make(chan ParsedEvent, 32)
	go func() {
		defer close(output)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), maxLine)
		for scanner.Scan() {
			for _, event := range parseLine(scanner.Bytes()) {
				output <- event
			}
		}
		if scanner.Err() != nil {
			output <- ParsedEvent{Event: provider.Event{Type: provider.EventError, Message: "Claude stream line exceeded limit"}}
		}
	}()
	return output
}

func parseLine(line []byte) []ParsedEvent {
	var envelope struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		Result    string `json:"result"`
		IsError   bool   `json:"is_error"`
		Message   struct {
			Content []struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				Name      string `json:"name"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"message"`
		RateLimit struct {
			Status   string `json:"status"`
			ResetsAt string `json:"resets_at"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return []ParsedEvent{{Event: provider.Event{Type: provider.EventError, Message: "malformed Claude stream event"}}}
	}
	base := ParsedEvent{SessionID: envelope.SessionID}
	var events []ParsedEvent
	switch envelope.Type {
	case "system":
		if envelope.SessionID != "" {
			events = append(events, base)
		}
	case "assistant":
		for _, content := range envelope.Message.Content {
			event := base
			switch content.Type {
			case "text":
				event.Event = provider.Event{Type: provider.EventAssistantMessage, Message: content.Text}
			case "tool_use":
				event.Event = provider.Event{Type: provider.EventToolStarted, Tool: content.Name}
			default:
				continue
			}
			events = append(events, event)
		}
	case "user":
		for _, content := range envelope.Message.Content {
			if content.Type == "tool_result" {
				event := base
				event.Event = provider.Event{Type: provider.EventToolEnded, Tool: content.ToolUseID}
				events = append(events, event)
			}
		}
	case "rate_limit_event":
		event := base
		event.Event = provider.Event{Type: provider.EventRateLimited, Message: envelope.RateLimit.Status}
		if reset, err := time.Parse(time.RFC3339, envelope.RateLimit.ResetsAt); err == nil {
			reset = reset.UTC()
			event.Event.ResetAt = &reset
		}
		events = append(events, event)
	case "result":
		event := base
		lower := strings.ToLower(envelope.Subtype + " " + envelope.Result)
		switch {
		case strings.Contains(lower, "max_usage") || strings.Contains(lower, "monthly limit") || strings.Contains(lower, "allowance exhausted"):
			event.Event = provider.Event{Type: provider.EventRateLimited, Message: envelope.Result}
			event.Paused = true
		case envelope.IsError && (strings.Contains(lower, "login") || strings.Contains(lower, "auth")):
			event.Event = provider.Event{Type: provider.EventAuthRequired, Message: envelope.Result}
		case envelope.IsError:
			event.Event = provider.Event{Type: provider.EventError, Message: envelope.Result}
		default:
			event.Event = provider.Event{Type: provider.EventCompleted, Message: envelope.Result}
		}
		events = append(events, event)
	}
	return events
}
