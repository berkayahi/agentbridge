//go:build darwin

package isolation

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func detectPlatformFacts() platformFacts {
	sandbox := trustedDarwinExecutable()
	facts := platformFacts{
		Platform: "darwin", ProcessGroups: true, DescendantCleanup: true,
		ResourceLimits: false, LowPrivilege: os.Getuid() != 0,
		FilesystemSandbox: sandbox != "", NetworkDeny: sandbox != "", NetworkAllowlist: sandbox != "",
	}
	if sandbox == "" {
		facts.Reasons = append(facts.Reasons, "macOS sandbox-exec is unavailable")
	}
	if !facts.ResourceLimits {
		facts.Reasons = append(facts.Reasons, "per-process resource limit application is unavailable")
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
	sandbox := trustedDarwinExecutable()
	if sandbox == "" {
		return fmt.Errorf("%w: macOS sandbox-exec", ErrCapabilityUnavailable)
	}
	if !policy.Limits.Empty() {
		return fmt.Errorf("%w: macOS resource limits", ErrCapabilityUnavailable)
	}
	profile := "(version 1)\n(deny default)\n(allow process*)\n(allow sysctl-read)\n(allow file-read*)\n"
	if policy.WorktreeRoot != "" {
		profile += "(allow file-write* (subpath \"" + sandboxQuote(policy.WorktreeRoot) + "\"))\n"
	}
	for _, path := range policy.WritablePaths {
		profile += "(allow file-write* (subpath \"" + sandboxQuote(path) + "\"))\n"
	}
	if policy.Network.Mode == NetworkAllowlist {
		for _, host := range policy.Network.ApprovedHosts() {
			profile += "(allow network-outbound (remote-name \"" + sandboxQuote(host) + "\"))\n"
		}
	}
	originalPath := cmd.Path
	originalArgs := append([]string(nil), cmd.Args...)
	cmd.Path = sandbox
	cmd.Args = []string{sandbox, "-p", profile, "--", originalPath}
	if len(originalArgs) > 1 {
		cmd.Args = append(cmd.Args, originalArgs[1:]...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func applyProcessLimits(*os.Process, ResourceLimits) error { return nil }

func trustedDarwinExecutable() string {
	info, err := os.Stat("/usr/bin/sandbox-exec")
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return ""
	}
	return "/usr/bin/sandbox-exec"
}

func sandboxQuote(value string) string { return strings.ReplaceAll(value, "\\\"", "\\\\\"") }
