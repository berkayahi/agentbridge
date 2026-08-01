package localcontrol

import (
	"context"

	"github.com/berkayahi/agentbridge/internal/advisory"
)

// ConfiguredAdvisoryCatalog adapts the existing configured provider catalog to
// the generic advisory capability view. Availability comes from configuration;
// eligibility comes from the provider's explicit capability attestation.
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
		profiles = append(profiles, advisory.ProviderProfile{
			ID: value.ID, ModelID: value.DefaultModel, Available: value.Available,
			Capability: provider.Capability(),
		})
	}
	return profiles, nil
}
