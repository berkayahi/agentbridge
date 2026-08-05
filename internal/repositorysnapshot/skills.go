package repositorysnapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	bridgegit "github.com/berkayahi/agentbridge/internal/git"
)

const (
	SkillsContractV1 = "repository-skills-v1"
	MaxSkillFiles    = 256
	MaxSkillBlob     = 128 << 10
	MaxSkillBytes    = 2 << 20
)

type SkillRequest struct {
	RepositoryProfileID string `json:"repository_profile_id"`
	ExpectedCommitSHA   string `json:"expected_commit_sha"`
	ScopedRoot          string `json:"scoped_root"`
}

type SkillFile struct {
	Path          string `json:"path"`
	Size          int    `json:"size"`
	Content       string `json:"content"`
	ContentDigest string `json:"content_digest"`
	Redacted      bool   `json:"redacted"`
}

type SkillBounds struct {
	Files         int `json:"files"`
	TotalBytes    int `json:"total_bytes"`
	MaxFiles      int `json:"max_files"`
	MaxBlobBytes  int `json:"max_blob_bytes"`
	MaxTotalBytes int `json:"max_total_bytes"`
}

type SkillPacket struct {
	ContractVersion     string      `json:"contract_version"`
	RepositoryProfileID string      `json:"repository_profile_id"`
	ExactCommitSHA      string      `json:"exact_commit_sha"`
	ScopedRoot          string      `json:"scoped_root"`
	Files               []SkillFile `json:"files"`
	Bounds              SkillBounds `json:"bounds"`
	ResultDigest        string      `json:"result_digest"`
}

// SkillReader is intentionally limited to committed files named SKILL.md.
// Repository content is never checked out, executed, or allowed to select an
// arbitrary path through this contract.
type SkillReader interface {
	ReadSkills(context.Context, ConfiguredRepository, SkillRequest) (SkillPacket, error)
}

type GitSkillReader struct{ Git GitRunner }

func (g GitSkillReader) ReadSkills(ctx context.Context, profile ConfiguredRepository, request SkillRequest) (SkillPacket, error) {
	if g.Git == nil || profile.ProfileID == "" || profile.CheckoutPath == "" {
		return SkillPacket{}, ErrInvalidRequest
	}
	normalized, err := normalizeSkillRequest(request)
	if err != nil {
		return SkillPacket{}, err
	}
	resolved, err := g.run(ctx, profile.CheckoutPath, "rev-parse", "--verify", normalized.ExpectedCommitSHA+"^{commit}")
	if err != nil {
		return SkillPacket{}, fmt.Errorf("resolve exact skill commit: %w", ErrCommitMismatch)
	}
	commit := strings.ToLower(strings.TrimSpace(resolved.Stdout))
	if commit != normalized.ExpectedCommitSHA {
		return SkillPacket{}, ErrCommitMismatch
	}

	// ls-tree does not implement Git's glob pathspec magic. Read the bounded
	// committed tree (or one literal subtree) and enforce the exact basename in
	// process; the runner and MaxTreeOutputBytes fail closed for oversized trees.
	args := []string{"ls-tree", "-r", "-z", "--full-tree", commit}
	if normalized.ScopedRoot != "." {
		args = append(args, "--", ":(literal)"+normalized.ScopedRoot)
	}
	result, err := g.run(ctx, profile.CheckoutPath, args...)
	if err != nil {
		return SkillPacket{}, fmt.Errorf("list committed skill tree: %w", ErrEvidenceMissing)
	}
	if strings.HasSuffix(result.Stdout, "…[TRUNCATED]") || len(result.Stdout) > MaxTreeOutputBytes {
		return SkillPacket{}, ErrBoundsExceeded
	}

	entries := make([]treeEntry, 0)
	for _, record := range strings.Split(result.Stdout, "\x00") {
		if record == "" {
			continue
		}
		match := treeLine.FindStringSubmatch(record)
		if match == nil || !safeTreePath(match[4]) {
			return SkillPacket{}, errors.New("repositorysnapshot: invalid committed skill entry")
		}
		entry := treeEntry{mode: match[1], objectType: match[2], objectID: match[3], path: match[4]}
		if !validSkillPath(normalized.ScopedRoot, entry.path) {
			continue
		}
		if entry.mode == "120000" || entry.objectType != "blob" {
			return SkillPacket{}, fmt.Errorf("%s: %w", entry.path, ErrPathNotAllowed)
		}
		entries = append(entries, entry)
	}
	if len(entries) > MaxSkillFiles {
		return SkillPacket{}, ErrBoundsExceeded
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })

	packet := SkillPacket{
		ContractVersion: SkillsContractV1, RepositoryProfileID: profile.ProfileID,
		ExactCommitSHA: commit, ScopedRoot: normalized.ScopedRoot,
		Files:  make([]SkillFile, 0, len(entries)),
		Bounds: SkillBounds{MaxFiles: MaxSkillFiles, MaxBlobBytes: MaxSkillBlob, MaxTotalBytes: MaxSkillBytes},
	}
	for _, entry := range entries {
		sizeResult, err := g.run(ctx, profile.CheckoutPath, "cat-file", "-s", entry.objectID)
		if err != nil {
			return SkillPacket{}, fmt.Errorf("read skill size: %w", ErrEvidenceMissing)
		}
		size, err := strconv.Atoi(strings.TrimSpace(sizeResult.Stdout))
		if err != nil || size < 0 {
			return SkillPacket{}, ErrEvidenceMissing
		}
		if size > MaxSkillBlob || packet.Bounds.TotalBytes+size > MaxSkillBytes {
			return SkillPacket{}, ErrBoundsExceeded
		}
		blob, err := g.run(ctx, profile.CheckoutPath, "cat-file", "blob", entry.objectID)
		if err != nil || strings.HasSuffix(blob.Stdout, "…[TRUNCATED]") || len(blob.Stdout) > MaxSkillBlob {
			return SkillPacket{}, ErrBoundsExceeded
		}
		content, redacted, err := redactEvidence(entry.path, []byte(blob.Stdout))
		if err != nil {
			return SkillPacket{}, fmt.Errorf("%s: %w", entry.path, err)
		}
		sum := sha256.Sum256(content)
		packet.Files = append(packet.Files, SkillFile{
			Path: entry.path, Size: len(content), Content: string(content),
			ContentDigest: "sha256:" + hex.EncodeToString(sum[:]), Redacted: redacted,
		})
		packet.Bounds.Files++
		packet.Bounds.TotalBytes += len(content)
	}
	packet.ResultDigest, err = digestSkillPacket(packet)
	if err != nil {
		return SkillPacket{}, err
	}
	return packet, nil
}

