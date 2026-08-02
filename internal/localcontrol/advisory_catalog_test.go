package localcontrol_test

import (
	"context"
	"testing"

	"github.com/berkayahi/agentbridge/internal/advisory"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

func TestConfiguredAdvisoryCatalogUsesAttestedAvailabilityAndModel(t *testing.T) {
	provider := advisory.DeterministicProvider{ProviderID: "fixture", ModelID: "deterministic-v1"}
	catalog := localcontrol.ConfiguredAdvisoryCatalog{
		Catalog:   fakeProviderCatalog{providers: []localcontrol.ProviderInfo{{ID: "fixture", Available: false}}},
		Providers: map[string]advisory.Provider{"fixture": provider},
	}
	profiles, err := catalog.ProviderProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || !profiles[0].Available || profiles[0].ModelID != "deterministic-v1" || len(profiles[0].ModelIDs) != 1 || profiles[0].ModelIDs[0] != "deterministic-v1" {
		t.Fatalf("advisory profiles = %#v", profiles)
	}
}
