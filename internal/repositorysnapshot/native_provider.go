package repositorysnapshot

import (
	"context"
	"errors"
	"strings"

	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/workmodel"
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
	taskID, err := provider.NewID("understanding-" + shortCommit(request.ExactCommitSHA))
	if err != nil {
		return ProviderResult{}, ErrInvalidRequest
	}
	session, events, err := p.Provider.Start(ctx, provider.StartRequest{
		TaskID: taskID, Input: provider.Input{Text: request.Prompt},
		WorkingDirectory: request.WorkspacePath, Model: request.Model,
		ExecutionProfile: workmodel.ExecutionProfile{Model: request.Model, ApprovalMode: "ask_every_time"},
	})
	if err != nil {
		return ProviderResult{}, err
	}
	defer func() { _ = p.Provider.Interrupt(context.Background(), session) }()
	var output strings.Builder
	for {
		select {
		case <-ctx.Done():
			return ProviderResult{}, ctx.Err()
		case event, ok := <-events:
			if !ok {
				return ProviderResult{}, ErrProviderOutput
			}
			switch event.Type {
			case provider.EventApprovalRequired:
				if event.RequestID.Valid() {
					_ = p.Provider.ResolveApproval(ctx, provider.ApprovalDecision{
						RequestID: event.RequestID, TaskID: taskID, Allow: false,
					})
				}
				return ProviderResult{ApprovalRequested: true}, ErrProviderApproval
			case provider.EventAssistantMessage:
				if output.Len()+len(event.Message) > MaxProviderOutputBytes {
					return ProviderResult{}, ErrProviderOutputBounds
				}
				output.WriteString(event.Message)
			case provider.EventError:
				return ProviderResult{}, errors.New("provider analysis failed")
			case provider.EventCompleted:
				model := request.Model
				if model == "" {
					model = p.DefaultModel
				}
				return ProviderResult{ProviderID: string(p.Provider.Name()), Model: model, Output: []byte(output.String())}, nil
			}
		}
	}
}

func shortCommit(commit string) string {
	if len(commit) > 16 {
		return commit[:16]
	}
	return commit
}
