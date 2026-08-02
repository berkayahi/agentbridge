package localcontrol_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/advisory"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

type fakeProviderCatalog struct {
	providers []localcontrol.ProviderInfo
	err       error
}

func (c fakeProviderCatalog) ProviderProfiles(context.Context) ([]localcontrol.ProviderInfo, error) {
	return c.providers, c.err
}

func newProviderService(t *testing.T, catalog localcontrol.ProviderCatalog) *localcontrol.Service {
	t.Helper()
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	now := time.Unix(1_700_000_000, 0).UTC()
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Providers: catalog,
		Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// A client must be able to offer the runtimes this host actually has, with the
// configured default model, instead of hardcoding a provider list.
func TestListProvidersReportsConfiguredRuntimes(t *testing.T) {
	service := newProviderService(t, fakeProviderCatalog{providers: []localcontrol.ProviderInfo{
		{ID: "claude", DefaultModel: "opus", Available: true, Capabilities: advisory.ReadOnlyCapability("claude")},
		{ID: "codex", DefaultModel: "gpt-5.6-terra", Available: false, Capabilities: advisory.IneligibleCapability("codex", "runtime_unavailable", "runtime is unavailable")},
	}})
	response, err := service.ListProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Providers) != 2 {
		t.Fatalf("providers = %#v", response.Providers)
	}
	if response.Providers[0].ID != "claude" || response.Providers[0].DefaultModel != "opus" || !response.Providers[0].Available {
		t.Fatalf("first provider = %#v", response.Providers[0])
	}
	// An installed-but-missing executable must be reported, not hidden: the
	// keeper needs to know why a runtime cannot be chosen.
	if response.Providers[1].Available {
		t.Fatalf("second provider = %#v, want Available false", response.Providers[1])
	}
	if response.ContractVersion != advisory.CapabilityContractVersion || !response.Providers[0].Capabilities.Valid() || !response.Providers[1].Capabilities.Valid() {
		t.Fatalf("provider capability contract = %#v", response)
	}
}

func TestListProvidersRequiresACatalog(t *testing.T) {
	service := newProviderService(t, nil)
	if _, err := service.ListProviders(context.Background()); !errors.Is(err, localcontrol.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestListProvidersSerializesCanonicalCapabilityContract(t *testing.T) {
	service := newProviderService(t, fakeProviderCatalog{providers: []localcontrol.ProviderInfo{
		{ID: "codex", DefaultModel: "deterministic-v1", Available: false, Capabilities: advisory.ReadOnlyCapability("codex")},
	}})
	response, err := service.ListProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"contract_version":"`+advisory.CapabilityContractVersion+`"`) || !strings.Contains(string(encoded), `"provider_id":"codex"`) {
		t.Fatalf("serialized capability contract = %s", encoded)
	}
	for _, legacy := range []string{"advisory_sessions", "decision_mutation", `"capability"`} {
		if strings.Contains(string(encoded), legacy) {
			t.Fatalf("legacy capability field %q crossed the wire: %s", legacy, encoded)
		}
	}
}
