package policy

import "errors"

var ErrDenied = errors.New("policy: effect denied")

type Request struct {
	FilesystemScope string
	CommandClass    string
	NetworkTarget   string
	RuntimeID       string
	Model           string
	IsolationTier   string
	Effect          Effect
}

func Allows(snapshot Snapshot, request Request) bool {
	if request.FilesystemScope != "" && !containsOrUnconstrained(snapshot.FilesystemScopes, request.FilesystemScope) {
		return false
	}
	if request.CommandClass != "" && !containsOrUnconstrained(snapshot.CommandClasses, request.CommandClass) {
		return false
	}
	if request.NetworkTarget != "" && !containsOrUnconstrained(snapshot.NetworkDestinations, request.NetworkTarget) {
		return false
	}
	if request.RuntimeID != "" && !containsOrUnconstrained(snapshot.RuntimeIDs, request.RuntimeID) {
		return false
	}
	if request.Model != "" && !containsOrUnconstrained(snapshot.Models, request.Model) {
		return false
	}
	if request.IsolationTier != "" && !containsOrUnconstrained(snapshot.IsolationTiers, request.IsolationTier) {
		return false
	}
	switch request.Effect {
	case EffectSecrets:
		return snapshot.Secrets
	case EffectGitPublication:
		return snapshot.GitPublication
	}
	return true
}

func Require(snapshot Snapshot, request Request) error {
	if !Allows(snapshot, request) {
		return ErrDenied
	}
	return nil
}

func containsOrUnconstrained(values []string, value string) bool {
	if values == nil {
		return true
	}
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
