package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerRunsCommandsInOrderAndRedactsEvents(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	order := filepath.Join(root, "order")
	commands := []Command{
		{Argv: []string{"/bin/sh", "-c", "printf first >> ../order; printf 'ghp_abcdefghijklmnopqrstuvwxyz123456\\n'"}, Dir: "sub"},
		{Argv: []string{"/bin/sh", "-c", "printf second >> ../order"}, Dir: "sub"},
	}
	report, err := (Runner{}).Run(context.Background(), root, commands)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(order)
	if string(content) != "firstsecond" {
		t.Fatalf("order = %q", content)
	}
	if len(report.Commands) != 2 || strings.Contains(report.Summary, "ghp_abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("report = %#v", report)
	}
}

func TestRunnerStopsOnFirstFailure(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker")
	_, err := (Runner{}).Run(context.Background(), root, []Command{{Argv: []string{"/bin/sh", "-c", "exit 3"}}, {Argv: []string{"/usr/bin/touch", marker}}})
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("second command ran")
	}
}

func TestRunnerRejectsEscapingDirectoryAndHonorsTimeout(t *testing.T) {
	root := t.TempDir()
	if _, err := (Runner{}).Run(context.Background(), root, []Command{{Argv: []string{"/bin/true"}, Dir: "../outside"}}); !errors.Is(err, ErrUnsafeDirectory) {
		t.Fatalf("dir err = %v", err)
	}
	started := time.Now()
	_, err := (Runner{}).Run(context.Background(), root, []Command{{Argv: []string{"/bin/sh", "-c", "sleep 10"}, Timeout: 20 * time.Millisecond}})
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("timeout err = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("timeout did not cancel promptly")
	}
}
