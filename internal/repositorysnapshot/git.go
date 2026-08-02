package repositorysnapshot

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	bridgegit "github.com/berkayahi/agentbridge/internal/git"
)

var treeLine = regexp.MustCompile(`^([0-7]{6}) ([a-z]+) ([0-9a-f]{40}|[0-9a-f]{64})\t(.*)$`)
var safeRemoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type GitRunner interface {
	RunWithEnvironment(context.Context, string, []string, ...string) (bridgegit.RunResult, error)
}

type GitInspector struct {
	Git GitRunner
}

var snapshotGitEnvironment = []string{
	"GIT_NO_LAZY_FETCH=1",
	"GIT_NO_REPLACE_OBJECTS=1",
}

type treeEntry struct {
	mode       string
	objectType string
	objectID   string
	path       string
}

func (g GitInspector) Inspect(ctx context.Context, profile ConfiguredRepository, request Request) (Response, error) {
	if g.Git == nil {
		return Response{}, ErrInvalidRequest
	}
	requestedCommitRef := request.RequestedRef
	if request.RequestedRef == profile.AllowedRef && profile.Remote != "" {
		if !safeRemoteName.MatchString(profile.Remote) {
			return Response{}, ErrNotConfigured
		}
		branch := strings.TrimPrefix(profile.AllowedRef, "refs/heads/")
		if branch == profile.AllowedRef || branch == "" {
			return Response{}, ErrNotConfigured
		}
		trackingRef := "refs/remotes/" + profile.Remote + "/" + branch
		refspec := "+" + profile.AllowedRef + ":" + trackingRef
		if _, err := g.run(ctx, profile.CheckoutPath, "fetch", "--no-tags", "--no-write-fetch-head", profile.Remote, refspec); err != nil {
			return Response{}, fmt.Errorf("refresh configured repository ref: %w", err)
		}
		requestedCommitRef = trackingRef
	}
	resolved, err := g.run(ctx, profile.CheckoutPath, "rev-parse", "--verify", requestedCommitRef+"^{commit}")
	if err != nil {
		return Response{}, fmt.Errorf("%w: resolve requested commit", ErrInvalidRequest)
	}
	commit := strings.ToLower(strings.TrimSpace(resolved.Stdout))
	if !fullObjectID.MatchString(commit) {
		return Response{}, errors.New("repositorysnapshot: Git returned an invalid commit identity")
	}
	if err := g.validateScope(ctx, profile.CheckoutPath, commit, request.ScopedRoot); err != nil {
		return Response{}, err
	}
	entries, err := g.listTree(ctx, profile.CheckoutPath, commit, request.ScopedRoot)
	if err != nil {
		return Response{}, err
	}
	response, selected := detectTree(entries)
	response.ExactCommitSHA = commit
	response.Ref = RefMetadata{Requested: request.RequestedRef, Kind: "exact_commit"}
	if request.RequestedRef == profile.AllowedRef {
		response.Ref.Kind = "configured_ref"
		response.Ref.AllowedRef = profile.AllowedRef
	}
	response.Bounds = defaultBounds()
	response.Bounds.TreeEntries = len(entries)
	if err := g.inspectSelectedBlobs(ctx, profile.CheckoutPath, selected, &response); err != nil {
		return Response{}, err
	}
	finalizeResponse(&response)
	return response, nil
}

func (g GitInspector) validateScope(ctx context.Context, checkout, commit, scope string) error {
	if scope == "." {
		result, err := g.run(ctx, checkout, "cat-file", "-t", commit+"^{tree}")
		if err != nil || strings.TrimSpace(result.Stdout) != "tree" {
			return ErrScopeNotFound
		}
		return nil
	}
	result, err := g.run(ctx, checkout, "cat-file", "-t", commit+":"+scope)
	if err != nil || strings.TrimSpace(result.Stdout) != "tree" {
		return ErrScopeNotFound
	}
	return nil
}

