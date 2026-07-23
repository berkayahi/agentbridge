package operations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type RestoreCheckOptions struct {
	Backup  string
	WorkDir string
}

type RestoreCheckResult struct {
	Backup   string
	Restored string
}

func RestoreCheck(ctx context.Context, options RestoreCheckOptions) (RestoreCheckResult, error) {
	if strings.TrimSpace(options.Backup) == "" || strings.TrimSpace(options.WorkDir) == "" {
		return RestoreCheckResult{}, errors.New("restore-check requires backup and work directory")
	}
	backupPath, err := findBackup(options.Backup)
	if err != nil {
		return RestoreCheckResult{}, fmt.Errorf("find backup: %w", err)
	}
	if err := os.MkdirAll(options.WorkDir, 0o700); err != nil {
		return RestoreCheckResult{}, fmt.Errorf("create restore work directory: %w", err)
	}
	source, err := openDatabase(ctx, backupPath)
	if err != nil {
		return RestoreCheckResult{}, err
	}
	defer source.Close()
	if err := verifyV2(ctx, source); err != nil {
		return RestoreCheckResult{}, fmt.Errorf("verify source backup: %w", err)
	}
	destination := filepath.Join(options.WorkDir, "agentbridge-restored.db")
	if _, err := os.Stat(destination); err == nil {
		return RestoreCheckResult{}, errors.New("restore destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return RestoreCheckResult{}, err
	}
	if err := snapshot(ctx, source, destination); err != nil {
		return RestoreCheckResult{}, err
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return RestoreCheckResult{}, err
	}
	restored, err := openDatabase(ctx, destination)
	if err != nil {
		return RestoreCheckResult{}, err
	}
	defer restored.Close()
	if err := verifyV2(ctx, restored); err != nil {
		return RestoreCheckResult{}, fmt.Errorf("verify restored database: %w", err)
	}
	return RestoreCheckResult{Backup: backupPath, Restored: destination}, nil
}