func normalizeSkillRequest(request SkillRequest) (SkillRequest, error) {
	request.RepositoryProfileID = strings.TrimSpace(request.RepositoryProfileID)
	request.ExpectedCommitSHA = strings.ToLower(strings.TrimSpace(request.ExpectedCommitSHA))
	if !safeIdentifier.MatchString(request.RepositoryProfileID) || !fullObjectID.MatchString(request.ExpectedCommitSHA) {
		return SkillRequest{}, ErrInvalidRequest
	}
	scope, err := normalizeScope(request.ScopedRoot)
	if err != nil {
		return SkillRequest{}, err
	}
	request.ScopedRoot = scope
	return request, nil
}

func validSkillPath(root, value string) bool {
	if !utf8.ValidString(value) || path.Base(value) != "SKILL.md" {
		return false
	}
	if root == "." {
		return value == "SKILL.md" || strings.Contains(value, "/")
	}
	return value == path.Join(root, "SKILL.md") || strings.HasPrefix(value, root+"/")
}

func (g GitSkillReader) run(ctx context.Context, checkout string, args ...string) (bridgegit.RunResult, error) {
	if raw, ok := g.Git.(interface {
		RunWithEnvironmentUnredacted(context.Context, string, []string, ...string) (bridgegit.RunResult, error)
	}); ok {
		return raw.RunWithEnvironmentUnredacted(ctx, checkout, snapshotGitEnvironment, args...)
	}
	return g.Git.RunWithEnvironment(ctx, checkout, snapshotGitEnvironment, args...)
}

func digestSkillPacket(packet SkillPacket) (string, error) {
	payload := struct {
		ContractVersion     string      `json:"contract_version"`
		RepositoryProfileID string      `json:"repository_profile_id"`
		ExactCommitSHA      string      `json:"exact_commit_sha"`
		ScopedRoot          string      `json:"scoped_root"`
		Files               []SkillFile `json:"files"`
		Bounds              SkillBounds `json:"bounds"`
	}{packet.ContractVersion, packet.RepositoryProfileID, packet.ExactCommitSHA, packet.ScopedRoot, packet.Files, packet.Bounds}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode skill packet digest: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
