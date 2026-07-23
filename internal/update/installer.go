package update

import (
	"context"
	"errors"
	"fmt"
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
	if err := i.Verify(ctx, metadata); err != nil {
		return err
	}
	info, err := os.Stat(staged)
	if err != nil || !info.Mode().IsRegular() {
		return ErrInstall
	}
	previous := i.Previous
	if previous == "" {
		previous = i.Target + ".previous"
	}
	if _, err := os.Stat(i.Target); err == nil {
		if err := os.Rename(i.Target, previous); err != nil {
			return fmt.Errorf("backup current binary: %w", err)
		}
	}
	if err := os.Rename(staged, i.Target); err != nil {
		_ = os.Rename(previous, i.Target)
		return fmt.Errorf("activate staged binary: %w", err)
	}
	if err := i.Health(ctx, i.Target); err != nil {
		_ = os.Remove(i.Target)
		_ = os.Rename(previous, i.Target)
		return fmt.Errorf("health rollback: %w", err)
	}
	if err := RecordFloor(ctx, i.Floor, metadata, now); err != nil {
		return err
	}
	return nil
}
