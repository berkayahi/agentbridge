package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

type daemonRuntime interface {
	Start(context.Context) error
	Run(context.Context) error
	Shutdown(context.Context) error
}

func runDaemonLifecycle(ctx context.Context, runtime daemonRuntime) error {
	if runtime == nil {
		return errors.New("daemon runtime is unavailable")
	}
	if err := runtime.Start(ctx); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		return errors.Join(err, runtime.Shutdown(shutdownCtx))
	}
	runErr := runtime.Run(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	shutdownErr := runtime.Shutdown(shutdownCtx)
	if ctx.Err() != nil && errors.Is(runErr, ctx.Err()) {
		runErr = nil
	}
	return errors.Join(runErr, shutdownErr)
}

type runtimePaths struct {
	data          string
	database      string
	attachments   string
	worktrees     string
	runtime       string
	controlSocket string
	claudeConfig  string
	mcpConfig     string
}

func deriveRuntimePaths(dataDir string) (runtimePaths, error) {
	if !filepath.IsAbs(dataDir) {
		return runtimePaths{}, errors.New("AGENTBRIDGE_DATA_DIR must be an absolute path")
	}
	data := filepath.Clean(dataDir)
	return runtimePaths{
		data: data, database: filepath.Join(data, "agentbridge.db"),
		attachments: filepath.Join(data, "attachments"), worktrees: filepath.Join(data, "worktrees"),
		runtime: filepath.Join(data, "run"), controlSocket: filepath.Join(data, "run", "control.sock"),
		claudeConfig: filepath.Join(data, "claude"), mcpConfig: filepath.Join(data, "mcp"),
	}, nil
}

func (p runtimePaths) prepare() error {
	for _, path := range []string{p.data, p.attachments, p.worktrees, p.runtime, p.claudeConfig, p.mcpConfig} {
		if err := privateDirectory(path); err != nil {
			return err
		}
	}
	return nil
}

func privateDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runtime directory cannot be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect runtime directory: %w", err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runtime directory is unsafe")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure runtime directory: %w", err)
	}
	return nil
}

func subscriptionEnvironment(base []string, claudeConfigDir string) []string {
	blocked := map[string]struct{}{
		"OPENAI_API_KEY": {}, "ANTHROPIC_API_KEY": {}, "ANTHROPIC_AUTH_TOKEN": {},
		"CLAUDE_CODE_OAUTH_TOKEN": {}, "CLAUDE_CONFIG_DIR": {},
	}
	environment := make([]string, 0, len(base)+1)
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(name, "AGENTBRIDGE_") {
			continue
		}
		if _, excluded := blocked[name]; excluded {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "CLAUDE_CONFIG_DIR="+claudeConfigDir)
}

type daemonBuilder func(context.Context, config.Config, runtimePaths, config.Credential, []string) (daemonRuntime, error)

func serveDaemon(ctx context.Context, configPath string) error {
	return serveDaemonWithBuilder(ctx, configPath, buildDaemon)
}

func serveDaemonWithBuilder(ctx context.Context, configPath string, builder daemonBuilder) error {
	if err := config.RejectAPIKeyEnvironment(); err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	paths, err := deriveRuntimePaths(os.Getenv("AGENTBRIDGE_DATA_DIR"))
	if err != nil {
		return err
	}
	if err := paths.prepare(); err != nil {
		return err
	}
	releaseDatabaseLock, err := sqlite.AcquireDatabaseRuntimeLock(paths.database)
	if err != nil {
		return err
	}
	defer releaseDatabaseLock()
	token, err := (config.CredentialReader{}).Read("telegram_bot_token")
	if err != nil {
		return err
	}
	runtime, err := builder(ctx, cfg, paths, token, subscriptionEnvironment(os.Environ(), paths.claudeConfig))
	if err != nil {
		return err
	}
	return runDaemonLifecycle(ctx, runtime)
}
