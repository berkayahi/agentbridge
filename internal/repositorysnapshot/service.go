package repositorysnapshot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/store"
)

var (
	safeIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	fullObjectID   = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
)

type Inspector interface {
	Inspect(context.Context, ConfiguredRepository, Request) (Response, error)
}

type Config struct {
	Store     Store
	Catalog   Catalog
	Inspector Inspector
	Knowledge KnowledgeReader
	Skills    SkillReader
	Clock     func() time.Time
	NewID     func() string
}

type Service struct {
	mu        sync.Mutex
	store     Store
	catalog   Catalog
	inspector Inspector
	knowledge KnowledgeReader
	skills    SkillReader
	clock     func() time.Time
	newID     func() string
}

func New(config Config) (*Service, error) {
	if config.Store == nil || config.Catalog == nil || config.Inspector == nil {
		return nil, ErrInvalidRequest
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.NewID == nil {
		config.NewID = newOperationID
	}
	return &Service{
		store: config.Store, catalog: config.Catalog, inspector: config.Inspector, knowledge: config.Knowledge, skills: config.Skills,
		clock: config.Clock, newID: config.NewID,
	}, nil
}

// ReadSkills returns only committed SKILL.md packages at one exact Git
// identity. The fixed filename contract prevents this from becoming an
// arbitrary repository file reader.
func (s *Service) ReadSkills(ctx context.Context, request SkillRequest) (SkillPacket, error) {
	if s == nil || s.catalog == nil || s.skills == nil {
		return SkillPacket{}, ErrNotConfigured
	}
	normalized, err := normalizeSkillRequest(request)
	if err != nil {
		return SkillPacket{}, err
	}
	profile, err := s.catalog.ResolveRepositoryProfile(ctx, normalized.RepositoryProfileID)
	if err != nil {
		return SkillPacket{}, err
	}
	if profile.ProfileID != normalized.RepositoryProfileID || !filepath.IsAbs(profile.CheckoutPath) {
		return SkillPacket{}, ErrNotConfigured
	}
	return s.skills.ReadSkills(ctx, profile, normalized)
}

// ReadKnowledge returns the typed Kovan knowledge notes committed at one exact
// Git identity. Repository lookup remains private to AgentBridge and the
// caller cannot widen the fixed .kovan/knowledge subtree.
func (s *Service) ReadKnowledge(ctx context.Context, request KnowledgeRequest) (KnowledgePacket, error) {
	if s == nil || s.catalog == nil || s.knowledge == nil {
		return KnowledgePacket{}, ErrNotConfigured
	}
	normalized, err := normalizeKnowledgeRequest(request)
	if err != nil {
		return KnowledgePacket{}, err
	}
	profile, err := s.catalog.ResolveRepositoryProfile(ctx, normalized.RepositoryProfileID)
	if err != nil {
		return KnowledgePacket{}, err
	}
	if profile.ProfileID != normalized.RepositoryProfileID || !filepath.IsAbs(profile.CheckoutPath) {
		return KnowledgePacket{}, ErrNotConfigured
	}
	return s.knowledge.ReadKnowledge(ctx, profile, normalized)
}

func (s *Service) Snapshot(ctx context.Context, request Request) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalized, err := normalizeRequest(request)
	if err != nil {
		return Response{}, err
	}
	requestDigest := digestRequest(normalized)
	if existing, err := s.store.LoadRepositorySnapshot(ctx, normalized.IdempotencyKey); err == nil {
		if existing.RequestDigest != requestDigest {
			return Response{}, ErrConflict
		}
		if err := validatePersistedOperation(existing); err != nil {
			return Response{}, err
		}
		return existing.Response, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return Response{}, fmt.Errorf("load repository snapshot operation: %w", err)
	}

	profile, err := s.catalog.ResolveRepositoryProfile(ctx, normalized.RepositoryProfileID)
	if err != nil {
		return Response{}, err
	}
	if profile.ProfileID != normalized.RepositoryProfileID || !filepath.IsAbs(profile.CheckoutPath) || profile.AllowedRef == "" {
		return Response{}, ErrNotConfigured
	}
	if !fullObjectID.MatchString(normalized.RequestedRef) && normalized.RequestedRef != profile.AllowedRef {
		return Response{}, ErrRefNotAllowed
	}

	response, err := s.inspector.Inspect(ctx, profile, normalized)
	if err != nil {
		return Response{}, err
	}
	now := s.clock().UTC()
	response.ContractVersion = RepositorySnapshotV1
	response.OperationID = s.newID()
	response.Repository = RepositoryIdentity{ProfileID: profile.ProfileID}
	response.ScopedRoot = normalized.ScopedRoot
	response.AnalyzerVersion = normalized.AnalyzerVersion
	response.ResultDigest, err = digestResponse(response)
	if err != nil {
		return Response{}, err
	}
	operation := Operation{
		ID: response.OperationID, IdempotencyKey: normalized.IdempotencyKey,
		RepositoryProfileID: normalized.RepositoryProfileID, RequestedRef: normalized.RequestedRef,
		ScopedRoot: normalized.ScopedRoot, AnalyzerVersion: normalized.AnalyzerVersion,
		RequestDigest: requestDigest, ExactCommitSHA: response.ExactCommitSHA,
		ResultDigest: response.ResultDigest, Status: "completed", Response: response,
		RequestedAt: now, CompletedAt: now,
	}
	if err := s.store.SaveRepositorySnapshot(ctx, operation); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return Response{}, fmt.Errorf("save repository snapshot operation: %w", err)
		}
		existing, loadErr := s.store.LoadRepositorySnapshot(ctx, normalized.IdempotencyKey)
		if loadErr != nil || existing.RequestDigest != requestDigest {
			return Response{}, ErrConflict
		}
		if err := validatePersistedOperation(existing); err != nil {
			return Response{}, err
		}
		return existing.Response, nil
	}
	return response, nil
}

