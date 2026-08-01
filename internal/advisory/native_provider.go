package advisory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/berkayahi/agentbridge/internal/provider"
)

// NativeProvider adapts an explicitly attested provider analysis capability.
// It gives the provider a disposable empty workspace; repository paths,
// credentials, and host environment are never part of an advisory request.
type NativeProvider struct {
	Provider   provider.SafeAnalysisProvider
	ProviderID string
	ModelID    string
}

func (p NativeProvider) Capability() ProviderCapability {
	if p.Provider == nil || !p.Provider.AnalysisIsolationAttestation().Valid() {
		return ProviderCapability{ID: p.ProviderID}
	}
	return ProviderCapability{
		ID: p.ProviderID, AdvisorySessions: true,
		ReadOnly: true, StructuredOutput: true,
	}
}

func (p NativeProvider) ConfiguredModel() string { return p.ModelID }

func (p NativeProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	if p.Provider == nil || !p.Provider.AnalysisIsolationAttestation().Valid() {
		return ExecutionResult{}, ErrPolicyViolation
	}
	if request.Policy != effectivePolicy() || request.WebResearch.Enabled {
		return ExecutionResult{}, ErrPolicyViolation
	}
	if err := validateSchema(request.OutputSchema); err != nil {
		return ExecutionResult{}, err
	}
	workspace, err := os.MkdirTemp("", "agentbridge-advisory-")
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("create advisory workspace: %w", err)
	}
	defer os.RemoveAll(filepath.Clean(workspace))
	contextJSON, err := json.Marshal(request.Context)
	if err != nil {
		return ExecutionResult{}, err
	}
	policy := provider.NewReadOnlyAnalysisPolicy(workspace)
	if result := policy.Validate(); !result.Allowed {
		return ExecutionResult{}, ErrPolicyViolation
	}
	taskID, err := provider.NewID("advisory-" + request.ExecutionSessionID)
	if err != nil {
		return ExecutionResult{}, ErrInvalidRequest
	}
	input := strings.Join([]string{
		request.Prompt,
		"Context bundle:", string(contextJSON),
		"Output contract: return exactly one JSON value matching this strict schema; do not return prose, tool calls, reasoning, secrets, or approval requests:",
		string(request.OutputSchema),
	}, "\n")
	result, err := p.Provider.AnalyzeReadOnly(ctx, provider.AnalysisRequest{
		TaskID:           taskID,
		Input:            provider.Input{Text: input},
		WorkingDirectory: workspace, Model: request.ModelID, Policy: policy,
	})
	if err != nil {
		if errors.Is(err, provider.ErrAnalysisApprovalDeclined) {
			return ExecutionResult{}, ErrPolicyViolation
		}
		return ExecutionResult{}, err
	}
	providerID := string(result.ProviderID)
	if providerID == "" {
		providerID = p.ProviderID
	}
	modelID := result.Model
	if modelID == "" {
		modelID = p.ModelID
	}
	output, err := sanitizeStructuredOutput(nil, result.Output)
	if err != nil {
		return ExecutionResult{}, err
	}
	return ExecutionResult{ProviderID: providerID, ModelID: modelID, Output: output}, nil
}
