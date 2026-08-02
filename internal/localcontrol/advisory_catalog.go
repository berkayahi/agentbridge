package localcontrol

import (
	"context"
	"strings"

	"github.com/berkayahi/agentbridge/internal/advisory"
)

// ConfiguredAdvisoryCatalog adapts the existing configured provider catalog to
// the generic advisory capability view. A provider may be advisory-available
// without a task runtime when its explicit capability attestation says so;
// eligibility still comes from that provider capability.
type ConfiguredAdvisoryCatalog struct {
	Catalog   ProviderCatalog
	Providers map[string]advisory.Provider
}

func (c ConfiguredAdvisoryCatalog) ProviderProfiles(ctx context.Context) ([]advisory.ProviderProfile, error) {
	if c.Catalog == nil {
		return nil, ErrNotConfigured
	}
	configured, err := c.Catalog.ProviderProfiles(ctx)
	if err != nil {
		return nil, err
	}
	profiles := make([]advisory.ProviderProfile, 0, len(configured))
	for _, value := range configured {
		provider, ok := c.Providers[value.ID]
		if !ok || provider == nil {
			continue
		}
		capability := provider.Capability()
		modelID := strings.TrimSpace(value.DefaultModel)
		if modelID == "" {
			if modelProvider, ok := provider.(interface{ ConfiguredModel() string }); ok {
				modelID = strings.TrimSpace(modelProvider.ConfiguredModel())
			}
		}
		modelIDs := append([]string(nil), value.Models...)
		if modelID != "" && !containsModel(modelIDs, modelID) {
			modelIDs = append(modelIDs, modelID)
		}
		profiles = append(profiles, advisory.ProviderProfile{
			ID: value.ID, ModelID: modelID, ModelIDs: modelIDs,
			ModelAliases: append([]string(nil), value.ModelAliases...),
			Available:    advisory.CapabilityEligible(capability, false) && (value.Available || capability.AdvisoryExecution),
			Capabilities: capability,
		})
	}
	return profiles, nil
}

func containsModel(models []string, wanted string) bool {
	for _, model := range models {
		if strings.TrimSpace(model) == wanted {
			return true
		}
	}
	return false
}
