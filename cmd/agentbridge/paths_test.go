package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// A desktop host must not re-derive the runtime layout: duplicating it would
// silently drift from the daemon. The binary reports the paths it will use.
func TestPathsCommandReportsDerivedRuntimePaths(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runWithDeps(context.Background(),
		[]string{"paths", "--data-dir", "/private/hive", "--json"},
		strings.NewReader(""), &stdout, &stderr, commandDeps{})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var reported struct {
		DataDir        string `json:"data_dir"`
		Database       string `json:"database"`
		LocalAPISocket string `json:"local_api_socket"`
		LocalAPISecret string `json:"local_api_secret"`
		ControlSocket  string `json:"control_socket"`
		Worktrees      string `json:"worktrees"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &reported); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, stdout.String())
	}
	want := map[string]string{
		reported.DataDir:        "/private/hive",
		reported.Database:       filepath.Join("/private/hive", "agentbridge.db"),
		reported.LocalAPISocket: filepath.Join("/private/hive", "run", "local-api.sock"),
		reported.LocalAPISecret: filepath.Join("/private/hive", "run", "local-api.secret"),
		reported.ControlSocket:  filepath.Join("/private/hive", "run", "control.sock"),
		reported.Worktrees:      filepath.Join("/private/hive", "worktrees"),
	}
	for got, expected := range want {
		if got != expected {
			t.Fatalf("reported %q, want %q", got, expected)
		}
	}
}

// A relative data dir is the same mistake serve rejects, so it must fail here
// too rather than emitting a path a host would then trust.
func TestPathsCommandRejectsRelativeDataDir(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runWithDeps(context.Background(),
		[]string{"paths", "--data-dir", "hive", "--json"},
		strings.NewReader(""), &stdout, &stderr, commandDeps{})
	if code == 0 {
		t.Fatalf("relative data dir must fail, stdout=%q", stdout.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("nothing may be reported on failure, stdout=%q", stdout.String())
	}
}

// The reported paths must never leak the local API secret itself, only its
// location: a host reads the file under its own owner-only permissions.
func TestPathsCommandReportsNoSecretMaterial(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runWithDeps(context.Background(),
		[]string{"paths", "--data-dir", "/private/hive", "--json"},
		strings.NewReader(""), &stdout, &stderr, commandDeps{}); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var generic map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &generic); err != nil {
		t.Fatal(err)
	}
	for key := range generic {
		if strings.Contains(key, "token") || key == "secret" {
			t.Fatalf("reported key %q may carry credential material", key)
		}
	}
}
