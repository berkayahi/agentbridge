package repositorysnapshot_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bridgegit "github.com/berkayahi/agentbridge/internal/git"
	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/provider/fake"
	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestKnowledgeStatesAreExactlyTheStableFive(t *testing.T) {
	for _, state := range []repositorysnapshot.KnowledgeState{
		repositorysnapshot.KnowledgeObserved, repositorysnapshot.KnowledgeDeclared,
		repositorysnapshot.KnowledgeInferred, repositorysnapshot.KnowledgeUnknown,
		repositorysnapshot.KnowledgeConflicting,
	} {
		if !state.Valid() {
			t.Fatalf("state %q rejected", state)
		}
	}
	for _, state := range []repositorysnapshot.KnowledgeState{"conflict", "certain", "", "observed "} {
		if state.Valid() {
			t.Fatalf("unsupported state %q accepted", state)
		}
	}
}

func TestNativeAnalysisProviderFailsClosedWithoutExplicitSafeCapability(t *testing.T) {
	native := repositorysnapshot.NativeAnalysisProvider{Provider: fake.New(workmodel.CodexSubscription, provider.MustID("session-1"), nil)}
	_, err := native.Analyze(context.Background(), repositorysnapshot.ProviderRequest{
		Role: repositorysnapshot.RoleCartographer, ExactCommitSHA: strings.Repeat("a", 40),
		WorkspacePath: t.TempDir(), Prompt: "inspect",
	})
	if !errors.Is(err, repositorysnapshot.ErrProviderPolicy) {
		t.Fatalf("error = %v, want explicit policy rejection", err)
	}
}

