package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/berkayahi/agentbridge/internal/config"
)

func TestPinnedAnalysisFixtureAttestsExactOwnedExecutableAndScrubsEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture-provider")
	contents := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	value := config.ProviderConfig{
		Executable: path, Model: "gpt-5.6-terra",
		AnalysisFixture: config.AnalysisFixtureConfig{Environment: "fixture", ExecutableSHA256: hex.EncodeToString(digest[:])},
	}
	attestation, err := pinnedAnalysisFixture(value)
	if err != nil || !attestation.Valid() || attestation.Mechanism != "pinned-deterministic-fixture" {
		t.Fatalf("attestation = %#v err=%v", attestation, err)
	}
	environment := strings.Join(pinnedFixtureEnvironment([]string{"PATH=/fixture/bin", "HOME=/secret/home", "OPENAI_API_KEY=secret", "AGENTBRIDGE_DATA_DIR=/state"}), "\n")
	if environment != "PATH=/fixture/bin" {
		t.Fatalf("fixture environment = %q", environment)
	}

	value.AnalysisFixture.ExecutableSHA256 = strings.Repeat("0", 64)
	if _, err := pinnedAnalysisFixture(value); err == nil {
		t.Fatal("mismatched pinned digest was accepted")
	}
}

func TestPinnedAnalysisFixtureRejectsSymlinkAndWritableExecutable(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	contents := []byte("fixture")
	if err := os.WriteFile(target, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	fixture := config.AnalysisFixtureConfig{Environment: "dev", ExecutableSHA256: hex.EncodeToString(digest[:])}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := pinnedAnalysisFixture(config.ProviderConfig{Executable: link, AnalysisFixture: fixture}); err == nil {
		t.Fatal("symlink fixture was accepted")
	}
	if err := os.Chmod(target, 0o722); err != nil {
		t.Fatal(err)
	}
	if _, err := pinnedAnalysisFixture(config.ProviderConfig{Executable: target, AnalysisFixture: fixture}); err == nil {
		t.Fatal("group/world-writable fixture was accepted")
	}
}
