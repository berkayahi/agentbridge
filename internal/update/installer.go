package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

var ErrInstall = errors.New("update: install failed")

type HealthCheck func(context.Context, string) error

type Installer struct {
	Target   string
	Previous string
	Verify   func(context.Context, Metadata) error
	Health   HealthCheck
	Floor    FloorStore
}

func (i Installer) Install(ctx context.Context, metadata Metadata, staged string, now time.Time) error {
	if !filepath.IsAbs(i.Target) || !filepath.IsAbs(staged) || i.Health == nil || i.Verify == nil || i.Floor == nil || metadata.Identity.GOOS != runtime.GOOS || metadata.Identity.GOARCH != runtime.GOARCH {
		return ErrInstall
	}
	if err := metadata.Validate(now); err != nil {
		return err
	}
	if err := i.Verify(ctx, metadata); err != nil {
		return err
	}
	info, err := os.Lstat(staged)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || info.Mode()&0o022 != 0 {
		return ErrInstall
	}
	file, err := os.Open(staged)
	if err != nil {
		return ErrInstall
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		file.Close()
		return ErrInstall
	}
	if err := file.Close(); err != nil || hex.EncodeToString(digest.Sum(nil)) != metadata.Identity.ArtifactDigest {
		return ErrInstall
	}
	previous := i.Previous
	if previous == "" {
		previous = i.Target + ".previous"
	}
	if !filepath.IsAbs(previous) {
		return ErrInstall
	}
	targetMoved := false
	if targetInfo, statErr := os.Lstat(i.Target); statErr == nil {
		if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
			return ErrInstall
		}
		if err := os.Rename(i.Target, previous); err != nil {
			return fmt.Errorf("backup current binary: %w", err)
		}
		targetMoved = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return ErrInstall
	}
	restore := func() error {
		if err := os.Remove(i.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if targetMoved {
			return os.Rename(previous, i.Target)
		}
		return nil
	}
	if err := os.Rename(staged, i.Target); err != nil {
		_ = restore()
		return fmt.Errorf("activate staged binary: %w", err)
	}
	if err := i.Health(ctx, i.Target); err != nil {
		_ = restore()
		return fmt.Errorf("health rollback: %w", err)
	}
	if err := RecordFloor(ctx, i.Floor, metadata, now); err != nil {
		return errors.Join(err, restore())
	}
	return nil
}
