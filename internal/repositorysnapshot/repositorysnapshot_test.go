package repositorysnapshot_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bridgegit "github.com/berkayahi/agentbridge/internal/git"
	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	"github.com/berkayahi/agentbridge/internal/security"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

func TestSnapshotIsExactDeterministicReadOnlyRedactedAndRestartSafe(t *testing.T) {
	ctx := context.Background()
	checkout := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	git := &recordingGit{runner: bridgegit.Runner{
		MaxOutputBytes: repositorysnapshot.MaxGitCommandOutput,
		Redactor: security.NewRedactor(security.Config{
			MaxFieldRunes: repositorysnapshot.MaxGitCommandOutput, MaxPayloadRunes: repositorysnapshot.MaxGitCommandOutput,
		}),
	}}
	runGit(t, git, checkout, "init", "-b", "main")
	runGit(t, git, checkout, "config", "user.name", "Snapshot Test")
	runGit(t, git, checkout, "config", "user.email", "snapshot@example.invalid")

	writeFile(t, checkout, "go.mod", "module example.invalid/snapshot\n\ngo 1.26\n")
	writeFile(t, checkout, "go.sum", "")
	writeFile(t, checkout, "package.json", `{
  "workspaces": ["web/*"],
  "scripts": {
    "build": "echo BUILD_CANARY_VALUE",
    "test": "echo TEST_CANARY_VALUE",
    "publish-secret": "echo SHOULD_NOT_BE_A_CANDIDATE"
  }
}`)
	writeFile(t, checkout, "pnpm-lock.yaml", "lockfileVersion: '9.0'\n")
	writeFile(t, checkout, ".env.example", "DATABASE_URL=postgres://canary-user:canary-password@host/db\nAPI_TOKEN=canary-token\n")
	writeFile(t, checkout, "Dockerfile", "ARG BUILD_TOKEN=canary-build-token\nFROM scratch\n")
	writeFile(t, checkout, "compose.yaml", "services:\n  api:\n    environment:\n      DATABASE_URL: ${DATABASE_URL:-canary-compose-value}\n")
	writeFile(t, checkout, ".github/workflows/test.yml", "name: test\n")
	writeFile(t, checkout, "db/migrations/001_create.sql", "SELECT 'canary-migration-value';\n")
	writeFile(t, checkout, "openapi.yaml", "openapi: 3.1.0\n")
	writeFile(t, checkout, "Makefile", "test:\n\t@echo CANARY_RECIPE_VALUE\nlint:\n\t@echo lint\n")
	writeFile(t, checkout, "services/api/go.mod", "module example.invalid/scoped\n")
	if err := os.Symlink(".env.example", filepath.Join(checkout, "config.example.env")); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, checkout, "add", ".")
	runGit(t, git, checkout, "commit", "-m", "test: repository snapshot fixture")
	baseSHA := runGit(t, git, checkout, "rev-parse", "HEAD")
	runGit(t, git, checkout, "update-index", "--add", "--cacheinfo", "160000,"+baseSHA+",vendor/module")
	runGit(t, git, checkout, "commit", "-m", "test: add unsupported gitlink")
	exactSHA := runGit(t, git, checkout, "rev-parse", "HEAD")

	writeFile(t, checkout, ".env.example", "NEW_SECRET=must-not-appear\n")
	runGit(t, git, checkout, "add", ".env.example")
	runGit(t, git, checkout, "commit", "-m", "test: advance repository")
	writeFile(t, checkout, "local.secret", "untracked-canary-value\n")
	statusBefore := runGit(t, git, checkout, "status", "--porcelain=v1", "-z")
	git.reset()

	dbPath := filepath.Join(t.TempDir(), "agentbridge.db")
	data, err := sqlite.OpenV2(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 31, 8, 0, 0, 0, time.UTC)
	catalog := fixedCatalog{profile: repositorysnapshot.ConfiguredRepository{
		ProfileID: "fixture", CheckoutPath: checkout, AllowedRef: "refs/heads/main",
	}}
	service := newService(t, data, catalog, git, now, func() string { return "repository-snapshot-fixed" })
	request := repositorysnapshot.Request{
		RepositoryProfileID: "fixture", RequestedRef: exactSHA, ScopedRoot: ".",
		AnalyzerVersion: "fixture-analyzer-v1", IdempotencyKey: "snapshot-key",
	}
	first, err := service.Snapshot(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExactCommitSHA != exactSHA || len(first.ExactCommitSHA) != 40 {
		t.Fatalf("exact commit = %q, want %q", first.ExactCommitSHA, exactSHA)
	}
	if first.OperationID != "repository-snapshot-fixed" || first.ResultDigest == "" {
		t.Fatalf("identity/digest missing: %#v", first)
	}
	if first.Ref.Kind != "exact_commit" || first.Ref.AllowedRef != "" {
		t.Fatalf("exact ref metadata = %#v", first.Ref)
	}
	assertObservation(t, first, "environment-name", ".env.example", "DATABASE_URL")
	assertObservation(t, first, "environment-name", ".env.example", "API_TOKEN")
	assertObservation(t, first, "command-candidate", "package.json", "pnpm run test")
	assertObservation(t, first, "migration-directory", "db/migrations", "migration-directory")
	assertLimitation(t, first, "gitlink_not_inspected", "vendor/module")
	assertLimitation(t, first, "symlink_not_read", "config.example.env")
	if first.Bounds.TreeEntries == 0 || first.Bounds.SelectedBlobs == 0 ||
		first.Bounds.SelectedBlobBytes <= 0 || first.Bounds.MaxTreeEntries != repositorysnapshot.MaxTreeEntries {
		t.Fatalf("bounds = %#v", first.Bounds)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		checkout, "checkout_path", "canary-password", "canary-token",
		"canary-build-token", "canary-compose-value", "CANARY_RECIPE_VALUE",
		"BUILD_CANARY_VALUE", "TEST_CANARY_VALUE", "must-not-appear", "untracked-canary-value",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("snapshot leaked %q: %s", forbidden, encoded)
		}
	}
	statusAfter := runGit(t, git, checkout, "status", "--porcelain=v1", "-z")
	if statusAfter != statusBefore {
		t.Fatalf("repository status changed: before=%q after=%q", statusBefore, statusAfter)
	}
	for _, command := range git.commandsSinceReset() {
		if !slices.Contains([]string{"rev-parse", "cat-file", "ls-tree", "status"}, command) {
			t.Fatalf("unexpected process command %q; snapshot may execute repository code", command)
		}
	}

	second, err := service.Snapshot(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if string(mustJSON(t, first)) != string(mustJSON(t, second)) {
		t.Fatalf("same-key replay changed response:\nfirst=%s\nsecond=%s", mustJSON(t, first), mustJSON(t, second))
	}
	conflict := request
	conflict.AnalyzerVersion = "fixture-analyzer-v2"
	if _, err := service.Snapshot(ctx, conflict); !errors.Is(err, repositorysnapshot.ErrConflict) {
		t.Fatalf("different input replay error = %v, want ErrConflict", err)
	}
	audit, err := data.LoadRepositorySnapshot(ctx, request.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if audit.RepositoryProfileID != "fixture" || audit.RequestedRef != exactSHA ||
		audit.ScopedRoot != "." || audit.RequestDigest == "" ||
		audit.ResultDigest != first.ResultDigest || audit.Status != "completed" {
		t.Fatalf("persisted request/result audit = %#v", audit)
	}

	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.OpenV2(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restartIDs := []string{"repository-snapshot-new-key", "repository-snapshot-allowed-ref", "repository-snapshot-scoped"}
	restarted := newService(t, reopened, catalog, git, now.Add(time.Hour), func() string {
		id := restartIDs[0]
		restartIDs = restartIDs[1:]
		return id
	})
	replayed, err := restarted.Snapshot(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.OperationID != first.OperationID || replayed.ResultDigest != first.ResultDigest {
		t.Fatalf("restart replay = %#v, want identity/digest from %#v", replayed, first)
	}
	newKey := request
	newKey.IdempotencyKey = "snapshot-key-2"
	deterministic, err := restarted.Snapshot(ctx, newKey)
	if err != nil {
		t.Fatal(err)
	}
	if deterministic.OperationID == first.OperationID || deterministic.ResultDigest != first.ResultDigest {
		t.Fatalf("new operation identity/digest = %q/%q, prior %q/%q",
			deterministic.OperationID, deterministic.ResultDigest, first.OperationID, first.ResultDigest)
	}

	allowedRef := request
	allowedRef.RequestedRef = "refs/heads/main"
	allowedRef.IdempotencyKey = "allowed-ref"
	refResponse, err := restarted.Snapshot(ctx, allowedRef)
	if err != nil {
		t.Fatal(err)
	}
	if refResponse.Ref.Kind != "configured_ref" || refResponse.Ref.AllowedRef != "refs/heads/main" {
		t.Fatalf("configured ref metadata = %#v", refResponse.Ref)
	}
	scoped := request
	scoped.ScopedRoot = "services/api"
	scoped.IdempotencyKey = "scoped-root"
	scopedResponse, err := restarted.Snapshot(ctx, scoped)
	if err != nil {
		t.Fatal(err)
	}
	if scopedResponse.ScopedRoot != "services/api" || len(scopedResponse.Observations) == 0 {
		t.Fatalf("scoped response = %#v", scopedResponse)
	}
	for _, observation := range scopedResponse.Observations {
		if !strings.HasPrefix(observation.EvidencePath, "services/api/") {
			t.Fatalf("scope leaked unrelated evidence: %#v", observation)
		}
	}
	fileScope := request
	fileScope.ScopedRoot = "go.mod"
	fileScope.IdempotencyKey = "file-scope"
	if _, err := restarted.Snapshot(ctx, fileScope); !errors.Is(err, repositorysnapshot.ErrScopeNotFound) {
		t.Fatalf("file scope error = %v, want ErrScopeNotFound", err)
	}
}

func TestConfiguredRefSnapshotRefreshesRemoteWithoutMovingControlCheckout(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := filepath.Join(root, "source")
	checkout := filepath.Join(root, "control")
	git := bridgegit.Runner{MaxOutputBytes: repositorysnapshot.MaxGitCommandOutput}

	runGit(t, git, root, "init", "--bare", remote)
	runGit(t, git, root, "init", "-b", "main", source)
	runGit(t, git, source, "config", "user.name", "Snapshot Test")
	runGit(t, git, source, "config", "user.email", "snapshot@example.invalid")
	writeFile(t, source, "go.mod", "module example.invalid/original\n\ngo 1.26\n")
	runGit(t, git, source, "add", "go.mod")
	runGit(t, git, source, "commit", "-m", "test: original remote tree")
	runGit(t, git, source, "remote", "add", "origin", remote)
	runGit(t, git, source, "push", "origin", "HEAD:refs/heads/hive/landing")
	runGit(t, git, root, "clone", "--branch", "hive/landing", remote, checkout)
	staleHead := runGit(t, git, checkout, "rev-parse", "HEAD")

	writeFile(t, source, "go.mod", "module example.invalid/current\n\ngo 1.26\n")
	runGit(t, git, source, "add", "go.mod")
	runGit(t, git, source, "commit", "-m", "test: advance configured remote ref")
	currentHead := runGit(t, git, source, "rev-parse", "HEAD")
	runGit(t, git, source, "push", "origin", "HEAD:refs/heads/hive/landing")

	data, err := sqlite.OpenV2(ctx, filepath.Join(root, "agentbridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service := newService(t, data, fixedCatalog{profile: repositorysnapshot.ConfiguredRepository{
		ProfileID: "fixture", CheckoutPath: checkout, Remote: "origin", AllowedRef: "refs/heads/hive/landing",
	}}, git, time.Now().UTC(), func() string { return "remote-refresh-snapshot" })

	response, err := service.Snapshot(ctx, repositorysnapshot.Request{
		RepositoryProfileID: "fixture", RequestedRef: "refs/heads/hive/landing", ScopedRoot: ".",
		AnalyzerVersion: "fixture-analyzer-v1", IdempotencyKey: "remote-refresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ExactCommitSHA != currentHead {
		t.Fatalf("snapshot commit = %s, want current remote %s", response.ExactCommitSHA, currentHead)
	}
	if head := runGit(t, git, checkout, "rev-parse", "HEAD"); head != staleHead {
		t.Fatalf("control checkout moved from %s to %s", staleHead, head)
	}
	if status := runGit(t, git, checkout, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("control checkout became dirty: %q", status)
	}
}

func TestSnapshotRejectsTraversalAbsoluteNonNormalizedAndDisallowedRefs(t *testing.T) {
	data, err := sqlite.OpenV2(context.Background(), filepath.Join(t.TempDir(), "agentbridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := repositorysnapshot.New(repositorysnapshot.Config{
		Store: data, Catalog: fixedCatalog{}, Inspector: rejectionInspector{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"", "/absolute", "../escape", "a/../escape", "./nested", "nested/", "a\\b", "a\x00b"} {
		t.Run(strings.ReplaceAll(scope, "/", "_"), func(t *testing.T) {
			_, err := service.Snapshot(context.Background(), repositorysnapshot.Request{
				RepositoryProfileID: "fixture", RequestedRef: strings.Repeat("a", 40),
				ScopedRoot: scope, AnalyzerVersion: "analyzer-v1", IdempotencyKey: "scope-key",
			})
			if !errors.Is(err, repositorysnapshot.ErrInvalidRequest) {
				t.Fatalf("scope %q error = %v, want ErrInvalidRequest", scope, err)
			}
		})
	}
	_, err = service.Snapshot(context.Background(), repositorysnapshot.Request{
		RepositoryProfileID: "fixture", RequestedRef: "refs/heads/not-allowed",
		ScopedRoot: ".", AnalyzerVersion: "analyzer-v1", IdempotencyKey: "ref-key",
	})
	if !errors.Is(err, repositorysnapshot.ErrRefNotAllowed) {
		t.Fatalf("disallowed ref error = %v, want ErrRefNotAllowed", err)
	}
}

func TestGitInspectorFailsClosedWhenTreeOutputExceedsBound(t *testing.T) {
	sha := strings.Repeat("a", 40)
	runner := &scriptedGit{results: []bridgegit.RunResult{
		{Stdout: sha + "\n"},
		{Stdout: "tree\n"},
		{Stdout: "100644 blob " + sha + "\tgo.mod\x00…[TRUNCATED]"},
	}}
	_, err := (repositorysnapshot.GitInspector{Git: runner}).Inspect(
		context.Background(),
		repositorysnapshot.ConfiguredRepository{ProfileID: "fixture", CheckoutPath: "/internal/path", AllowedRef: "refs/heads/main"},
		repositorysnapshot.Request{RequestedRef: sha, ScopedRoot: "."},
	)
	if !errors.Is(err, repositorysnapshot.ErrBoundsExceeded) {
		t.Fatalf("oversized tree error = %v, want ErrBoundsExceeded", err)
	}
}

func TestGitInspectorIgnoresReplacementRefs(t *testing.T) {
	ctx := context.Background()
	checkout := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	git := bridgegit.Runner{MaxOutputBytes: repositorysnapshot.MaxGitCommandOutput}
	runGit(t, git, checkout, "init", "-b", "main")
	runGit(t, git, checkout, "config", "user.name", "Snapshot Test")
	runGit(t, git, checkout, "config", "user.email", "snapshot@example.invalid")

	writeFile(t, checkout, "go.mod", "module example.invalid/original\n\ngo 1.26\n")
	runGit(t, git, checkout, "add", "go.mod")
	runGit(t, git, checkout, "commit", "-m", "test: original tree")
	originalCommit := runGit(t, git, checkout, "rev-parse", "HEAD")

	if err := os.Remove(filepath.Join(checkout, "go.mod")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, checkout, "package.json", `{"scripts":{"test":"echo replacement"}}`)
	runGit(t, git, checkout, "add", "-A")
	runGit(t, git, checkout, "commit", "-m", "test: replacement tree")
	replacementCommit := runGit(t, git, checkout, "rev-parse", "HEAD")
	runGit(t, git, checkout, "replace", originalCommit, replacementCommit)

	apparentTree := runGit(t, git, checkout, "ls-tree", "-r", "--name-only", originalCommit)
	if !strings.Contains(apparentTree, "package.json") || strings.Contains(apparentTree, "go.mod") {
		t.Fatalf("replacement ref did not falsify the unguarded control tree: %q", apparentTree)
	}

	objectsBefore := objectStoreState(t, filepath.Join(checkout, ".git", "objects"))
	response, err := (repositorysnapshot.GitInspector{Git: git}).Inspect(
		ctx,
		repositorysnapshot.ConfiguredRepository{
			ProfileID: "fixture", CheckoutPath: checkout, AllowedRef: "refs/heads/main",
		},
		repositorysnapshot.Request{RequestedRef: originalCommit, ScopedRoot: "."},
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.ExactCommitSHA != originalCommit {
		t.Fatalf("exact commit = %q, want unreplaced object %q", response.ExactCommitSHA, originalCommit)
	}
	if !hasEvidencePath(response, "go.mod") {
		t.Fatalf("unreplaced tree evidence is missing: %#v", response.Observations)
	}
	if hasEvidencePath(response, "package.json") {
		t.Fatalf("replacement tree falsified snapshot evidence: %#v", response.Observations)
	}
	if objectsAfter := objectStoreState(t, filepath.Join(checkout, ".git", "objects")); objectsAfter != objectsBefore {
		t.Fatal("replacement-safe inspection mutated the Git object store")
	}
}

func TestGitInspectorDoesNotLazyFetchMissingPromisorObjects(t *testing.T) {
	git := bridgegit.Runner{MaxOutputBytes: repositorysnapshot.MaxGitCommandOutput}
	root, remote, commit, blob := newPromisorFixture(t, git)
	guardedClone := clonePromisorRepository(t, git, root, remote, "guarded")

	if _, err := git.RunWithEnvironment(
		context.Background(), guardedClone,
		[]string{"GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1"},
		"cat-file", "-e", blob,
	); err == nil {
		t.Skip("Git clone did not leave the filtered blob missing")
	}
	objectsBefore := objectStoreState(t, filepath.Join(guardedClone, ".git", "objects"))
	response, err := (repositorysnapshot.GitInspector{Git: git}).Inspect(
		context.Background(),
		repositorysnapshot.ConfiguredRepository{
			ProfileID: "fixture", CheckoutPath: guardedClone, AllowedRef: "refs/heads/main",
		},
		repositorysnapshot.Request{RequestedRef: commit, ScopedRoot: "."},
	)
	if err == nil {
		t.Fatalf("snapshot unexpectedly succeeded with a missing promisor blob: %#v", response)
	}
	if objectsAfter := objectStoreState(t, filepath.Join(guardedClone, ".git", "objects")); objectsAfter != objectsBefore {
		t.Fatal("snapshot lazily fetched into the Git object store")
	}

	controlClone := clonePromisorRepository(t, git, root, remote, "unguarded-control")
	if _, err := git.RunWithEnvironment(
		context.Background(), controlClone,
		[]string{"GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1"},
		"cat-file", "-e", blob,
	); err == nil {
		t.Skip("Git control clone did not leave the filtered blob missing")
	}
	controlBefore := objectStoreState(t, filepath.Join(controlClone, ".git", "objects"))
	if _, err := git.Run(context.Background(), controlClone, "cat-file", "-s", blob); err != nil {
		t.Fatalf("unguarded control read did not lazy-fetch the blob: %v", err)
	}
	if controlAfter := objectStoreState(t, filepath.Join(controlClone, ".git", "objects")); controlAfter == controlBefore {
		t.Fatal("unguarded control read did not demonstrate object-store mutation")
	}
}

type fixedCatalog struct {
	profile repositorysnapshot.ConfiguredRepository
}

func (c fixedCatalog) ResolveRepositoryProfile(_ context.Context, id string) (repositorysnapshot.ConfiguredRepository, error) {
	if c.profile.ProfileID == "" {
		return repositorysnapshot.ConfiguredRepository{
			ProfileID: id, CheckoutPath: "/internal/path", AllowedRef: "refs/heads/main",
		}, nil
	}
	if id != c.profile.ProfileID {
		return repositorysnapshot.ConfiguredRepository{}, repositorysnapshot.ErrNotConfigured
	}
	return c.profile, nil
}

type rejectionInspector struct{}

func (rejectionInspector) Inspect(context.Context, repositorysnapshot.ConfiguredRepository, repositorysnapshot.Request) (repositorysnapshot.Response, error) {
	return repositorysnapshot.Response{}, errors.New("inspector should not be called")
}

type recordingGit struct {
	mu       sync.Mutex
	runner   bridgegit.Runner
	commands []string
}

func (g *recordingGit) Run(ctx context.Context, dir string, args ...string) (bridgegit.RunResult, error) {
	g.mu.Lock()
	if len(args) > 0 {
		g.commands = append(g.commands, args[0])
	}
	g.mu.Unlock()
	return g.runner.Run(ctx, dir, args...)
}

func (g *recordingGit) RunWithEnvironment(ctx context.Context, dir string, environment []string, args ...string) (bridgegit.RunResult, error) {
	g.mu.Lock()
	if len(args) > 0 {
		g.commands = append(g.commands, args[0])
	}
	g.mu.Unlock()
	return g.runner.RunWithEnvironment(ctx, dir, environment, args...)
}

func (g *recordingGit) reset() {
	g.mu.Lock()
	g.commands = nil
	g.mu.Unlock()
}

func (g *recordingGit) commandsSinceReset() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.commands...)
}

type scriptedGit struct {
	results []bridgegit.RunResult
}

func (g *scriptedGit) RunWithEnvironment(context.Context, string, []string, ...string) (bridgegit.RunResult, error) {
	if len(g.results) == 0 {
		return bridgegit.RunResult{}, errors.New("unexpected Git command")
	}
	result := g.results[0]
	g.results = g.results[1:]
	return result, nil
}

func newService(t *testing.T, data *sqlite.RuntimeStore, catalog repositorysnapshot.Catalog, git repositorysnapshot.GitRunner, now time.Time, newID func() string) *repositorysnapshot.Service {
	t.Helper()
	service, err := repositorysnapshot.New(repositorysnapshot.Config{
		Store: data, Catalog: catalog, Inspector: repositorysnapshot.GitInspector{Git: git},
		Clock: func() time.Time { return now }, NewID: newID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func runGit(t *testing.T, git interface {
	Run(context.Context, string, ...string) (bridgegit.RunResult, error)
}, dir string, args ...string) string {
	t.Helper()
	result, err := git.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(result.Stdout)
}

func writeFile(t *testing.T, root, name, contents string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertObservation(t *testing.T, response repositorysnapshot.Response, detector, evidencePath, observation string) {
	t.Helper()
	for _, value := range response.Observations {
		if value.DetectorID == detector && value.EvidencePath == evidencePath && value.Observation == observation {
			return
		}
	}
	t.Fatalf("missing observation %q/%q/%q in %#v", detector, evidencePath, observation, response.Observations)
}

func assertLimitation(t *testing.T, response repositorysnapshot.Response, code, evidencePath string) {
	t.Helper()
	for _, value := range response.Limitations {
		if value.Code == code && value.EvidencePath == evidencePath {
			return
		}
	}
	t.Fatalf("missing limitation %q/%q in %#v", code, evidencePath, response.Limitations)
}

func hasEvidencePath(response repositorysnapshot.Response, evidencePath string) bool {
	for _, observation := range response.Observations {
		if observation.EvidencePath == evidencePath {
			return true
		}
	}
	return false
}

func newPromisorFixture(t *testing.T, git bridgegit.Runner) (root, remote, commit, blob string) {
	t.Helper()
	root = t.TempDir()
	source := filepath.Join(root, "source")
	remote = filepath.Join(root, "remote.git")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, root, "init", "--bare", remote)
	runGit(t, git, remote, "config", "uploadpack.allowFilter", "true")
	runGit(t, git, source, "init", "-b", "main")
	runGit(t, git, source, "config", "user.name", "Snapshot Test")
	runGit(t, git, source, "config", "user.email", "snapshot@example.invalid")
	writeFile(t, source, "package.json", `{"scripts":{"test":"echo promisor"}}`)
	runGit(t, git, source, "add", "package.json")
	runGit(t, git, source, "commit", "-m", "test: promisor fixture")
	commit = runGit(t, git, source, "rev-parse", "HEAD")
	blob = runGit(t, git, source, "rev-parse", "HEAD:package.json")
	runGit(t, git, source, "remote", "add", "origin", remote)
	runGit(t, git, source, "push", "origin", "HEAD:refs/heads/main")
	return root, remote, commit, blob
}

func clonePromisorRepository(t *testing.T, git bridgegit.Runner, root, remote, name string) string {
	t.Helper()
	clone := filepath.Join(root, name)
	runGit(t, git, root, "-c", "protocol.file.allow=always", "clone",
		"--filter=blob:none", "--no-checkout", "--no-local", remote, clone)
	promisor := runGit(t, git, clone, "config", "--get", "remote.origin.promisor")
	if promisor != "true" {
		t.Skip("installed Git does not support a local partial/promisor clone")
	}
	return clone
}

func objectStoreState(t *testing.T, root string) string {
	t.Helper()
	var state strings.Builder
	err := filepath.WalkDir(root, func(file string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(contents)
		_, _ = fmt.Fprintf(&state, "%s %d %x\n", filepath.ToSlash(relative), len(contents), digest)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state.String()
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
