//go:build linux

package isolation

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func detectPlatformFacts() platformFacts {
	bwrap := trustedExecutable("/usr/bin/bwrap", "/usr/local/bin/bwrap", "/bin/bwrap")
	facts := platformFacts{
		Platform: "linux", ProcessGroups: true, DescendantCleanup: true,
		ResourceLimits: true, LowPrivilege: os.Getuid() != 0,
		FilesystemSandbox: bwrap != "", NetworkDeny: bwrap != "",
	}
	if bwrap == "" {
		facts.Reasons = append(facts.Reasons, "rootless bubblewrap is unavailable")
	}
	if !facts.LowPrivilege {
		facts.Reasons = append(facts.Reasons, "daemon is running as root")
	}
	return facts
}

func preparePlatformCommand(cmd *exec.Cmd, policy Policy) error {
	if err := invalidPlatformPolicy(policy); err != nil {
		return err
	}
	if policy.Tier == TierWeak {
		return nil
	}
	bwrap := trustedExecutable("/usr/bin/bwrap", "/usr/local/bin/bwrap", "/bin/bwrap")
	if bwrap == "" {
		return fmt.Errorf("%w: rootless bubblewrap", ErrCapabilityUnavailable)
	}
	originalPath := cmd.Path
	if originalPath == "" {
		return errors.New("isolation: command executable is empty")
	}
	originalArgs := append([]string(nil), cmd.Args...)
	args := []string{"--die-with-parent", "--unshare-user", "--unshare-pid", "--unshare-net", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp"}
	for _, path := range append([]string{policy.WorktreeRoot}, policy.WritablePaths...) {
		if path == "" {
			continue
		}
		args = append(args, "--bind", path, path)
	}
	args = append(args, "--chdir", cmd.Dir, "--", originalPath)
	if len(originalArgs) > 1 {
		args = append(args, originalArgs[1:]...)
	}
	cmd.Path = bwrap
	cmd.Args = append([]string{bwrap}, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	return nil
}

func applyProcessLimits(process *os.Process, limits ResourceLimits) error {
	pid := process.Pid
	values := []struct {
		resource int
		value    uint64
	}{
		{unix.RLIMIT_CPU, limits.CPUSeconds},
		{unix.RLIMIT_AS, limits.MemoryBytes},
		{unix.RLIMIT_FSIZE, limits.FileSizeBytes},
		{unix.RLIMIT_NOFILE, limits.OpenFiles},
		{unix.RLIMIT_NPROC, limits.Processes},
	}
	for _, item := range values {
		if item.value == 0 {
			continue
		}
		limit := unix.Rlimit{Cur: item.value, Max: item.value}
		if err := unix.Prlimit(pid, item.resource, &limit, nil); err != nil {
			return fmt.Errorf("set process resource limit: %w", err)
		}
	}
	return nil
}

func trustedExecutable(paths ...string) string {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			continue
		}
		return filepath.Clean(path)
	}
	return ""
}