func TestConfiguredUnderstandingPathReturnsTypedUnavailableWhenNoSafeProviderIsExposed(t *testing.T) {
	service, err := repositorysnapshot.NewUnderstandingService(repositorysnapshot.UnderstandingConfig{
		Store:   &understandingStore{operations: make(map[string]repositorysnapshot.UnderstandingOperation)},
		Catalog: understandingCatalog{}, Evidence: understandingEvidence{},
		Providers: map[string]repositorysnapshot.AnalysisProvider{}, DefaultProvider: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Understand(context.Background(), understandingRequest("typed-unavailable", repositorysnapshot.RoleCartographer))
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "not_configured" || response.ErrorCode != "provider_not_configured" || response.Provider.Status != "not_configured" {
		t.Fatalf("unavailable response = %#v", response)
	}
	if len(response.Findings) != 0 {
		t.Fatal("unavailable provider fabricated findings")
	}
}

func TestSynthesizerBindsExactSnapshotAndDurablePriorOutputs(t *testing.T) {
	store := &understandingStore{operations: make(map[string]repositorysnapshot.UnderstandingOperation)}
	service, err := repositorysnapshot.NewUnderstandingService(repositorysnapshot.UnderstandingConfig{
		Store: store, Catalog: understandingCatalog{}, Evidence: understandingEvidence{},
		Providers: map[string]repositorysnapshot.AnalysisProvider{"fixture": roleUnderstandingProvider{}}, DefaultProvider: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	refs := make(map[repositorysnapshot.AnalysisRole]repositorysnapshot.PriorOutputReference)
	for _, role := range []repositorysnapshot.AnalysisRole{repositorysnapshot.RoleCartographer, repositorysnapshot.RoleProductArchaeologist, repositorysnapshot.RoleQualityOperations} {
		response, err := service.Understand(context.Background(), understandingRequestWithCommit("fixture-repository", "prior-"+string(role), role, "0123456789abcdef0123456789abcdef01234567"))
		if err != nil {
			t.Fatal(err)
		}
		refs[role] = repositorysnapshot.PriorOutputReference{IdempotencyKey: "prior-" + string(role), ResultDigest: response.ResultDigest}
	}
	fixture, err := os.ReadFile("../../protocol/fixtures/v1/repository-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot repositorysnapshot.Response
	if err := json.Unmarshal(fixture, &snapshot); err != nil {
		t.Fatal(err)
	}
	synth, err := service.Understand(context.Background(), repositorysnapshot.UnderstandingRequest{
		RepositoryProfileID: "fixture-repository", ExpectedCommitSHA: snapshot.ExactCommitSHA, Role: repositorysnapshot.RoleSynthesizer,
		ProviderID: "fixture", IdempotencyKey: "synth", Snapshot: &snapshot, PriorOutputs: refs,
	})
	if err != nil || synth.Status != "completed" {
		t.Fatalf("synthesis = %#v err=%v", synth, err)
	}
	forged := snapshot
	forged.ResultDigest = "sha256:forged"
	bad := repositorysnapshot.UnderstandingRequest{
		RepositoryProfileID: "fixture-repository", ExpectedCommitSHA: forged.ExactCommitSHA, Role: repositorysnapshot.RoleSynthesizer,
		ProviderID: "fixture", IdempotencyKey: "synth-forged-snapshot", Snapshot: &forged, PriorOutputs: refs,
	}
	if _, err := service.Understand(context.Background(), bad); !errors.Is(err, repositorysnapshot.ErrCommitMismatch) {
		t.Fatalf("forged snapshot = %v, want digest rejection", err)
	}
	forgedRef := refs
	forgedRef[repositorysnapshot.RoleCartographer] = repositorysnapshot.PriorOutputReference{IdempotencyKey: "prior-" + string(repositorysnapshot.RoleCartographer), ResultDigest: "sha256:forged"}
	bad.PriorOutputs = forgedRef
	bad.IdempotencyKey = "synth-forged-prior"
	bad.Snapshot = &snapshot
	if _, err := service.Understand(context.Background(), bad); !errors.Is(err, repositorysnapshot.ErrConflict) {
		t.Fatalf("forged prior = %v, want binding conflict", err)
	}
}

func TestEvidenceReaderIsExactBoundedCommittedAndRedacted(t *testing.T) {
	root := t.TempDir()
	git := bridgegit.Runner{MaxOutputBytes: repositorysnapshot.MaxGitCommandOutput}
	runEvidenceGit(t, git, root, "init", "-b", "main")
	runEvidenceGit(t, git, root, "config", "user.name", "Evidence Test")
	runEvidenceGit(t, git, root, "config", "user.email", "evidence@example.invalid")
	writeEvidenceFile(t, root, "README.md", "committed-value\n")
	writeEvidenceFile(t, root, ".env.example", "API_TOKEN=do-not-leak\nDATABASE_URL=postgres://user:password@example.invalid/db\n")
	runEvidenceGit(t, git, root, "add", ".")
	runEvidenceGit(t, git, root, "commit", "-m", "test: evidence")
	commit := runEvidenceGit(t, git, root, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("working-tree-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := (repositorysnapshot.GitEvidenceReader{Git: git}).RetrieveEvidence(context.Background(), repositorysnapshot.ConfiguredRepository{
		ProfileID: "fixture", CheckoutPath: root,
	}, repositorysnapshot.EvidenceRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: commit, Paths: []string{"README.md", ".env.example"},
	})
	if !errors.Is(err, repositorysnapshot.ErrSecretLikeFile) {
		t.Fatalf("secret assignment = %v, want ErrSecretLikeFile", err)
	}
	packet, err := (repositorysnapshot.GitEvidenceReader{Git: git}).RetrieveEvidence(context.Background(), repositorysnapshot.ConfiguredRepository{
		ProfileID: "fixture", CheckoutPath: root,
	}, repositorysnapshot.EvidenceRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: commit, Paths: []string{"README.md"},
	})
	if err != nil || packet.ExactCommitSHA != commit || packet.ResultDigest == "" || packet.Files[0].Content != "committed-value\n" {
		t.Fatalf("packet = %#v err=%v", packet, err)
	}
	if strings.Contains(packet.Files[0].Content, "working-tree-value") {
		t.Fatal("evidence reader read the working tree")
	}
}

func TestEvidenceReaderRejectsUnsafePathsCommitMismatchSecretFilesAndBounds(t *testing.T) {
	root := t.TempDir()
	git := bridgegit.Runner{MaxOutputBytes: repositorysnapshot.MaxGitCommandOutput}
	runEvidenceGit(t, git, root, "init", "-b", "main")
	runEvidenceGit(t, git, root, "config", "user.name", "Evidence Test")
	runEvidenceGit(t, git, root, "config", "user.email", "evidence@example.invalid")
	writeEvidenceFile(t, root, "small.txt", "small")
	writeEvidenceFile(t, root, "large.txt", strings.Repeat("x", repositorysnapshot.MaxEvidenceBlob+1))
	writeEvidenceFile(t, root, "private.pem", "-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----\n")
	writeEvidenceFile(t, root, "secret.json", `{"api_key":"secret-value"}`)
	writeEvidenceFile(t, root, "secret.yaml", "password: 'secret-value'\n")
	if err := os.WriteFile(filepath.Join(root, "control.txt"), []byte("safe\x00content"), 0o600); err != nil {
		t.Fatal(err)
	}
	runEvidenceGit(t, git, root, "add", ".")
	runEvidenceGit(t, git, root, "commit", "-m", "test: bounds")
	commit := runEvidenceGit(t, git, root, "rev-parse", "HEAD")
	reader := repositorysnapshot.GitEvidenceReader{Git: git}
	profile := repositorysnapshot.ConfiguredRepository{ProfileID: "fixture", CheckoutPath: root}
	for _, path := range []string{"../escape", "/absolute", "a/../escape", "./small.txt", "small.txt/", "a\\b"} {
		_, err := reader.RetrieveEvidence(context.Background(), profile, repositorysnapshot.EvidenceRequest{
			RepositoryProfileID: "fixture", ExpectedCommitSHA: commit, Paths: []string{path},
		})
		if !errors.Is(err, repositorysnapshot.ErrPathNotAllowed) {
			t.Fatalf("path %q error = %v, want ErrPathNotAllowed", path, err)
		}
	}
	_, err := reader.RetrieveEvidence(context.Background(), profile, repositorysnapshot.EvidenceRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: strings.Repeat("a", 40), Paths: []string{"small.txt"},
	})
	if !errors.Is(err, repositorysnapshot.ErrCommitMismatch) {
		t.Fatalf("commit mismatch = %v", err)
	}
	_, err = reader.RetrieveEvidence(context.Background(), profile, repositorysnapshot.EvidenceRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: commit, Paths: []string{"private.pem"},
	})
	if !errors.Is(err, repositorysnapshot.ErrSecretLikeFile) {
		t.Fatalf("private file = %v", err)
	}
	_, err = reader.RetrieveEvidence(context.Background(), profile, repositorysnapshot.EvidenceRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: commit, Paths: []string{"large.txt"},
	})
	if !errors.Is(err, repositorysnapshot.ErrBoundsExceeded) {
		t.Fatalf("large file = %v", err)
	}
	for _, name := range []string{"secret.json", "secret.yaml"} {
		_, err := reader.RetrieveEvidence(context.Background(), profile, repositorysnapshot.EvidenceRequest{
			RepositoryProfileID: "fixture", ExpectedCommitSHA: commit, Paths: []string{name},
		})
		if !errors.Is(err, repositorysnapshot.ErrSecretLikeFile) {
			t.Fatalf("%s = %v, want ErrSecretLikeFile", name, err)
		}
	}
	_, err = reader.RetrieveEvidence(context.Background(), profile, repositorysnapshot.EvidenceRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: commit, Paths: []string{"control.txt"},
	})
	if !errors.Is(err, repositorysnapshot.ErrBinaryEvidence) {
		t.Fatalf("control bytes = %v, want ErrBinaryEvidence", err)
	}
	paths := make([]string, repositorysnapshot.MaxEvidencePaths+1)
	for index := range paths {
		paths[index] = "small-" + string(rune('a'+index)) + ".txt"
	}
	_, err = reader.RetrieveEvidence(context.Background(), profile, repositorysnapshot.EvidenceRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: commit, Paths: paths,
	})
	if !errors.Is(err, repositorysnapshot.ErrInvalidRequest) {
		t.Fatalf("path count = %v", err)
	}
}

