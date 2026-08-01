package repositorysnapshot_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bridgegit "github.com/berkayahi/agentbridge/internal/git"
	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	"github.com/berkayahi/agentbridge/internal/store"
)

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

	packet, err := (repositorysnapshot.GitEvidenceReader{Git: git}).RetrieveEvidence(context.Background(), repositorysnapshot.ConfiguredRepository{
		ProfileID: "fixture", CheckoutPath: root,
	}, repositorysnapshot.EvidenceRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: commit, Paths: []string{"README.md", ".env.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if packet.ExactCommitSHA != commit || packet.ResultDigest == "" || packet.Files[0].Content != "committed-value\n" {
		t.Fatalf("packet = %#v", packet)
	}
	if strings.Contains(packet.Files[1].Content, "do-not-leak") || strings.Contains(packet.Files[1].Content, "password") || !packet.Files[1].Redacted {
		t.Fatalf("dotenv was not redacted: %#v", packet.Files[1])
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

type understandingProvider struct {
	output    []byte
	approval  bool
	workspace string
	calls     int
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

func (understandingCatalog) ResolveRepositoryProfile(context.Context, string) (repositorysnapshot.ConfiguredRepository, error) {
	return repositorysnapshot.ConfiguredRepository{ProfileID: "fixture", CheckoutPath: "/disposable-not-live-checkout"}, nil
}

type understandingEvidence struct{}

func (understandingEvidence) RetrieveEvidence(context.Context, repositorysnapshot.ConfiguredRepository, repositorysnapshot.EvidenceRequest) (repositorysnapshot.EvidencePacket, error) {
	return repositorysnapshot.EvidencePacket{ContractVersion: repositorysnapshot.EvidenceContractV1, RepositoryProfileID: "fixture", ExactCommitSHA: "0123456789012345678901234567890123456789", Files: []repositorysnapshot.EvidenceFile{{Path: "README.md", Content: "evidence", ContentDigest: "sha256:evidence", Size: 8}}}, nil
}

func newUnderstandingTestService(t *testing.T, provider repositorysnapshot.AnalysisProvider) *repositorysnapshot.UnderstandingService {
	t.Helper()
	service, err := repositorysnapshot.NewUnderstandingService(repositorysnapshot.UnderstandingConfig{
		Store:   &understandingStore{operations: make(map[string]repositorysnapshot.UnderstandingOperation)},
		Catalog: understandingCatalog{}, Evidence: understandingEvidence{}, Providers: map[string]repositorysnapshot.AnalysisProvider{"fixture": provider}, DefaultProvider: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func understandingRequest(key string, role repositorysnapshot.AnalysisRole) repositorysnapshot.UnderstandingRequest {
	return repositorysnapshot.UnderstandingRequest{RepositoryProfileID: "fixture", ExpectedCommitSHA: "0123456789012345678901234567890123456789", Paths: []string{"README.md"}, Role: role, ProviderID: "fixture", IdempotencyKey: key}
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
