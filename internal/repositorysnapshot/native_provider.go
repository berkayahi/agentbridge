package repositorysnapshot

import (
	"context"
	"errors"
	"strings"

	"github.com/berkayahi/agentbridge/internal/provider"
)

// NativeAnalysisProvider adapts an already configured provider to the
// read-only understanding boundary. The only filesystem path supplied to it
// is the disposable evidence workspace; no delivery or commit operation exists
// on this adapter.
type NativeAnalysisProvider struct {
	Provider     provider.Provider
	DefaultModel string
}

func (p NativeAnalysisProvider) Analyze(ctx context.Context, request ProviderRequest) (ProviderResult, error) {
	if p.Provider == nil || strings.TrimSpace(request.WorkspacePath) == "" {
		return ProviderResult{}, ErrProviderNotConfigured
	}
	safe, ok := p.Provider.(provider.SafeAnalysisProvider)
	if !ok {
		return ProviderResult{}, ErrProviderPolicy
	}
	taskID, err := provider.NewID("understanding-" + shortCommit(request.ExactCommitSHA))
	if err != nil {
		return ProviderResult{}, ErrInvalidRequest
	}
	policy := provider.NewReadOnlyAnalysisPolicy(request.WorkspacePath)
	if result := policy.Validate(); !result.Allowed {
		return ProviderResult{}, ErrProviderPolicy
	}
	result, err := safe.AnalyzeReadOnly(ctx, provider.AnalysisRequest{
		TaskID: taskID, Input: provider.Input{Text: request.Prompt},
		WorkingDirectory: request.WorkspacePath, Model: request.Model, Policy: policy,
	})
	if err != nil {
		if errors.Is(err, provider.ErrAnalysisApprovalDeclined) {
			return ProviderResult{ApprovalRequested: true}, ErrProviderApproval
		}
		return ProviderResult{}, err
	}
	model := result.Model
	if model == "" {
		model = p.DefaultModel
	}
	return ProviderResult{ProviderID: string(result.ProviderID), Model: model, Output: result.Output}, nil
}

func shortCommit(commit string) string {
	if len(commit) > 16 {
		return commit[:16]
	}
	return commit
}