func (g GitInspector) listTree(ctx context.Context, checkout, commit, scope string) ([]treeEntry, error) {
	args := []string{"ls-tree", "-r", "-z", "--full-tree", commit}
	if scope != "." {
		args = append(args, "--", ":(literal)"+scope)
	}
	result, err := g.run(ctx, checkout, args...)
	if err != nil {
		return nil, fmt.Errorf("list committed repository tree: %w", err)
	}
	if strings.HasSuffix(result.Stdout, "…[TRUNCATED]") || len(result.Stdout) > MaxTreeOutputBytes {
		return nil, ErrBoundsExceeded
	}
	records := strings.Split(result.Stdout, "\x00")
	entries := make([]treeEntry, 0, min(len(records), MaxTreeEntries))
	for _, record := range records {
		if record == "" {
			continue
		}
		if len(entries) == MaxTreeEntries {
			return nil, ErrBoundsExceeded
		}
		match := treeLine.FindStringSubmatch(record)
		if match == nil || !safeTreePath(match[4]) {
			return nil, errors.New("repositorysnapshot: invalid committed tree entry")
		}
		entries = append(entries, treeEntry{
			mode: match[1], objectType: match[2], objectID: match[3], path: match[4],
		})
	}
	return entries, nil
}

func (g GitInspector) inspectSelectedBlobs(ctx context.Context, checkout string, selected []treeEntry, response *Response) error {
	if len(selected) > MaxSelectedBlobs {
		selected = selected[:MaxSelectedBlobs]
		addLimitation(response, Limitation{Code: "selected_blob_limit"})
	}
	for _, entry := range selected {
		if entry.mode == "120000" {
			addLimitation(response, Limitation{Code: "symlink_not_read", EvidencePath: entry.path})
			continue
		}
		sizeResult, err := g.run(ctx, checkout, "cat-file", "-s", entry.objectID)
		if err != nil {
			return fmt.Errorf("read selected blob size: %w", err)
		}
		size, err := strconv.Atoi(strings.TrimSpace(sizeResult.Stdout))
		if err != nil || size < 0 {
			return errors.New("repositorysnapshot: invalid selected blob size")
		}
		if size > MaxBlobBytes {
			addLimitation(response, Limitation{Code: "blob_too_large", EvidencePath: entry.path})
			continue
		}
		if response.Bounds.SelectedBlobBytes+size > MaxTotalBlobBytes {
			addLimitation(response, Limitation{Code: "total_blob_byte_limit"})
			break
		}
		blob, err := g.run(ctx, checkout, "cat-file", "blob", entry.objectID)
		if err != nil {
			return fmt.Errorf("read selected committed blob: %w", err)
		}
		if strings.HasSuffix(blob.Stdout, "…[TRUNCATED]") || len(blob.Stdout) > MaxBlobBytes {
			return ErrBoundsExceeded
		}
		response.Bounds.SelectedBlobs++
		response.Bounds.SelectedBlobBytes += size
		detectBlob(entry.path, []byte(blob.Stdout), response)
	}
	return nil
}

func (g GitInspector) run(ctx context.Context, checkout string, args ...string) (bridgegit.RunResult, error) {
	return g.Git.RunWithEnvironment(ctx, checkout, snapshotGitEnvironment, args...)
}

func normalizeScope(value string) (string, error) {
	if value == "" || value == "." {
		if value == "." {
			return ".", nil
		}
		return "", ErrInvalidRequest
	}
	if len(value) > 512 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n\\") ||
		strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return "", ErrInvalidRequest
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", ErrInvalidRequest
	}
	for _, part := range strings.Split(cleaned, "/") {
		if part == "" || part == "." || part == ".." {
			return "", ErrInvalidRequest
		}
	}
	return cleaned, nil
}

func safeTreePath(value string) bool {
	if value == "" || len(value) > 4_096 || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\x00\r\n\\") || strings.HasPrefix(value, "/") {
		return false
	}
	cleaned := path.Clean(value)
	return cleaned == value && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func defaultBounds() Bounds {
	return Bounds{
		MaxTreeEntries: MaxTreeEntries, MaxSelectedBlobs: MaxSelectedBlobs,
		MaxBlobBytes: MaxBlobBytes, MaxTotalBlobBytes: MaxTotalBlobBytes,
		MaxObservations: MaxObservations, MaxLimitations: MaxLimitations,
		MaxTreeOutputBytes: MaxTreeOutputBytes,
	}
}
