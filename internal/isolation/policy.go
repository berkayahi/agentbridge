package isolation

import (
	"errors"
	"fmt"
	"path/filepath"
)

type Automation struct {
	Secrets     bool `json:"secrets" yaml:"secrets"`
	Network     bool `json:"network" yaml:"network"`
	Publication bool `json:"publication" yaml:"publication"`
}

type ResourceLimits struct {
	CPUSeconds    uint64 `json:"cpu_seconds" yaml:"cpu_seconds"`
	MemoryBytes   uint64 `json:"memory_bytes" yaml:"memory_bytes"`
	FileSizeBytes uint64 `json:"file_size_bytes" yaml:"file_size_bytes"`
	OpenFiles     uint64 `json:"open_files" yaml:"open_files"`
	Processes     uint64 `json:"processes" yaml:"processes"`
}

func (l ResourceLimits) Empty() bool {
	return l.CPUSeconds == 0 && l.MemoryBytes == 0 && l.FileSizeBytes == 0 && l.OpenFiles == 0 && l.Processes == 0
}

type Policy struct {
	Tier          Tier           `json:"tier" yaml:"tier"`
	WorktreeRoot  string         `json:"-" yaml:"worktree_root,omitempty"`
	WritablePaths []string       `json:"-" yaml:"writable_paths,omitempty"`
	Network       NetworkPolicy  `json:"network" yaml:"network"`
	Limits        ResourceLimits `json:"limits" yaml:"limits"`
	Automation    Automation     `json:"automation" yaml:"automation"`
}

func (p Policy) normalized() Policy {
	if p.Tier == "" {
		p.Tier = TierWeak
	}
	if p.Network.Mode == "" && p.Tier != TierWeak {
		p.Network.Mode = NetworkDeny
	}
	return p
}

func (p Policy) Validate() error {
	p = p.normalized()
	if !validTier(p.Tier) {
		return fmt.Errorf("%w: unknown tier %q", ErrInvalidPolicy, p.Tier)
	}
	if p.Tier != TierWeak && !filepath.IsAbs(p.WorktreeRoot) {
		return fmt.Errorf("%w: worktree root must be absolute for %s tier", ErrInvalidPolicy, p.Tier)
	}
	if p.WorktreeRoot != "" && !filepath.IsAbs(p.WorktreeRoot) {
		return fmt.Errorf("%w: worktree root must be absolute", ErrInvalidPolicy)
	}
	for _, path := range p.WritablePaths {
		if path == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("%w: writable paths must be absolute", ErrInvalidPolicy)
		}
	}
	if err := p.Network.Validate(); err != nil {
		return fmt.Errorf("%w: network: %v", ErrInvalidPolicy, err)
	}
	if p.Tier == TierWeak && (p.Automation.Secrets || p.Automation.Network || p.Automation.Publication) {
		return ErrWeakTier
	}
	return nil
}

func (p Policy) Enforce() (Capabilities, error) {
	p = p.normalized()
	if err := p.Validate(); err != nil {
		return Capabilities{}, err
	}
	caps := DetectCapabilities()
	if !caps.SupportsTier(p.Tier) {
		return caps, fmt.Errorf("%w: host tier %s cannot satisfy %s", ErrCapabilityUnavailable, caps.Tier, p.Tier)
	}
	if p.Network.Mode == NetworkDeny && !caps.NetworkDeny {
		return caps, fmt.Errorf("%w: outbound network deny", ErrCapabilityUnavailable)
	}
	if p.Network.Mode == NetworkAllowlist && !caps.NetworkAllowlist {
		return caps, fmt.Errorf("%w: outbound network allowlist", ErrCapabilityUnavailable)
	}
	if !p.Limits.Empty() && !caps.ResourceLimits {
		return caps, fmt.Errorf("%w: resource limits", ErrCapabilityUnavailable)
	}
	return caps, nil
}

func (p Policy) RequiresSandbox() bool {
	p = p.normalized()
	return p.Tier != TierWeak || p.Network.Mode != "" || p.WorktreeRoot != "" || !p.Limits.Empty()
}

func isCapabilityError(err error) bool {
	return errors.Is(err, ErrCapabilityUnavailable) || errors.Is(err, ErrWeakTier)
}
