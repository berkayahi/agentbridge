package localcontrol_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

// fakeCatalog in service_test.go is the runtime catalog; this is the repository
// catalog, which reports the configured, executable repository profiles.
type fakeRepositoryCatalog struct {
	profiles []localcontrol.RepositoryProfile
	err      error
}

func (c fakeRepositoryCatalog) RepositoryProfiles(context.Context) ([]localcontrol.RepositoryProfile, error) {
	return c.profiles, c.err
}

func newCatalogService(t *testing.T, catalog localcontrol.RepositoryCatalog) *localcontrol.Service {
	t.Helper()
	ctx := context.Background()
	data, err := sqlite.OpenV2Runtime(ctx, filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	now := time.Unix(1_700_000_000, 0).UTC()
	service, err := localcontrol.New(localcontrol.Config{
		Store: data, Runtimes: fakeCatalog{}, Repositories: catalog,
		Clock: func() time.Time { return now }, NewID: deterministicIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// A repository id is only usable if the executor can resolve it against a
// configured profile, so registration must return the configured profile id
// instead of minting an id that can never start a task.
func TestRegisterRepositoryResolvesConfiguredProfile(t *testing.T) {
	service := newCatalogService(t, fakeRepositoryCatalog{profiles: []localcontrol.RepositoryProfile{
		{ID: "first-flight", Remote: "origin", BaseRef: "refs/heads/staging"},
		{ID: "other", Remote: "upstream"},
	}})
	response, err := service.RegisterRepository(context.Background(), localcontrol.RegisterRepositoryRequest{
		Remote: "origin", IdempotencyKey: "repository-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Repository.ID != "first-flight" {
		t.Fatalf("repository id = %q, want the configured profile id %q", response.Repository.ID, "first-flight")
	}
	if response.Repository.Remote != "origin" {
		t.Fatalf("repository remote = %q", response.Repository.Remote)
	}
}

func TestRegisterRepositoryRejectsUnconfiguredRemote(t *testing.T) {
	service := newCatalogService(t, fakeRepositoryCatalog{profiles: []localcontrol.RepositoryProfile{
		{ID: "first-flight", Remote: "origin"},
	}})
	if _, err := service.RegisterRepository(context.Background(), localcontrol.RegisterRepositoryRequest{
		Remote: "nowhere", IdempotencyKey: "repository-key",
	}); !errors.Is(err, localcontrol.ErrRepositoryNotConfigured) {
		t.Fatalf("err = %v, want ErrRepositoryNotConfigured", err)
	}
}

func TestRegisterRepositoryRejectsAmbiguousRemote(t *testing.T) {
	service := newCatalogService(t, fakeRepositoryCatalog{profiles: []localcontrol.RepositoryProfile{
		{ID: "one", Remote: "origin"},
		{ID: "two", Remote: "origin"},
	}})
	if _, err := service.RegisterRepository(context.Background(), localcontrol.RegisterRepositoryRequest{
		Remote: "origin", IdempotencyKey: "repository-key",
	}); !errors.Is(err, localcontrol.ErrRepositoryAmbiguous) {
		t.Fatalf("err = %v, want ErrRepositoryAmbiguous", err)
	}
}

// Registration is idempotent against the same configured profile: a second
// call with a fresh key must return the same id rather than a second binding.
func TestRegisterRepositoryIsStableAcrossKeys(t *testing.T) {
	service := newCatalogService(t, fakeRepositoryCatalog{profiles: []localcontrol.RepositoryProfile{
		{ID: "first-flight", Remote: "origin"},
	}})
	ctx := context.Background()
	first, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.RegisterRepository(ctx, localcontrol.RegisterRepositoryRequest{Remote: "origin", IdempotencyKey: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Repository.ID != second.Repository.ID {
		t.Fatalf("ids diverged: %q vs %q", first.Repository.ID, second.Repository.ID)
	}
}

// The desktop needs to offer real choices, so the catalog is readable.
func TestListRepositoriesReturnsConfiguredProfiles(t *testing.T) {
	service := newCatalogService(t, fakeRepositoryCatalog{profiles: []localcontrol.RepositoryProfile{
		{ID: "first-flight", Remote: "origin", BaseRef: "refs/heads/staging"},
		{ID: "other", Remote: "upstream", BaseRef: "refs/heads/main"},
	}})
	response, err := service.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Repositories) != 2 {
		t.Fatalf("repositories = %#v", response.Repositories)
	}
	if response.Repositories[0].ID != "first-flight" || response.Repositories[0].BaseRef != "refs/heads/staging" {
		t.Fatalf("first profile = %#v", response.Repositories[0])
	}
}

// Without a configured catalog the service must fail closed rather than hand
// out an id the executor cannot resolve.
func TestListRepositoriesRequiresACatalog(t *testing.T) {
	service := newCatalogService(t, nil)
	if _, err := service.ListRepositories(context.Background()); !errors.Is(err, localcontrol.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}
