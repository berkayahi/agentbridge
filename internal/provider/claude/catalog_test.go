package claude

import (
	"slices"
	"testing"
)

func TestClaudeCatalogPreservesLiveSelectorsAndPerModelCapabilities(t *testing.T) {
	var response catalogResponse
	response.Type = "control_response"
	response.Response.Subtype = "success"
	response.Response.Response.Models = append(response.Response.Response.Models,
		catalogModel{
			Value: "opus[1m]", DisplayName: "Opus", Description: "Opus 4.8 with 1M context",
			SupportsEffort: true, SupportedEffortLevels: []string{"low", "high", "xhigh", "max"}, SupportsAutoMode: true,
		},
		catalogModel{Value: "haiku", DisplayName: "Haiku", Description: "Haiku 4.5"},
	)

	catalog := claudeCatalog("opus", response)
	if catalog.DefaultModel != "opus" || !slices.Contains(catalog.ModelAliases, "best") {
		t.Fatalf("catalog defaults and aliases = %#v", catalog)
	}
	if catalog.Models[0].DefaultReasoningEffort != "xhigh" ||
		!slices.Contains(catalog.Models[0].ApprovalModes, "auto") {
		t.Fatalf("Opus profile = %#v", catalog.Models[0])
	}
	if len(catalog.Models[1].ReasoningEfforts) != 0 ||
		slices.Contains(catalog.Models[1].ApprovalModes, "auto") {
		t.Fatalf("Haiku profile = %#v", catalog.Models[1])
	}
}