func TestUnderstandingStrictProviderOutputApprovalAndIdempotency(t *testing.T) {
	for _, test := range []struct {
		name     string
		output   string
		approval bool
		wantErr  error
	}{
		{name: "prose", output: "ordinary prose", wantErr: repositorysnapshot.ErrProviderOutput},
		{name: "unknown knowledge state", output: `{"role":"cartographer","findings":[{"id":"f1","statement":"x","knowledge_state":"certain"}]}`, wantErr: repositorysnapshot.ErrProviderOutput},
		{name: "approval", output: `{"role":"cartographer","findings":[]}`, approval: true, wantErr: repositorysnapshot.ErrProviderApproval},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &understandingProvider{output: []byte(test.output), approval: test.approval}
			service := newUnderstandingTestService(t, provider)
			_, err := service.Understand(context.Background(), understandingRequest("key-"+strings.ReplaceAll(test.name, " ", "-"), repositorysnapshot.RoleCartographer))
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if provider.workspace == "" || strings.Contains(provider.workspace, "checkout") {
				t.Fatalf("provider workspace = %q", provider.workspace)
			}
		})
	}

	provider := &understandingProvider{output: []byte(`{"role":"cartographer","findings":[],"capabilities":[],"assumptions":[],"conflicts":[],"unknowns":[]}`)}
	service := newUnderstandingTestService(t, provider)
	request := understandingRequest("idempotent", repositorysnapshot.RoleCartographer)
	first, err := service.Understand(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Understand(context.Background(), request)
	if err != nil || first.ResultDigest != second.ResultDigest || provider.calls != 1 {
		t.Fatalf("replay = %#v/%#v calls=%d err=%v", first, second, provider.calls, err)
	}
	conflict := request
	conflict.Role = repositorysnapshot.RoleQualityOperations
	if _, err := service.Understand(context.Background(), conflict); !errors.Is(err, repositorysnapshot.ErrConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
}

func TestUnderstandingRejectsSecretLikeAndControlProviderOutputBeforePersistence(t *testing.T) {
	outputs := []struct {
		name   string
		output string
	}{
		{name: "json quoted assignment", output: `{"role":"cartographer","findings":[{"id":"f1","statement":"API_TOKEN: \"quoted-value\"","knowledge_state":"observed"}]}`},
		{name: "yaml quoted assignment", output: `{"role":"cartographer","findings":[{"id":"f1","statement":"password: 'quoted-value'","knowledge_state":"observed"}]}`},
		{name: "bearer value", output: `{"role":"cartographer","findings":[{"id":"f1","statement":"Authorization: Bearer short-value","knowledge_state":"observed"}]}`},
		{name: "private key", output: `{"role":"cartographer","findings":[{"id":"f1","statement":"-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----","knowledge_state":"observed"}]}`},
		{name: "escaped control", output: `{"role":"cartographer","findings":[{"id":"f1","statement":"unsafe\u0000value","knowledge_state":"observed"}]}`},
	}
	for _, test := range outputs {
		t.Run(test.name, func(t *testing.T) {
			provider := &understandingProvider{output: []byte(test.output)}
			service, persisted := newUnderstandingTestServiceAndStore(t, provider)
			_, err := service.Understand(context.Background(), understandingRequest("unsafe-"+strings.ReplaceAll(test.name, " ", "-"), repositorysnapshot.RoleCartographer))
			if !errors.Is(err, repositorysnapshot.ErrProviderOutput) {
				t.Fatalf("error = %v, want provider output rejection", err)
			}
			if len(persisted.operations) != 0 {
				t.Fatal("unsafe provider output was persisted")
			}
		})
	}
}

type understandingProvider struct {
	output    []byte
	approval  bool
	workspace string
	calls     int
}

type roleUnderstandingProvider struct{}

func (roleUnderstandingProvider) Analyze(_ context.Context, request repositorysnapshot.ProviderRequest) (repositorysnapshot.ProviderResult, error) {
	output, _ := json.Marshal(repositorysnapshot.StructuredOutput{
		Role: request.Role, Findings: []repositorysnapshot.Finding{}, Capabilities: []string{},
		Assumptions: []string{}, Conflicts: []string{}, Unknowns: []string{},
	})
	return repositorysnapshot.ProviderResult{ProviderID: "fixture", Model: "fixture-model", Output: output}, nil
}

func (p *understandingProvider) Analyze(_ context.Context, request repositorysnapshot.ProviderRequest) (repositorysnapshot.ProviderResult, error) {
	p.calls++
	p.workspace = request.WorkspacePath
	return repositorysnapshot.ProviderResult{ProviderID: "fixture", Model: "fixture-model", Output: p.output, ApprovalRequested: p.approval}, nil
}

type understandingStore struct {
	operations map[string]repositorysnapshot.UnderstandingOperation
}

func (s *understandingStore) LoadRepositoryUnderstanding(_ context.Context, key string) (repositorysnapshot.UnderstandingOperation, error) {
	operation, ok := s.operations[key]
	if !ok {
		return repositorysnapshot.UnderstandingOperation{}, store.ErrNotFound
	}
	return operation, nil
}

func (s *understandingStore) SaveRepositoryUnderstanding(_ context.Context, operation repositorysnapshot.UnderstandingOperation) error {
	if _, exists := s.operations[operation.IdempotencyKey]; exists {
		return store.ErrConflict
	}
	s.operations[operation.IdempotencyKey] = operation
	return nil
}

type understandingCatalog struct{}

func (understandingCatalog) ResolveRepositoryProfile(_ context.Context, id string) (repositorysnapshot.ConfiguredRepository, error) {
	return repositorysnapshot.ConfiguredRepository{ProfileID: id, CheckoutPath: "/disposable-not-live-checkout"}, nil
}

type understandingEvidence struct{}

func (understandingEvidence) RetrieveEvidence(_ context.Context, _ repositorysnapshot.ConfiguredRepository, request repositorysnapshot.EvidenceRequest) (repositorysnapshot.EvidencePacket, error) {
	return repositorysnapshot.EvidencePacket{ContractVersion: repositorysnapshot.EvidenceContractV1, RepositoryProfileID: request.RepositoryProfileID, ExactCommitSHA: request.ExpectedCommitSHA, Files: []repositorysnapshot.EvidenceFile{{Path: "README.md", Content: "evidence", ContentDigest: "sha256:evidence", Size: 8}}}, nil
}

func newUnderstandingTestService(t *testing.T, provider repositorysnapshot.AnalysisProvider) *repositorysnapshot.UnderstandingService {
	service, _ := newUnderstandingTestServiceAndStore(t, provider)
	return service
}

func newUnderstandingTestServiceAndStore(t *testing.T, provider repositorysnapshot.AnalysisProvider) (*repositorysnapshot.UnderstandingService, *understandingStore) {
	t.Helper()
	persisted := &understandingStore{operations: make(map[string]repositorysnapshot.UnderstandingOperation)}
	service, err := repositorysnapshot.NewUnderstandingService(repositorysnapshot.UnderstandingConfig{
		Store:   persisted,
		Catalog: understandingCatalog{}, Evidence: understandingEvidence{}, Providers: map[string]repositorysnapshot.AnalysisProvider{"fixture": provider}, DefaultProvider: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, persisted
}

func understandingRequest(key string, role repositorysnapshot.AnalysisRole) repositorysnapshot.UnderstandingRequest {
	return understandingRequestForProfile("fixture", key, role)
}

func understandingRequestForProfile(profile, key string, role repositorysnapshot.AnalysisRole) repositorysnapshot.UnderstandingRequest {
	return understandingRequestWithCommit(profile, key, role, "0123456789012345678901234567890123456789")
}

func understandingRequestWithCommit(profile, key string, role repositorysnapshot.AnalysisRole, commit string) repositorysnapshot.UnderstandingRequest {
	request := repositorysnapshot.UnderstandingRequest{RepositoryProfileID: profile, ExpectedCommitSHA: commit, Paths: []string{"README.md"}, Role: role, ProviderID: "fixture", IdempotencyKey: key}
	if role == repositorysnapshot.RoleSynthesizer {
		request.Paths = nil
	}
	return request
}

func runEvidenceGit(t *testing.T, git bridgegit.Runner, dir string, args ...string) string {
	t.Helper()
	result, err := git.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(result.Stdout)
}

func writeEvidenceFile(t *testing.T, root, name, content string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
