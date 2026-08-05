package repositorysnapshot_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	bridgegit "github.com/berkayahi/agentbridge/internal/git"
	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	"github.com/berkayahi/agentbridge/internal/security"
)

func TestSkillReaderReturnsOnlyExactCommittedPackagesWithoutChangingCheckout(t *testing.T) {
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
	runGit(t, git, checkout, "config", "user.name", "Skill Test")
	runGit(t, git, checkout, "config", "user.email", "skill@example.invalid")
	rootSkill := "---\nname: Root Cartographer\ndescription: Map the repository.\n---\n# Root Cartographer\n"
	nestedSkill := "---\nname: Go Reviewer\ndescription: Review Go changes.\n---\n# Go Reviewer\n"
	writeFile(t, checkout, "SKILL.md", rootSkill)
	writeFile(t, checkout, ".agents/skills/go-reviewer/SKILL.md", nestedSkill)
	writeFile(t, checkout, "docs/SKILL.md.bak", "not a package\n")
	writeFile(t, checkout, "README.md", "not a package\n")
	runGit(t, git, checkout, "add", ".")
	runGit(t, git, checkout, "commit", "-m", "test: committed skill packages")
	exactCommit := runGit(t, git, checkout, "rev-parse", "HEAD")
	writeFile(t, checkout, ".agents/skills/go-reviewer/SKILL.md", "dirty content must not appear\n")
	statusBefore := runGit(t, git, checkout, "status", "--porcelain=v1", "-z")
	git.reset()

	reader := repositorysnapshot.GitSkillReader{Git: git}
	packet, err := reader.ReadSkills(ctx, repositorysnapshot.ConfiguredRepository{
		ProfileID: "fixture", CheckoutPath: checkout, AllowedRef: "refs/heads/main",
	}, repositorysnapshot.SkillRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: exactCommit, ScopedRoot: ".",
	})
	if err != nil {
		t.Fatal(err)
	}
	if packet.ContractVersion != repositorysnapshot.SkillsContractV1 || packet.ExactCommitSHA != exactCommit ||
		packet.Bounds.Files != 2 || packet.Bounds.TotalBytes != len(rootSkill)+len(nestedSkill) ||
		!strings.HasPrefix(packet.ResultDigest, "sha256:") {
		t.Fatalf("unexpected skill packet: %#v", packet)
	}
	if got := []string{packet.Files[0].Path, packet.Files[1].Path}; !slices.Equal(got, []string{".agents/skills/go-reviewer/SKILL.md", "SKILL.md"}) {
		t.Fatalf("skill paths = %#v", got)
	}
	for _, file := range packet.Files {
		if strings.Contains(file.Content, "dirty content") || strings.Contains(file.Path, "README") || strings.Contains(file.Path, ".bak") {
			t.Fatalf("reader returned non-commit or non-package content: %#v", file)
		}
		digest := sha256.Sum256([]byte(file.Content))
		if file.ContentDigest != "sha256:"+hex.EncodeToString(digest[:]) || file.Size != len(file.Content) {
			t.Fatalf("invalid file identity: %#v", file)
		}
	}
	encoded, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), checkout) {
		t.Fatalf("packet leaked checkout path: %s", encoded)
	}
	statusAfter := runGit(t, git, checkout, "status", "--porcelain=v1", "-z")
	if statusAfter != statusBefore {
		t.Fatalf("checkout changed: before=%q after=%q", statusBefore, statusAfter)
	}
	for _, command := range git.commandsSinceReset() {
		if !slices.Contains([]string{"rev-parse", "ls-tree", "cat-file", "status"}, command) {
			t.Fatalf("unexpected command %q", command)
		}
	}

	if err := os.Symlink("../SKILL.md", filepath.Join(checkout, ".agents", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, checkout, "add", ".agents/SKILL.md")
	runGit(t, git, checkout, "commit", "-m", "test: committed skill symlink")
	symlinkCommit := runGit(t, git, checkout, "rev-parse", "HEAD")
	_, err = reader.ReadSkills(ctx, repositorysnapshot.ConfiguredRepository{
		ProfileID: "fixture", CheckoutPath: checkout, AllowedRef: "refs/heads/main",
	}, repositorysnapshot.SkillRequest{
		RepositoryProfileID: "fixture", ExpectedCommitSHA: symlinkCommit, ScopedRoot: ".",
	})
	if !errors.Is(err, repositorysnapshot.ErrPathNotAllowed) {
		t.Fatalf("symlink error = %v, want ErrPathNotAllowed", err)
	}
}
