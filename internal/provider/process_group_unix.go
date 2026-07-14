//go:build darwin || linux

package provider

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ConfigureProcessGroup makes the provider CLI the leader of a private
// process group. Tool and shell descendants inherit that group by default.
func ConfigureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// SweepProcessGroup is called by the in-memory wait owner immediately after
// the provider leader is reaped and before completion is published. If a tool
// descendant still holds the owned PGID it is killed; no PID/PGID is persisted
// or rediscovered after restart.
func SweepProcessGroup(process *os.Process) error {
	if process == nil {
		return nil
	}
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// StopProcessGroup signals only the still-owned process group. AgentBridge
// never persists a PID or signals one recovered from disk, avoiding PID-reuse
// hazards after a daemon restart.
func StopProcessGroup(process *os.Process, done <-chan struct{}, grace time.Duration) error {
	if process == nil {
		return nil
	}
	select {
	case <-done:
		// The owned process was already reaped before shutdown began. Do not
		// signal a persisted/stale numeric PID or PGID.
		return nil
	default:
	}
	if grace <= 0 {
		grace = 5 * time.Second
	}
	interruptErr := syscall.Kill(-process.Pid, syscall.SIGINT)
	if interruptErr != nil && !errors.Is(interruptErr, syscall.ESRCH) {
		return interruptErr
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		// SIGINT was sent while the group leader was still owned. The leader may
		// exit while a tool child ignores SIGINT, so sweep that same live group.
		killErr := SweepProcessGroup(process)
		if killErr != nil {
			return killErr
		}
		return nil
	case <-timer.C:
	}
	killErr := SweepProcessGroup(process)
	if killErr != nil {
		return killErr
	}
	<-done
	return nil
}
