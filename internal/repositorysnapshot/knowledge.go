package repositorysnapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	bridgegit "github.com/berkayahi/agentbridge/internal/git"
)

const (
	KnowledgeContractV1 = "repository-knowledge-v1"
	MaxKnowledgeFiles   = 256
	MaxKnowledgeBlob    = 128 << 10
	MaxKnowledgeBytes   = 2 << 20
)

var knowledgeFilename = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,126}\.md$`)

var knowledgeCategories = map[string]struct{}{
	"product": {}, "decisions": {}, "research": {}, "domain": {},
	"specifications": {}, "milestones": {}, "reviews": {}, "lessons": {},
}

// KnowledgeRequest names only an exact commit. Callers resolve mutable refs
// through the snapshot contract first, so a returned note can never be
// mistaken for content from whichever checkout happened to be live.
type KnowledgeRequest struct {
	RepositoryProfileID string `json:"repository_profile_id"`
	ExpectedCommitSHA   string `json:"expected_commit_sha"`
	ScopedRoot          string `json:"scoped_root"`
}

type KnowledgeFile struct {
	Path          string `json:"path"`
	Size          int    `json:"size"`
	Content       string `json:"content"`
	ContentDigest string `json:"content_digest"`
	Redacted      bool   `json:"redacted"`
}

type KnowledgeBounds struct {
	Files         int `json:"files"`
	TotalBytes    int `json:"total_bytes"`
	MaxFiles      int `json:"max_files"`
	MaxBlobBytes  int `json:"max_blob_bytes"`
	MaxTotalBytes int `json:"max_total_bytes"`
}

type KnowledgePacket struct {
	ContractVersion     string          `json:"contract_version"`
	RepositoryProfileID string          `json:"repository_profile_id"`
	ExactCommitSHA      string          `json:"exact_commit_sha"`
	ScopedRoot          string          `json:"scoped_root"`
	Files               []KnowledgeFile `json:"files"`
	Bounds              KnowledgeBounds `json:"bounds"`
	ResultDigest        string          `json:"result_digest"`
}

// KnowledgeReader reads only the repository's typed Kovan knowledge subtree.
// It is deliberately narrower than EvidenceReader: callers cannot select an
// arbitrary prefix or smuggle a checkout path into the request.
type KnowledgeReader interface {
	ReadKnowledge(context.Context, ConfiguredRepository, KnowledgeRequest) (KnowledgePacket, error)
}

type GitKnowledgeReader struct {
	Git GitRunner
}

func (g GitKnowledgeReader) ReadKnowledge(ctx context.Context, profile ConfiguredRepository, request KnowledgeRequest) (KnowledgePacket, error) {
	if g.Git == nil || profile.ProfileID == "" || profile.CheckoutPath == "" {
		return KnowledgePacket{}, ErrInvalidRequest
	}
	normalized, err := normalizeKnowledgeRequest(request)
	if err != nil {
		return KnowledgePacket{}, err
	}
	resolved, err := g.run(ctx, profile.CheckoutPath, "rev-parse", "--verify", normalized.ExpectedCommitSHA+"^{commit}")
	if err != nil {
		return KnowledgePacket{}, fmt.Errorf("resolve exact knowledge commit: %w", ErrCommitMismatch)
	}
	commit := strings.ToLower(strings.TrimSpace(resolved.Stdout))
	if commit != normalized.ExpectedCommitSHA {
		return KnowledgePacket{}, ErrCommitMismatch
	}

	root := ".kovan/knowledge"
	if normalized.ScopedRoot != "." {
		root = path.Join(normalized.ScopedRoot, root)
	}
	result, err := g.run(ctx, profile.CheckoutPath, "ls-tree", "-r", "-z", "--full-tree", commit, "--", ":(literal)"+root)
	if err != nil {
		return KnowledgePacket{}, fmt.Errorf("list committed knowledge tree: %w", ErrEvidenceMissing)
	}
	if strings.HasSuffix(result.Stdout, "…[TRUNCATED]") || len(result.Stdout) > MaxTreeOutputBytes {
		return KnowledgePacket{}, ErrBoundsExceeded
	}

	entries := make([]treeEntry, 0)
	for _, record := range strings.Split(result.Stdout, "\x00") {
		if record == "" {
			continue
		}
		match := treeLine.FindStringSubmatch(record)
		if match == nil || !safeTreePath(match[4]) {
			return KnowledgePacket{}, errors.New("repositorysnapshot: invalid committed knowledge entry")
		}
		entry := treeEntry{mode: match[1], objectType: match[2], objectID: match[3], path: match[4]}
		if !validKnowledgePath(root, entry.path) {
			continue
		}
		if entry.mode == "120000" || entry.objectType != "blob" {
			return KnowledgePacket{}, fmt.Errorf("%s: %w", entry.path, ErrPathNotAllowed)
		}
		entries = append(entries, entry)
	}
	if len(entries) > MaxKnowledgeFiles {
		return KnowledgePacket{}, ErrBoundsExceeded
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })

	packet := KnowledgePacket{
		ContractVersion: KnowledgeContractV1, RepositoryProfileID: profile.ProfileID,
		ExactCommitSHA: commit, ScopedRoot: normalized.ScopedRoot,
		Files:  make([]KnowledgeFile, 0, len(entries)),
		Bounds: KnowledgeBounds{MaxFiles: MaxKnowledgeFiles, MaxBlobBytes: MaxKnowledgeBlob, MaxTotalBytes: MaxKnowledgeBytes},
	}
	for _, entry := range entries {
		sizeResult, err := g.run(ctx, profile.CheckoutPath, "cat-file", "-s", entry.objectID)
		if err != nil {
			return KnowledgePacket{}, fmt.Errorf("read knowledge size: %w", ErrEvidenceMissing)
		}
		size, err := strconv.Atoi(strings.TrimSpace(sizeResult.Stdout))
		if err != nil || size < 0 {
			return KnowledgePacket{}, ErrEvidenceMissing
		}
		if size > MaxKnowledgeBlob || packet.Bounds.TotalBytes+size > MaxKnowledgeBytes {
			return KnowledgePacket{}, ErrBoundsExceeded
		}
		blob, err := g.run(ctx, profile.CheckoutPath, "cat-file", "blob", entry.objectID)
		if err != nil || strings.HasSuffix(blob.Stdout, "…[TRUNCATED]") || len(blob.Stdout) > MaxKnowledgeBlob {
			return KnowledgePacket{}, ErrBoundsExceeded
		}
		content, redacted, err := redactEvidence(entry.path, []byte(blob.Stdout))
		if err != nil {
			return KnowledgePacket{}, fmt.Errorf("%s: %w", entry.path, err)
		}
		digest := sha256.Sum256(content)
		packet.Files = append(packet.Files, KnowledgeFile{
			Path: entry.path, Size: len(content), Content: string(content),
			ContentDigest: "sha256:" + hex.EncodeToString(digest[:]), Redacted: redacted,
		})
		packet.Bounds.Files++
		packet.Bounds.TotalBytes += len(content)
	}
	packet.ResultDigest, err = digestKnowledgePacket(packet)
	if err != nil {
		return KnowledgePacket{}, err
	}
	return packet, nil
}

func normalizeKnowledgeRequest(request KnowledgeRequest) (KnowledgeRequest, error) {
	request.RepositoryProfileID = strings.TrimSpace(request.RepositoryProfileID)
	request.ExpectedCommitSHA = strings.ToLower(strings.TrimSpace(request.ExpectedCommitSHA))
	if !safeIdentifier.MatchString(request.RepositoryProfileID) || !fullObjectID.MatchString(request.ExpectedCommitSHA) {
		return KnowledgeRequest{}, ErrInvalidRequest
	}
	scope, err := normalizeScope(request.ScopedRoot)
	if err != nil {
		return KnowledgeRequest{}, err
	}
	request.ScopedRoot = scope
	return request, nil
}

func validKnowledgePath(root, value string) bool {
	if !strings.HasPrefix(value, root+"/") {
		return false
	}
	relative := strings.TrimPrefix(value, root+"/")
	parts := strings.Split(relative, "/")
	if len(parts) != 2 {
		return false
	}
	_, category := knowledgeCategories[parts[0]]
	return category && knowledgeFilename.MatchString(parts[1]) && utf8.ValidString(value)
}

func (g GitKnowledgeReader) run(ctx context.Context, checkout string, args ...string) (bridgegit.RunResult, error) {
	if raw, ok := g.Git.(interface {
		RunWithEnvironmentUnredacted(context.Context, string, []string, ...string) (bridgegit.RunResult, error)
	}); ok {
		return raw.RunWithEnvironmentUnredacted(ctx, checkout, snapshotGitEnvironment, args...)
	}
	return g.Git.RunWithEnvironment(ctx, checkout, snapshotGitEnvironment, args...)
}

func digestKnowledgePacket(packet KnowledgePacket) (string, error) {
	payload := struct {
		ContractVersion     string          `json:"contract_version"`
		RepositoryProfileID string          `json:"repository_profile_id"`
		ExactCommitSHA      string          `json:"exact_commit_sha"`
		ScopedRoot          string          `json:"scoped_root"`
		Files               []KnowledgeFile `json:"files"`
		Bounds              KnowledgeBounds `json:"bounds"`
	}{packet.ContractVersion, packet.RepositoryProfileID, packet.ExactCommitSHA, packet.ScopedRoot, packet.Files, packet.Bounds}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode knowledge digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
