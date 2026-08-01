package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/berkayahi/agentbridge/internal/config"
	"github.com/berkayahi/agentbridge/internal/provider"
)

const maxPinnedFixtureExecutableBytes = 16 << 20

// pinnedAnalysisFixture verifies the exact deterministic fixture executable
// before issuing an analysis attestation. A YAML declaration alone has no
// authority: the file must be a non-symlink regular file owned by this daemon
// user, not group/world writable, within a small bound, and match the pinned
// digest. This seam is intentionally limited to fixture/dev acceptance.
func pinnedAnalysisFixture(value config.ProviderConfig) (provider.AnalysisIsolationAttestation, error) {
	fixture := value.AnalysisFixture
	if strings.TrimSpace(fixture.Environment) == "" && strings.TrimSpace(fixture.ExecutableSHA256) == "" {
		return provider.AnalysisIsolationAttestation{}, nil
	}
	if fixture.Environment != "fixture" && fixture.Environment != "dev" {
		return provider.AnalysisIsolationAttestation{}, errors.New("analysis fixture environment is not fixture/dev")
	}
	path := strings.TrimSpace(value.Executable)
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return provider.AnalysisIsolationAttestation{}, fmt.Errorf("inspect pinned analysis fixture: %w", err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.Mode().IsRegular() {
		return provider.AnalysisIsolationAttestation{}, errors.New("pinned analysis fixture must be a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return provider.AnalysisIsolationAttestation{}, fmt.Errorf("open pinned analysis fixture: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !os.SameFile(linkInfo, info) || !info.Mode().IsRegular() {
		return provider.AnalysisIsolationAttestation{}, errors.New("pinned analysis fixture identity changed during verification")
	}
	if !ownedByCurrentUser(info) {
		return provider.AnalysisIsolationAttestation{}, errors.New("pinned analysis fixture must be owned by the daemon user")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return provider.AnalysisIsolationAttestation{}, errors.New("pinned analysis fixture must not be group/world writable")
	}
	if info.Size() < 1 || info.Size() > maxPinnedFixtureExecutableBytes {
		return provider.AnalysisIsolationAttestation{}, errors.New("pinned analysis fixture size is invalid")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxPinnedFixtureExecutableBytes+1)); err != nil {
		return provider.AnalysisIsolationAttestation{}, fmt.Errorf("hash pinned analysis fixture: %w", err)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != fixture.ExecutableSHA256 {
		return provider.AnalysisIsolationAttestation{}, errors.New("pinned analysis fixture digest mismatch")
	}
	return provider.AnalysisIsolationAttestation{
		Mechanism: "pinned-deterministic-fixture", FilesystemReadsWorkspaceOnly: true,
		HostEnvironmentExcluded: true, NetworkDenied: true, ProductionDataDenied: true, DestructiveActionsDenied: true,
	}, nil
}

// pinnedFixtureEnvironment excludes the host environment. PATH is retained so
// a pinned script with an env shebang can find its pinned interpreter; no home,
// credential, provider, daemon, or repository variables cross the boundary.
func pinnedFixtureEnvironment(base []string) []string {
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if ok && name == "PATH" {
			return []string{entry}
		}
	}
	return []string{"PATH=/usr/bin:/bin"}
}
