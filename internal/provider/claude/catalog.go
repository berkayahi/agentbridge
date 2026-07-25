package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/isolation"
	"github.com/berkayahi/agentbridge/internal/provider"
)

type catalogResponse struct {
	Type     string `json:"type"`
	Response struct {
		Subtype  string `json:"subtype"`
		Response struct {
			Models []catalogModel `json:"models"`
		} `json:"response"`
	} `json:"response"`
}

type catalogModel struct {
	Value                 string   `json:"value"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
	SupportsAutoMode      bool     `json:"supportsAutoMode"`
}

// ExecutionCatalog asks Claude Code's SDK initialization handshake for the
// models available to the current account and policy. This is deliberately a
// no-turn probe: stdin closes after the control request, no model request is
// made, and no resumable session is persisted.
func (a *Adapter) ExecutionCatalog(ctx context.Context) (provider.ExecutionCatalog, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	executable := strings.TrimSpace(a.process.Executable)
	if executable == "" {
		executable = "claude"
	}
	request := []byte(`{"type":"control_request","request_id":"agentbridge-catalog","request":{"subtype":"initialize","hooks":{},"sdkMcpServers":{},"agents":{}}}` + "\n")
	command := exec.CommandContext(probeCtx, executable,
		"-p", "--verbose",
		"--input-format", "stream-json", "--output-format", "stream-json",
		"--permission-mode", "dontAsk",
		"--safe-mode", "--no-session-persistence",
	)
	command.Stdin = bytes.NewReader(request)
	command.Dir = a.process.Dir
	baseEnvironment := a.process.Environment
	if baseEnvironment == nil {
		baseEnvironment = os.Environ()
	}
	configDir := strings.TrimSpace(a.process.ClaudeConfigDir)
	if configDir == "" {
		for _, entry := range baseEnvironment {
			name, value, found := strings.Cut(entry, "=")
			if found && name == "CLAUDE_CONFIG_DIR" {
				configDir = value
				break
			}
		}
	}
	extra := make(map[string]string)
	if configDir != "" {
		extra["CLAUDE_CONFIG_DIR"] = configDir
	}
	command.Env = isolation.FilterEnvironment(baseEnvironment, isolation.EnvironmentPolicy{Extra: extra})
	output, err := command.Output()
	if err != nil {
		return provider.ExecutionCatalog{}, fmt.Errorf("inspect Claude execution catalog: %w", err)
	}
	var response catalogResponse
	if err := json.NewDecoder(bytes.NewReader(output)).Decode(&response); err != nil {
		return provider.ExecutionCatalog{}, fmt.Errorf("decode Claude execution catalog: %w", err)
	}
	if response.Type != "control_response" || response.Response.Subtype != "success" || len(response.Response.Response.Models) == 0 {
		return provider.ExecutionCatalog{}, fmt.Errorf("inspect Claude execution catalog: invalid initialization response")
	}
	return claudeCatalog(a.process.Model, response), nil
}

func claudeCatalog(configuredDefault string, response catalogResponse) provider.ExecutionCatalog {
	result := provider.ExecutionCatalog{
		DefaultModel:        "default",
		DefaultApprovalMode: "auto",
		ApprovalModes: []provider.ApprovalMode{
			{ID: "default", Description: "Ask before edits and other actions that require permission."},
			{ID: "acceptEdits", Description: "Accept file edits and common filesystem operations automatically."},
			{ID: "plan", Description: "Analyze and propose a plan without editing."},
			{ID: "auto", Description: "Run with Claude Code's background safety classifier."},
			{ID: "dontAsk", Description: "Deny actions that are not pre-approved instead of prompting."},
			{ID: "bypassPermissions", Description: "Bypass permission prompts and safety checks."},
		},
	}
	for _, available := range response.Response.Response.Models {
		id := strings.TrimSpace(available.Value)
		if id == "" {
			continue
		}
		model := provider.Model{
			ID: id, DisplayName: strings.TrimSpace(available.DisplayName),
			Description:   strings.TrimSpace(available.Description),
			ApprovalModes: []string{"default", "acceptEdits", "plan", "dontAsk", "bypassPermissions"},
		}
		if available.SupportsAutoMode {
			model.ApprovalModes = append(model.ApprovalModes, "auto")
		}
		if available.SupportsEffort {
			for _, effort := range available.SupportedEffortLevels {
				effort = strings.TrimSpace(effort)
				if effort == "" {
					continue
				}
				model.ReasoningEfforts = append(model.ReasoningEfforts, provider.ReasoningEffort{
					ID: effort, Description: claudeEffortDescription(effort), Kind: provider.ReasoningEffortStandard,
				})
			}
			model.DefaultReasoningEffort = claudeDefaultEffort(id, model.Description, model.ReasoningEfforts)
		}
		model.Aliases = claudeModelAliases(id)
		result.Models = append(result.Models, model)
		for _, alias := range append([]string{id}, model.Aliases...) {
			if claudeAlias(alias) && !slices.Contains(result.ModelAliases, alias) {
				result.ModelAliases = append(result.ModelAliases, alias)
			}
		}
	}
	configuredDefault = strings.TrimSpace(configuredDefault)
	if configuredDefault != "" {
		for _, model := range result.Models {
			if model.ID == configuredDefault || slices.Contains(model.Aliases, configuredDefault) {
				result.DefaultModel = configuredDefault
				break
			}
		}
	}
	return result
}

func claudeModelAliases(model string) []string {
	switch {
	case strings.HasPrefix(model, "claude-fable-"):
		return []string{"fable"}
	case model == "opus[1m]":
		return []string{"best", "opus"}
	case strings.HasPrefix(model, "claude-opus-"):
		return []string{"best", "opus"}
	case strings.HasPrefix(model, "claude-sonnet-"):
		return []string{"sonnet"}
	case strings.HasPrefix(model, "claude-haiku-"):
		return []string{"haiku"}
	default:
		return nil
	}
}

func claudeAlias(value string) bool {
	return value != "" && value != "default" && !strings.HasPrefix(value, "claude-")
}

func claudeDefaultEffort(model, description string, efforts []provider.ReasoningEffort) string {
	lower := strings.ToLower(model + " " + description)
	if (strings.Contains(lower, "opus 4.8") || strings.Contains(lower, "opus 4.7") || strings.HasPrefix(lower, "opus")) &&
		hasClaudeEffort(efforts, "xhigh") {
		return "xhigh"
	}
	for _, effort := range []string{"high", "medium", "low"} {
		if hasClaudeEffort(efforts, effort) {
			return effort
		}
	}
	return ""
}

func hasClaudeEffort(efforts []provider.ReasoningEffort, id string) bool {
	for _, effort := range efforts {
		if effort.ID == id {
			return true
		}
	}
	return false
}

func claudeEffortDescription(effort string) string {
	switch effort {
	case "low":
		return "Quick, straightforward implementation with minimal overhead."
	case "medium":
		return "Balanced implementation with standard testing."
	case "high":
		return "Comprehensive implementation with extensive testing."
	case "xhigh":
		return "Extended reasoning with thorough analysis."
	case "max":
		return "Maximum capability with the deepest reasoning."
	default:
		return ""
	}
}

var _ provider.ExecutionCatalogProvider = (*Adapter)(nil)
