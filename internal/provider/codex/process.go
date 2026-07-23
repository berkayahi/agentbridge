package codex

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/egressguard"
	"github.com/berkayahi/agentbridge/internal/isolation"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/security"
)

const maxStderrBytes = 64 * 1024

type ProcessConfig struct {
	Executable  string
	Args        []string
	Env         []string
	Dir         string
	Isolation   *isolation.Policy
	EgressGuard *egressguard.Guard
}

type Process struct {
	Client *Client
	cmd    *exec.Cmd
	stdin  io.WriteCloser

	stderrMu  sync.Mutex
	stderr    bytes.Buffer
	wait      chan struct{}
	closeOnce sync.Once
	closeErr  error
	egress    *egressguard.Guard
}

func AppServerArgs() []string { return []string{"app-server", "--listen", "stdio://"} }

func StartAppServer(ctx context.Context, executable string, env []string) (*Process, error) {
	return StartProcess(ctx, ProcessConfig{Executable: executable, Args: AppServerArgs(), Env: env})
}

func StartProcess(ctx context.Context, cfg ProcessConfig) (*Process, error) {
	if cfg.Executable == "" {
		cfg.Executable = "codex"
	}
	if cfg.Args == nil {
		cfg.Args = AppServerArgs()
	}
	cmd := exec.Command(cfg.Executable, cfg.Args...)
	cmd.Dir = cfg.Dir
	baseEnvironment := cfg.Env
	if baseEnvironment == nil {
		baseEnvironment = os.Environ()
	}
	cmd.Env = isolation.FilterEnvironment(baseEnvironment, isolation.EnvironmentPolicy{})
	provider.ConfigureProcessGroup(cmd)
	if cfg.Isolation != nil {
		if err := isolation.PrepareCommand(cmd, *cfg.Isolation); err != nil {
			return nil, fmt.Errorf("codex isolation: %w", err)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("codex stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex app server: %w", err)
	}
	if cfg.Isolation != nil {
		if err := isolation.ApplyStartedProcess(cmd.Process, *cfg.Isolation); err != nil {
			_ = provider.SweepProcessGroup(cmd.Process)
			_, _ = cmd.Process.Wait()
			return nil, fmt.Errorf("codex isolation limits: %w", err)
		}
	}
	p := &Process{cmd: cmd, stdin: stdin, wait: make(chan struct{}), egress: cfg.EgressGuard}
	p.Client = NewClient(stdout, stdin, ClientOptions{})
	go p.captureStderr(stderr)
	go func() {
		_ = cmd.Wait()
		_ = provider.SweepProcessGroup(cmd.Process)
		close(p.wait)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Close()
		case <-p.wait:
		}
	}()

	initialize := map[string]any{
		"clientInfo":   map[string]string{"name": "agentbridge", "version": "dev"},
		"capabilities": map[string]any{"experimentalApi": false},
	}
	var response map[string]any
	if err := p.Client.Call(ctx, "initialize", initialize, &response); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("initialize codex app server: %w", err)
	}
	if err := p.Client.Notify(ctx, "initialized", map[string]any{}); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("notify codex initialized: %w", err)
	}
	return p, nil
}

func (p *Process) Stderr() string {
	p.stderrMu.Lock()
	defer p.stderrMu.Unlock()
	return p.stderr.String()
}

func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		_ = p.Client.Close()
		p.closeErr = provider.StopProcessGroup(p.cmd.Process, p.wait, 5*time.Second)
	})
	return p.closeErr
}

func (p *Process) captureStderr(reader io.Reader) {
	data, _ := io.ReadAll(io.LimitReader(reader, maxStderrBytes+1))
	redacted := security.NewRedactor(security.Config{MaxPayloadRunes: maxStderrBytes}).RedactBytes(data)
	if p.egress != nil {
		if guarded, _ := p.egress.Check(egressguard.ClassTerminalOutput, redacted); guarded != nil {
			redacted = guarded
		}
	}
	p.stderrMu.Lock()
	defer p.stderrMu.Unlock()
	_, _ = p.stderr.Write(redacted)
}
