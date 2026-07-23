// Package isolation describes host-enforced execution boundaries.
package isolation

import "errors"

type Tier string

const (
	TierStrong   Tier = "strong"
	TierStandard Tier = "standard"
	TierWeak     Tier = "weak"
)

var (
	ErrCapabilityUnavailable = errors.New("isolation: requested capability is unavailable")
	ErrInvalidPolicy         = errors.New("isolation: invalid policy")
	ErrWeakTier              = errors.New("isolation: weak tier cannot enable automatic capability")
)

// Capabilities are observations of the current host, never configuration
// claims. A caller may publish this value through a runtime registry without
// exposing any local filesystem path.
type Capabilities struct {
	Platform          string   `json:"platform"`
	Tier              Tier     `json:"tier"`
	ProcessGroups     bool     `json:"process_groups"`
	DescendantCleanup bool     `json:"descendant_cleanup"`
	ResourceLimits    bool     `json:"resource_limits"`
	LowPrivilege      bool     `json:"low_privilege"`
	FilesystemSandbox bool     `json:"filesystem_sandbox"`
	NetworkDeny       bool     `json:"network_deny"`
	NetworkAllowlist  bool     `json:"network_allowlist"`
	Reasons           []string `json:"reasons,omitempty"`
}

type platformFacts struct {
	Platform          string
	ProcessGroups     bool
	DescendantCleanup bool
	ResourceLimits    bool
	LowPrivilege      bool
	FilesystemSandbox bool
	NetworkDeny       bool
	NetworkAllowlist  bool
	Reasons           []string
}

func DetectCapabilities() Capabilities {
	facts := detectPlatformFacts()
	tier := TierWeak
	if facts.LowPrivilege && facts.ProcessGroups && facts.DescendantCleanup && facts.ResourceLimits && facts.FilesystemSandbox && facts.NetworkAllowlist {
		tier = TierStrong
	} else if facts.LowPrivilege && facts.ProcessGroups && facts.DescendantCleanup && facts.ResourceLimits && facts.FilesystemSandbox && facts.NetworkDeny {
		tier = TierStandard
	}
	return Capabilities{
		Platform: facts.Platform, Tier: tier,
		ProcessGroups: facts.ProcessGroups, DescendantCleanup: facts.DescendantCleanup,
		ResourceLimits: facts.ResourceLimits, LowPrivilege: facts.LowPrivilege,
		FilesystemSandbox: facts.FilesystemSandbox, NetworkDeny: facts.NetworkDeny,
		NetworkAllowlist: facts.NetworkAllowlist, Reasons: append([]string(nil), facts.Reasons...),
	}
}

func (c Capabilities) SupportsTier(want Tier) bool {
	return tierRank(c.Tier) >= tierRank(want)
}

func tierRank(value Tier) int {
	switch value {
	case TierStrong:
		return 3
	case TierStandard:
		return 2
	case TierWeak:
		return 1
	default:
		return 0
	}
}

func validTier(value Tier) bool {
	return value == TierStrong || value == TierStandard || value == TierWeak
}
