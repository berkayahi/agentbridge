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
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	bridgegit "github.com/berkayahi/agentbridge/internal/git"
)

const (
	EvidenceContractV1 = "repository-evidence-v1"
	MaxEvidencePaths   = 32
	MaxEvidencePathLen = 4_096
	MaxEvidenceBlob    = 128 << 10
	MaxEvidenceBytes   = 512 << 10
)

var (
	ErrCommitMismatch  = errors.New("repositorysnapshot: exact commit mismatch")
	ErrPathNotAllowed  = errors.New("repositorysnapshot: evidence path is not allowed")
	ErrPathNotFound    = errors.New("repositorysnapshot: evidence path is not committed")
	ErrSecretLikeFile  = errors.New("repositorysnapshot: secret-like file is not readable")
	ErrBinaryEvidence  = errors.New("repositorysnapshot: binary evidence is not readable")
	ErrEvidenceMissing = errors.New("repositorysnapshot: requested evidence is unavailable")
)

type EvidenceRequest struct {
	RepositoryProfileID string   `json:"repository_profile_id"`
	ExpectedCommitSHA   string   `json:"expected_commit_sha"`
	Paths               []string `json:"paths"`
}

type EvidenceFile struct {
	Path          string `json:"path"`
	Size          int    `json:"size"`
	Content       string `json:"content"`
	ContentDigest string `json:"content_digest"`
	Redacted      bool   `json:"redacted"`
}

type EvidencePacket struct {
	ContractVersion     string         `json:"contract_version"`
	RepositoryProfileID string         `json:"repository_profile_id"`
	ExactCommitSHA      string         `json:"exact_commit_sha"`
	Files               []EvidenceFile `json:"files"`
	TotalBytes          int            `json:"total_bytes"`
	ResultDigest        string         `json:"result_digest"`
}

type EvidenceReader interface {
	RetrieveEvidence(context.Context, ConfiguredRepository, EvidenceRequest) (EvidencePacket, error)
}

type GitEvidenceReader struct {
	Git GitRunner
}

