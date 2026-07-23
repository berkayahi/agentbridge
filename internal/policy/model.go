// Package policy compiles layered execution permissions into an immutable
// device-local snapshot.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

type ApprovalMode string

const (
	ApprovalAskEveryTime     ApprovalMode = "ask_every_time"
	ApprovalProviderDefault  ApprovalMode = "provider_default"
	ApprovalAutoWithinPolicy ApprovalMode = "auto_within_policy"
)

type Effect string

const (
	EffectSecrets          Effect = "secrets"
	EffectGitPublication   Effect = "git_publication"
	EffectProviderRecovery Effect = "provider_recovery"
	EffectUpdate           Effect = "update"
)

var ErrInvalidPolicy = errors.New("policy: invalid policy")

// Layer uses nil slices to mean unconstrained and non-nil empty slices to mean
// deny-all. Intersecting layers can therefore only narrow permissions.
type Layer struct {
	FilesystemScopes         []string      `json:"filesystem_scopes,omitempty"`
	CommandClasses           []string      `json:"command_classes,omitempty"`
	NetworkDestinations      []string      `json:"network_destinations,omitempty"`
	ArtifactRetention        time.Duration `json:"artifact_retention"`
	RawTerminal              bool          `json:"raw_terminal"`
	GitPublication           bool          `json:"git_publication"`
	Secrets                  bool          `json:"secrets"`
	RuntimeIDs               []string      `json:"runtime_ids,omitempty"`
	Models                   []string      `json:"models,omitempty"`
	IsolationTiers           []string      `json:"isolation_tiers,omitempty"`
	MaxDuration              time.Duration `json:"max_duration"`
	ApprovalMode             ApprovalMode  `json:"approval_mode"`
	RequireLocalConfirmation []Effect      `json:"require_local_confirmation,omitempty"`
}

type Snapshot struct {
	Layer
	Digest string `json:"digest"`
}

func Compile(layers ...Layer) (Snapshot, error) {
	if len(layers) == 0 {
		return Snapshot{}, ErrInvalidPolicy
	}
	compiled := layers[0]
	for _, layer := range layers[1:] {
		compiled = narrow(compiled, layer)
	}
	if compiled.ApprovalMode == "" {
		compiled.ApprovalMode = ApprovalProviderDefault
	}
	if compiled.ArtifactRetention < 0 || compiled.MaxDuration < 0 {
		return Snapshot{}, ErrInvalidPolicy
	}
	compiled = normalize(compiled)
	data, err := json.Marshal(compiled)
	if err != nil {
		return Snapshot{}, err
	}
	digest := sha256.Sum256(data)
	return Snapshot{Layer: compiled, Digest: hex.EncodeToString(digest[:])}, nil
}

func narrow(left, right Layer) Layer {
	return Layer{
		FilesystemScopes:         intersect(left.FilesystemScopes, right.FilesystemScopes),
		CommandClasses:           intersect(left.CommandClasses, right.CommandClasses),
		NetworkDestinations:      intersect(left.NetworkDestinations, right.NetworkDestinations),
		ArtifactRetention:        minPositive(left.ArtifactRetention, right.ArtifactRetention),
		RawTerminal:              left.RawTerminal && right.RawTerminal,
		GitPublication:           left.GitPublication && right.GitPublication,
		Secrets:                  left.Secrets && right.Secrets,
		RuntimeIDs:               intersect(left.RuntimeIDs, right.RuntimeIDs),
		Models:                   intersect(left.Models, right.Models),
		IsolationTiers:           intersect(left.IsolationTiers, right.IsolationTiers),
		MaxDuration:              minPositive(left.MaxDuration, right.MaxDuration),
		ApprovalMode:             narrowApproval(left.ApprovalMode, right.ApprovalMode),
		RequireLocalConfirmation: union(left.RequireLocalConfirmation, right.RequireLocalConfirmation),
	}
}

func narrowApproval(left, right ApprovalMode) ApprovalMode {
	if left == "" {
		return right
	}
	if right == "" || left == right {
		return left
	}
	if left == ApprovalAskEveryTime || right == ApprovalAskEveryTime {
		return ApprovalAskEveryTime
	}
	return ApprovalProviderDefault
}

func intersect(left, right []string) []string {
	if left == nil {
		return clone(right)
	}
	if right == nil {
		return clone(left)
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, value := range right {
		rightSet[value] = struct{}{}
	}
	result := make([]string, 0, len(left))
	for _, value := range left {
		if _, ok := rightSet[value]; ok {
			result = append(result, value)
		}
	}
	return result
}

func union(left, right []Effect) []Effect {
	set := make(map[Effect]struct{}, len(left)+len(right))
	for _, value := range left {
		set[value] = struct{}{}
	}
	for _, value := range right {
		set[value] = struct{}{}
	}
	result := make([]Effect, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func normalize(value Layer) Layer {
	value.FilesystemScopes = sortedUnique(value.FilesystemScopes)
	value.CommandClasses = sortedUnique(value.CommandClasses)
	value.NetworkDestinations = sortedUnique(value.NetworkDestinations)
	value.RuntimeIDs = sortedUnique(value.RuntimeIDs)
	value.Models = sortedUnique(value.Models)
	value.IsolationTiers = sortedUnique(value.IsolationTiers)
	return value
}

func sortedUnique(values []string) []string {
	if values == nil {
		return nil
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
func clone(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}
func minPositive(left, right time.Duration) time.Duration {
	if left == 0 {
		return right
	}
	if right == 0 || left < right {
		return left
	}
	return right
}
