//go:build !linux && !darwin

package isolation

import (
	"fmt"
	"os"
	"os/exec"
)

func detectPlatformFacts() platformFacts {
	return platformFacts{Platform: "unsupported", Reasons: []string{"no supported process isolation primitive"}}
}

func preparePlatformCommand(_ *exec.Cmd, policy Policy) error {
	if policy.Tier != TierWeak || policy.Network.Mode != "" || !policy.Limits.Empty() {
		return fmt.Errorf("%w: unsupported platform", ErrCapabilityUnavailable)
	}
	return nil
}

func applyProcessLimits(*os.Process, ResourceLimits) error { return nil }
