package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
)

type RestoreCheckOptions struct {
	Backup       string
	WorkDir      string
	IdentityPath string
	RecordPath   string
}

type RestoreCheckResult struct {
	Backup               string
	Restored             string
	Mode                 string
	ManagedReady         bool
	ReEnrollmentRequired bool
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
	result := RestoreCheckResult{Backup: backupPath, Restored: destination, Mode: "standalone"}
	manifest, err := loadManifest(backupPath + ".manifest.json")
	if err == nil {
		result.Mode = manifest.Mode
		if result.Mode == "" {
			result.Mode = "standalone"
		}
		result.ReEnrollmentRequired = manifest.ReEnrollmentRequired
		if manifest.Mode == "managed" {
			result.ReEnrollmentRequired = true
			if strings.TrimSpace(options.IdentityPath) != "" {
				key, keyErr := deviceidentity.Load(options.IdentityPath)
				recordPath := options.RecordPath
				if recordPath == "" {
					recordPath = filepath.Join(filepath.Dir(options.IdentityPath), "enrollment.json")
				}
				record, recordErr := deviceidentity.LoadRecord(recordPath)
				if keyErr == nil && recordErr == nil && record.Fingerprint == key.Fingerprint() && record.OrganizationID == manifest.OrganizationID && record.DeviceID == manifest.DeviceID && record.HighestControllerEpoch >= manifest.HighestControllerEpoch && !record.Revoked && !record.Quarantined {
					result.ManagedReady = true
					result.ReEnrollmentRequired = false
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return RestoreCheckResult{}, fmt.Errorf("read backup manifest: %w", err)
	}
	return result, nil
}

func loadManifest(path string) (BackupManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return BackupManifest{}, err
	}
	defer file.Close()
	var manifest BackupManifest
	if err := json.NewDecoder(io.LimitReader(file, 128*1024)).Decode(&manifest); err != nil {
		return BackupManifest{}, err
	}
	return manifest, nil
}
