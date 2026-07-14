package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStopProcessGroupTerminatesDescendants(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-group regression requires Unix")
	}
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	cmd := exec.Command("/bin/sh", "-c", `(trap '' INT; while :; do echo tick >> "$1"; sleep 0.05; done) & trap 'exit 0' INT; wait`, "sh", heartbeat)
	ConfigureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	waitForHeartbeat(t, heartbeat)
	if err := StopProcessGroup(cmd.Process, done, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	after, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("descendant continued running after provider shutdown")
	}
}

func TestSweepProcessGroupTerminatesDescendantsAfterNaturalLeaderExit(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-group regression requires Unix")
	}
	heartbeat := filepath.Join(t.TempDir(), "natural-heartbeat")
	cmd := exec.Command("/bin/sh", "-c", `(trap '' INT TERM; while :; do echo tick >> "$1"; sleep 0.05; done) & sleep 0.1; exit 0`, "sh", heartbeat)
	ConfigureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForHeartbeat(t, heartbeat)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := SweepProcessGroup(cmd.Process); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	after, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("descendant survived natural provider leader exit")
	}
}

func waitForHeartbeat(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		info, err := os.Stat(path)
		if err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child heartbeat was not recorded")
}