var (
	privateKeyBlock     = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	bearerValue         = regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9._~+/=-]+`)
	secretEnvAssignment = regexp.MustCompile("(?im)(^|[[:space:]])(?:export[[:space:]]+)?[A-Za-z_][A-Za-z0-9_]*(?:token|key|secret|password)[A-Za-z0-9_]*[[:space:]]*=[[:space:]]*[^[:space:]]+")
	secretAssignment    = regexp.MustCompile(`(?im)(^|[\{\s,])["]?(?:api[_-]?key|access[_-]?token|auth[_-]?token|authorization|bearer|client[_-]?secret|password|secret|token)["]?\s*[:=]\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^,\r\n\}\s]+)`)
)

func (g GitEvidenceReader) RetrieveEvidence(ctx context.Context, profile ConfiguredRepository, request EvidenceRequest) (EvidencePacket, error) {
	if g.Git == nil || profile.ProfileID == "" || profile.CheckoutPath == "" {
		return EvidencePacket{}, ErrInvalidRequest
	}
	normalized, err := normalizeEvidenceRequest(request)
	if err != nil {
		return EvidencePacket{}, err
	}
	resolved, err := g.run(ctx, profile.CheckoutPath, "rev-parse", "--verify", normalized.ExpectedCommitSHA+"^{commit}")
	if err != nil {
		return EvidencePacket{}, fmt.Errorf("resolve exact evidence commit: %w", ErrCommitMismatch)
	}
	commit := strings.ToLower(strings.TrimSpace(resolved.Stdout))
	if commit != normalized.ExpectedCommitSHA {
		return EvidencePacket{}, ErrCommitMismatch
	}

	packet := EvidencePacket{
		ContractVersion: EvidenceContractV1, RepositoryProfileID: profile.ProfileID,
		ExactCommitSHA: commit, Files: make([]EvidenceFile, 0, len(normalized.Paths)),
	}
	for _, requestedPath := range normalized.Paths {
		if secretLikePath(requestedPath) {
			return EvidencePacket{}, fmt.Errorf("%s: %w", requestedPath, ErrSecretLikeFile)
		}
		entry, err := g.lookupPath(ctx, profile.CheckoutPath, commit, requestedPath)
		if err != nil {
			return EvidencePacket{}, err
		}
		if entry.mode == "120000" || entry.objectType != "blob" {
			return EvidencePacket{}, fmt.Errorf("%s: %w", requestedPath, ErrPathNotFound)
		}
		sizeResult, err := g.run(ctx, profile.CheckoutPath, "cat-file", "-s", entry.objectID)
		if err != nil {
			return EvidencePacket{}, fmt.Errorf("read evidence size: %w", ErrEvidenceMissing)
		}
		size, err := strconv.Atoi(strings.TrimSpace(sizeResult.Stdout))
		if err != nil || size < 0 {
			return EvidencePacket{}, ErrEvidenceMissing
		}
		if size > MaxEvidenceBlob || packet.TotalBytes+size > MaxEvidenceBytes {
			return EvidencePacket{}, ErrBoundsExceeded
		}
		blob, err := g.run(ctx, profile.CheckoutPath, "cat-file", "blob", entry.objectID)
		if err != nil || strings.HasSuffix(blob.Stdout, "…[TRUNCATED]") || len(blob.Stdout) > MaxEvidenceBlob {
			return EvidencePacket{}, ErrBoundsExceeded
		}
		content, redacted, err := redactEvidence(requestedPath, []byte(blob.Stdout))
		if err != nil {
			return EvidencePacket{}, fmt.Errorf("%s: %w", requestedPath, err)
		}
		digest := sha256.Sum256(content)
		packet.Files = append(packet.Files, EvidenceFile{
			Path: requestedPath, Size: len(content), Content: string(content),
			ContentDigest: "sha256:" + hex.EncodeToString(digest[:]), Redacted: redacted,
		})
		packet.TotalBytes += len(content)
		if packet.TotalBytes > MaxEvidenceBytes {
			return EvidencePacket{}, ErrBoundsExceeded
		}
	}
	packet.ResultDigest, err = digestEvidencePacket(packet)
	if err != nil {
		return EvidencePacket{}, err
	}
	return packet, nil
}

func normalizeEvidenceRequest(request EvidenceRequest) (EvidenceRequest, error) {
	request.RepositoryProfileID = strings.TrimSpace(request.RepositoryProfileID)
	request.ExpectedCommitSHA = strings.ToLower(strings.TrimSpace(request.ExpectedCommitSHA))
	if !safeIdentifier.MatchString(request.RepositoryProfileID) || !fullObjectID.MatchString(request.ExpectedCommitSHA) || len(request.Paths) == 0 || len(request.Paths) > MaxEvidencePaths {
		return EvidenceRequest{}, ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(request.Paths))
	for index, value := range request.Paths {
		cleaned, err := normalizeEvidencePath(value)
		if err != nil {
			return EvidenceRequest{}, err
		}
		if _, exists := seen[cleaned]; exists {
			return EvidenceRequest{}, ErrPathNotAllowed
		}
		seen[cleaned] = struct{}{}
		request.Paths[index] = cleaned
	}
	return request, nil
}

func normalizeEvidencePath(value string) (string, error) {
	if value == "" || len(value) > MaxEvidencePathLen || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n\\") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, ":") {
		return "", ErrPathNotAllowed
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", ErrPathNotAllowed
	}
	for _, part := range strings.Split(cleaned, "/") {
		if part == "" || part == "." || part == ".." {
			return "", ErrPathNotAllowed
		}
	}
	return cleaned, nil
}

func (g GitEvidenceReader) lookupPath(ctx context.Context, checkout, commit, requestedPath string) (treeEntry, error) {
	result, err := g.run(ctx, checkout, "ls-tree", "-z", commit, "--", ":(literal)"+requestedPath)
	if err != nil || len(result.Stdout) == 0 || len(result.Stdout) > MaxEvidencePathLen+256 {
		return treeEntry{}, fmt.Errorf("%s: %w", requestedPath, ErrPathNotFound)
	}
	records := strings.Split(result.Stdout, "\x00")
	var found treeEntry
	count := 0
	for _, record := range records {
		if record == "" {
			continue
		}
		match := treeLine.FindStringSubmatch(record)
		if match == nil || match[4] != requestedPath {
			continue
		}
		found = treeEntry{mode: match[1], objectType: match[2], objectID: match[3], path: match[4]}
		count++
	}
	if count != 1 {
		return treeEntry{}, fmt.Errorf("%s: %w", requestedPath, ErrPathNotFound)
	}
	return found, nil
}

func (g GitEvidenceReader) run(ctx context.Context, checkout string, args ...string) (bridgegit.RunResult, error) {
	if raw, ok := g.Git.(interface {
		RunWithEnvironmentUnredacted(context.Context, string, []string, ...string) (bridgegit.RunResult, error)
	}); ok {
		return raw.RunWithEnvironmentUnredacted(ctx, checkout, snapshotGitEnvironment, args...)
	}
	return g.Git.RunWithEnvironment(ctx, checkout, snapshotGitEnvironment, args...)
}

func secretLikePath(value string) bool {
	for _, component := range strings.Split(strings.ToLower(path.Clean(value)), "/") {
		if component == "" || component == "." {
			continue
		}
		if component == ".env" || component == "credentials" || component == "credentials.json" || component == "secrets" || component == "secrets.json" || component == "private" || component == "keys" || component == "id_rsa" || component == "id_ed25519" {
			return true
		}
		if strings.HasSuffix(component, ".pem") || strings.HasSuffix(component, ".key") || strings.HasSuffix(component, ".p12") || strings.HasSuffix(component, ".pfx") {
			return true
		}
		if strings.Contains(component, "secret") && !strings.Contains(component, "example") && !strings.Contains(component, "sample") {
			return true
		}
		if strings.Contains(component, "credential") && !strings.Contains(component, "example") && !strings.Contains(component, "sample") {
			return true
		}
	}
	return false
}

func redactEvidence(pathname string, input []byte) ([]byte, bool, error) {
	if !utf8.Valid(input) {
		return nil, false, ErrBinaryEvidence
	}
	for _, value := range string(input) {
		if unicode.IsControl(value) && value != '\t' && value != '\n' && value != '\r' {
			return nil, false, ErrBinaryEvidence
		}
	}
	value := string(input)
	if secretLikePath(pathname) {
		return nil, false, ErrSecretLikeFile
	}
	if privateKeyBlock.MatchString(value) || bearerValue.MatchString(value) || secretAssignment.MatchString(value) || secretEnvAssignment.MatchString(value) {
		return nil, false, ErrSecretLikeFile
	}
	return input, strings.Contains(value, "[REDACTED:"), nil
}

func digestEvidencePacket(packet EvidencePacket) (string, error) {
	payload := struct {
		ContractVersion     string         `json:"contract_version"`
		RepositoryProfileID string         `json:"repository_profile_id"`
		ExactCommitSHA      string         `json:"exact_commit_sha"`
		Files               []EvidenceFile `json:"files"`
		TotalBytes          int            `json:"total_bytes"`
	}{packet.ContractVersion, packet.RepositoryProfileID, packet.ExactCommitSHA, packet.Files, packet.TotalBytes}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode evidence digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