// LoadOperation returns the exact persisted snapshot identified by its
// operation ID. Higher-level adapters use this narrow lookup to bind
// caller-provided snapshot metadata to the snapshot service's durable result.
func (s *Service) LoadOperation(ctx context.Context, operationID string) (Operation, error) {
	if s == nil || strings.TrimSpace(operationID) == "" {
		return Operation{}, ErrInvalidRequest
	}
	operation, err := s.store.LoadRepositorySnapshotByID(ctx, operationID)
	if err != nil {
		return Operation{}, err
	}
	if err := validatePersistedOperation(operation); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

func normalizeRequest(request Request) (Request, error) {
	if !safeIdentifier.MatchString(request.RepositoryProfileID) ||
		!safeIdentifier.MatchString(request.AnalyzerVersion) ||
		!safeIdentifier.MatchString(request.IdempotencyKey) {
		return Request{}, ErrInvalidRequest
	}
	if request.RepositoryProfileID != strings.TrimSpace(request.RepositoryProfileID) ||
		request.AnalyzerVersion != strings.TrimSpace(request.AnalyzerVersion) ||
		request.IdempotencyKey != strings.TrimSpace(request.IdempotencyKey) ||
		request.RequestedRef != strings.TrimSpace(request.RequestedRef) {
		return Request{}, ErrInvalidRequest
	}
	scope, err := normalizeScope(request.ScopedRoot)
	if err != nil {
		return Request{}, err
	}
	request.ScopedRoot = scope
	if request.RequestedRef == "" || len(request.RequestedRef) > 256 ||
		strings.ContainsAny(request.RequestedRef, "\x00\r\n") ||
		strings.HasPrefix(request.RequestedRef, "-") {
		return Request{}, ErrInvalidRequest
	}
	return request, nil
}

func digestRequest(request Request) string {
	payload := struct {
		RepositoryProfileID string `json:"repository_profile_id"`
		RequestedRef        string `json:"requested_ref"`
		ScopedRoot          string `json:"scoped_root"`
		AnalyzerVersion     string `json:"analyzer_version"`
	}{
		RepositoryProfileID: request.RepositoryProfileID,
		RequestedRef:        request.RequestedRef,
		ScopedRoot:          request.ScopedRoot,
		AnalyzerVersion:     request.AnalyzerVersion,
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func digestResponse(response Response) (string, error) {
	payload := struct {
		ContractVersion string             `json:"contract_version"`
		Repository      RepositoryIdentity `json:"repository"`
		ExactCommitSHA  string             `json:"exact_commit_sha"`
		Ref             RefMetadata        `json:"ref"`
		ScopedRoot      string             `json:"scoped_root"`
		AnalyzerVersion string             `json:"analyzer_version"`
		Detectors       []Detector         `json:"detectors"`
		Observations    []Observation      `json:"observations"`
		Limitations     []Limitation       `json:"limitations"`
		Bounds          Bounds             `json:"bounds"`
	}{
		ContractVersion: response.ContractVersion, Repository: response.Repository,
		ExactCommitSHA: response.ExactCommitSHA, Ref: response.Ref,
		ScopedRoot: response.ScopedRoot, AnalyzerVersion: response.AnalyzerVersion,
		Detectors: response.Detectors, Observations: response.Observations,
		Limitations: response.Limitations, Bounds: response.Bounds,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode repository snapshot digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// VerifyResponseDigest checks that a snapshot is the exact persisted result,
// rather than merely a response with a matching commit. It is exported so
// downstream analysis boundaries can bind to the same canonical digest.
func VerifyResponseDigest(response Response) error {
	if response.ContractVersion != RepositorySnapshotV1 || response.ResultDigest == "" {
		return ErrInvalidRequest
	}
	digest, err := digestResponse(response)
	if err != nil {
		return err
	}
	if digest != response.ResultDigest {
		return ErrConflict
	}
	return nil
}

func validatePersistedOperation(operation Operation) error {
	if operation.ID == "" || operation.Status != "completed" ||
		operation.Response.ContractVersion != RepositorySnapshotV1 ||
		operation.Response.OperationID != operation.ID ||
		operation.Response.Repository.ProfileID != operation.RepositoryProfileID ||
		operation.Response.Ref.Requested != operation.RequestedRef ||
		operation.Response.ScopedRoot != operation.ScopedRoot ||
		operation.Response.AnalyzerVersion != operation.AnalyzerVersion ||
		operation.Response.ResultDigest != operation.ResultDigest ||
		operation.Response.ExactCommitSHA != operation.ExactCommitSHA {
		return errors.New("repositorysnapshot: invalid persisted operation")
	}
	digest, err := digestResponse(operation.Response)
	if err != nil {
		return err
	}
	if digest != operation.ResultDigest {
		return errors.New("repositorysnapshot: persisted result digest mismatch")
	}
	return nil
}

func newOperationID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("repository-snapshot-%d", time.Now().UnixNano())
	}
	return "repository-snapshot-" + base64.RawURLEncoding.EncodeToString(value)
}
