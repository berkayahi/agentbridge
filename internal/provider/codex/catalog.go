package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/berkayahi/agentbridge/internal/provider"
)

const modelCatalogPageSize = 100

type appServerClient interface {
	Call(context.Context, string, any, any) error
	Notify(context.Context, string, any) error
}

// InitializeAppServer performs the initialization handshake required by the
// installed Codex app-server protocol before any other request.
func InitializeAppServer(ctx context.Context, client appServerClient) error {
	if client == nil {
		return errors.New("initialize Codex app server: nil client")
	}
	var response struct {
		UserAgent string `json:"userAgent"`
	}
	if err := client.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    "agentbridge",
			"version": "1",
		},
		"capabilities": map[string]any{},
	}, &response); err != nil {
		return fmt.Errorf("initialize Codex app server: %w", err)
	}
	if err := client.Notify(ctx, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("notify Codex app server initialization: %w", err)
	}
	return nil
}

type modelListResponse struct {
	Data       []codexModel `json:"data"`
	NextCursor string       `json:"nextCursor"`
}

type codexModel struct {
	ID                     string                 `json:"id"`
	Model                  string                 `json:"model"`
	DisplayName            string                 `json:"displayName"`
	Description            string                 `json:"description"`
	Hidden                 bool                   `json:"hidden"`
	IsDefault              bool                   `json:"isDefault"`
	DefaultReasoningEffort string                 `json:"defaultReasoningEffort"`
	ReasoningEfforts       []codexReasoningEffort `json:"supportedReasoningEfforts"`
}

type codexReasoningEffort struct {
	ID          string `json:"reasoningEffort"`
	Description string `json:"description"`
}

// ExecutionCatalog returns only combinations advertised by model/list. Ultra
// remains the provider's exact effort value, but is classified as
// orchestration because this app-server version defines it as automatic task
// delegation and deprecates multiAgentMode in its favor.
func (a *Adapter) ExecutionCatalog(ctx context.Context) (provider.ExecutionCatalog, error) {
	var result provider.ExecutionCatalog
	cursor := ""
	seen := make(map[string]struct{})
	for {
		params := map[string]any{"includeHidden": false, "limit": modelCatalogPageSize}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page modelListResponse
		if err := a.rpc.Call(ctx, "model/list", params, &page); err != nil {
			return provider.ExecutionCatalog{}, mapCallError(err)
		}
		for _, value := range page.Data {
			if value.Hidden {
				continue
			}
			modelID := strings.TrimSpace(value.Model)
			if modelID == "" {
				modelID = strings.TrimSpace(value.ID)
			}
			if modelID == "" {
				continue
			}
			model := provider.Model{
				ID:                     modelID,
				DisplayName:            value.DisplayName,
				Description:            value.Description,
				DefaultReasoningEffort: value.DefaultReasoningEffort,
			}
			for _, effort := range value.ReasoningEfforts {
				id := strings.TrimSpace(effort.ID)
				if id == "" {
					continue
				}
				kind := provider.ReasoningEffortStandard
				if id == "ultra" {
					kind = provider.ReasoningEffortOrchestration
				}
				model.ReasoningEfforts = append(model.ReasoningEfforts, provider.ReasoningEffort{
					ID: id, Description: effort.Description, Kind: kind,
				})
			}
			result.Models = append(result.Models, model)
			if value.IsDefault {
				result.DefaultModel = modelID
			}
		}
		next := strings.TrimSpace(page.NextCursor)
		if next == "" {
			break
		}
		if _, duplicate := seen[next]; duplicate {
			return provider.ExecutionCatalog{}, errors.New("Codex model catalog returned a repeated cursor")
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return result, nil
}
