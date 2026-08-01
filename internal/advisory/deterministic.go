package advisory

import (
	"context"
	"encoding/json"
)

// DeterministicProvider is a process-local adapter for acceptance and
// development. It has no process, filesystem, network, credential, or mutable
// state dependency and returns the configured JSON value unchanged.
type DeterministicProvider struct {
	ProviderID string
	ModelID    string
	Output     []byte
}

func (p DeterministicProvider) Capability() ProviderCapability {
	return ProviderCapability{
		ID: p.ProviderID, AdvisorySessions: true, ReadOnly: true, StructuredOutput: true,
		WebResearch: false,
	}
}

func (p DeterministicProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecutionResult{}, err
	}
	if request.Policy.RepositoryWrites || request.Policy.BranchMutation || request.Policy.WorktreeMutation ||
		request.Policy.GitIntegration || request.Policy.SecretValueAccess || request.Policy.DecisionMutation || request.Policy.HumanApproval ||
		request.Policy.WebResearchAllowed {
		return ExecutionResult{}, ErrPolicyViolation
	}
	if !json.Valid(p.Output) {
		return ExecutionResult{}, ErrStructuredOutput
	}
	return ExecutionResult{ProviderID: p.ProviderID, ModelID: p.ModelID, Output: append([]byte(nil), p.Output...)}, nil
}
