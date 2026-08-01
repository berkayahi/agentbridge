package localcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
)

// RepositoryUnderstandingRequest is the canonical product-neutral engine
// contract. ScopeID is opaque. Snapshot and prior-output references bind every
// role to durable operations; caller-supplied analysis JSON is never trusted.
type RepositoryUnderstandingRequest struct {
	ScopeID             string                                   `json:"scope_id"`
	Role                repositorysnapshot.AnalysisRole          `json:"role"`
	RepositoryProfileID string                                   `json:"repository_profile_id"`
	SnapshotCommit      string                                   `json:"snapshot_commit"`
	SnapshotDigest      string                                   `json:"snapshot_digest"`
	SnapshotOperationID string                                   `json:"snapshot_operation_id"`
	IdempotencyKey      string                                   `json:"idempotency_key"`
	PriorOutputs        []RepositoryUnderstandingOutputReference `json:"prior_outputs,omitempty"`
}

type RepositoryUnderstandingOutputReference struct {
	Role         repositorysnapshot.AnalysisRole `json:"role"`
	OperationID  string                          `json:"operation_id"`
	ResultDigest string                          `json:"result_digest"`
}

// RepositoryUnderstandingResponse is deliberately flat so consumers can use
// one endpoint and one DTO for investigator and synthesizer roles.
type RepositoryUnderstandingResponse struct {
	ContractVersion string                              `json:"contract_version"`
	OperationID     string                              `json:"operation_id"`
	ScopeID         string                              `json:"scope_id"`
	Role            repositorysnapshot.AnalysisRole     `json:"role"`
	ExactCommitSHA  string                              `json:"exact_commit_sha"`
	Provider        repositorysnapshot.ProviderMetadata `json:"provider"`
	Findings        []RepositoryUnderstandingFinding    `json:"findings"`
	Capabilities    []RepositoryUnderstandingCapability `json:"capabilities"`
	Assumptions     []string                            `json:"assumptions"`
	Conflicts       []string                            `json:"conflicts"`
	Unknowns        []string                            `json:"unknowns"`
	Status          string                              `json:"status"`
	ErrorCode       string                              `json:"error_code,omitempty"`
	ResultDigest    string                              `json:"result_digest"`
}

type RepositoryUnderstandingFinding struct {
	ID            string                            `json:"id"`
	Key           string                            `json:"key"`
	Summary       string                            `json:"summary"`
	Evidence      []UnderstandingEvidenceReference  `json:"evidence"`
	Assumptions   []string                          `json:"assumptions"`
	EvidenceState repositorysnapshot.KnowledgeState `json:"evidence_state"`
}

type RepositoryUnderstandingCapability struct {
	ID                   string                                            `json:"id"`
	Key                  string                                            `json:"key"`
	Name                 string                                            `json:"name"`
	Actor                string                                            `json:"actor"`
	VerifiedBehavior     string                                            `json:"verified_behavior"`
	ImplementationStatus repositorysnapshot.CapabilityImplementationStatus `json:"implementation_status"`
	Summary              string                                            `json:"summary"`
	Evidence             []UnderstandingEvidenceReference                  `json:"evidence"`
	Assumptions          []string                                          `json:"assumptions"`
	Confidence           *float64                                          `json:"confidence,omitempty"`
	EvidenceState        repositorysnapshot.KnowledgeState                 `json:"evidence_state"`
}

type UnderstandingEvidenceReference struct {
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	Observation string `json:"observation"`
	Digest      string `json:"digest,omitempty"`
}

func (s *Service) UnderstandRepository(ctx context.Context, request RepositoryUnderstandingRequest) (RepositoryUnderstandingResponse, error) {
	if s == nil || s.understanding == nil || strings.TrimSpace(request.ScopeID) == "" || len(request.ScopeID) > 128 || !request.Role.Valid() {
		return RepositoryUnderstandingResponse{}, ErrNotConfigured
	}
	snapshot, err := s.loadVerifiedSnapshot(ctx, request)
	if err != nil {
		return RepositoryUnderstandingResponse{}, err
	}
	commit := snapshot.ExactCommitSHA
	digest := snapshot.ResultDigest
	operationKey := strings.TrimSpace(request.IdempotencyKey)
	if operationKey == "" {
		operationKey = understandingKey(request.ScopeID, commit, digest, request.Role)
	}
	if !validID(operationKey) {
		return RepositoryUnderstandingResponse{}, repositorysnapshot.ErrInvalidRequest
	}
	analysis := repositorysnapshot.UnderstandingRequest{
		RepositoryProfileID: request.RepositoryProfileID, ExpectedCommitSHA: commit, Role: request.Role,
		ProviderID: "", IdempotencyKey: operationKey,
	}
	if request.Role == repositorysnapshot.RoleSynthesizer {
		analysis.Snapshot = &snapshot
		analysis.PriorOutputs, err = s.verifiedPriorOutputs(ctx, request, commit)
		if err != nil {
			return RepositoryUnderstandingResponse{}, err
		}
	} else {
		if len(request.PriorOutputs) != 0 {
			return RepositoryUnderstandingResponse{}, repositorysnapshot.ErrInvalidRequest
		}
		analysis.Paths, err = snapshotEvidencePaths(snapshot)
		if err != nil {
			return RepositoryUnderstandingResponse{}, err
		}
	}
	result, err := s.understanding.Understand(ctx, analysis)
	if err != nil {
		return RepositoryUnderstandingResponse{}, err
	}
	return repositoryUnderstandingResponse(request.ScopeID, result), nil
}

func (s *Service) loadVerifiedSnapshot(ctx context.Context, request RepositoryUnderstandingRequest) (repositorysnapshot.Response, error) {
	reader, ok := s.snapshots.(RepositorySnapshotOperationReader)
	operationID := strings.TrimSpace(request.SnapshotOperationID)
	if !ok || operationID == "" {
		return repositorysnapshot.Response{}, repositorysnapshot.ErrCommitMismatch
	}
	operation, err := reader.LoadOperation(ctx, operationID)
	if err != nil {
		if ctx.Err() != nil {
			return repositorysnapshot.Response{}, ctx.Err()
		}
		return repositorysnapshot.Response{}, repositorysnapshot.ErrCommitMismatch
	}
	if operation.ID != operationID || operation.RepositoryProfileID != request.RepositoryProfileID ||
		operation.ExactCommitSHA != request.SnapshotCommit || operation.ResultDigest != request.SnapshotDigest ||
		operation.Response.OperationID != operationID || operation.Response.Repository.ProfileID != request.RepositoryProfileID ||
		operation.Response.ExactCommitSHA != request.SnapshotCommit || operation.Response.ResultDigest != request.SnapshotDigest {
		return repositorysnapshot.Response{}, repositorysnapshot.ErrCommitMismatch
	}
	if err := repositorysnapshot.VerifyResponseDigest(operation.Response); err != nil {
		return repositorysnapshot.Response{}, repositorysnapshot.ErrCommitMismatch
	}
	return operation.Response, nil
}

func (s *Service) verifiedPriorOutputs(ctx context.Context, request RepositoryUnderstandingRequest, commit string) (map[repositorysnapshot.AnalysisRole]repositorysnapshot.PriorOutputReference, error) {
	roles := []repositorysnapshot.AnalysisRole{repositorysnapshot.RoleCartographer, repositorysnapshot.RoleProductArchaeologist, repositorysnapshot.RoleQualityOperations}
	if len(request.PriorOutputs) != len(roles) {
		return nil, repositorysnapshot.ErrInvalidRequest
	}
	provided := make(map[repositorysnapshot.AnalysisRole]RepositoryUnderstandingOutputReference, len(request.PriorOutputs))
	for _, reference := range request.PriorOutputs {
		if !reference.Role.Valid() || reference.Role == repositorysnapshot.RoleSynthesizer || strings.TrimSpace(reference.OperationID) == "" || strings.TrimSpace(reference.ResultDigest) == "" {
			return nil, repositorysnapshot.ErrInvalidRequest
		}
		if _, exists := provided[reference.Role]; exists {
			return nil, repositorysnapshot.ErrConflict
		}
		provided[reference.Role] = reference
	}
	result := make(map[repositorysnapshot.AnalysisRole]repositorysnapshot.PriorOutputReference, len(roles))
	for _, role := range roles {
		reference, ok := provided[role]
		if !ok {
			return nil, repositorysnapshot.ErrInvalidRequest
		}
		operation, err := s.understandingResultByID(ctx, reference.OperationID)
		if err != nil {
			return nil, err
		}
		if operation.Role != role || operation.RepositoryProfileID != request.RepositoryProfileID || operation.ExpectedCommitSHA != commit || operation.ResultDigest != reference.ResultDigest {
			return nil, repositorysnapshot.ErrConflict
		}
		result[role] = repositorysnapshot.PriorOutputReference{IdempotencyKey: operation.IdempotencyKey, ResultDigest: operation.ResultDigest}
	}
	return result, nil
}

func (s *Service) understandingResultByID(ctx context.Context, operationID string) (repositorysnapshot.UnderstandingOperation, error) {
	if value, ok := s.understanding.(*repositorysnapshot.UnderstandingService); ok {
		return value.LoadOperationByID(ctx, operationID)
	}
	return repositorysnapshot.UnderstandingOperation{}, ErrNotConfigured
}

func snapshotEvidencePaths(snapshot repositorysnapshot.Response) ([]string, error) {
	paths := make([]string, 0, len(snapshot.Observations))
	seen := make(map[string]struct{})
	for _, observation := range snapshot.Observations {
		path := strings.TrimSpace(observation.EvidencePath)
		if path == "" || !safeRoleEvidencePath(path) {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, repositorysnapshot.ErrEvidenceMissing
	}
	return paths, nil
}

func safeRoleEvidencePath(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	for _, component := range strings.Split(lower, "/") {
		if component == "" {
			continue
		}
		if strings.HasPrefix(component, ".env") || strings.Contains(component, "secret") || strings.Contains(component, "credential") ||
			component == "password" || component == "token" || component == "private" || component == "keys" || component == "id_rsa" || component == "id_ed25519" ||
			strings.HasSuffix(component, ".pem") || strings.HasSuffix(component, ".key") || strings.HasSuffix(component, ".p12") || strings.HasSuffix(component, ".pfx") {
			return false
		}
	}
	return true
}

func understandingKey(scopeID, commit, digest string, role repositorysnapshot.AnalysisRole) string {
	seed := strings.Join([]string{scopeID, commit, digest, string(role)}, "\x00")
	sum := sha256.Sum256([]byte(seed))
	return "repository-understanding-" + hex.EncodeToString(sum[:16])
}

func repositoryUnderstandingResponse(scopeID string, response repositorysnapshot.AnalysisResponse) RepositoryUnderstandingResponse {
	result := RepositoryUnderstandingResponse{
		ContractVersion: response.ContractVersion, OperationID: response.OperationID, ScopeID: scopeID,
		Role: response.Role, ExactCommitSHA: response.ExactCommitSHA, Provider: response.Provider,
		Findings: []RepositoryUnderstandingFinding{}, Capabilities: []RepositoryUnderstandingCapability{},
		Assumptions: append([]string(nil), response.Assumptions...), Conflicts: append([]string(nil), response.Conflicts...),
		Unknowns: append([]string(nil), response.Unknowns...), Status: response.Status, ErrorCode: response.ErrorCode,
		ResultDigest: response.ResultDigest,
	}
	for _, finding := range response.Findings {
		result.Findings = append(result.Findings, RepositoryUnderstandingFinding{
			ID: finding.ID, Key: finding.Area, Summary: finding.Statement,
			Evidence:    understandingEvidenceReferences(response.Evidence, finding.EvidencePaths, finding.Statement),
			Assumptions: append([]string(nil), response.Assumptions...), EvidenceState: finding.KnowledgeState,
		})
	}
	for _, capability := range response.Capabilities {
		result.Capabilities = append(result.Capabilities, RepositoryUnderstandingCapability{
			ID: capability.ID, Key: capability.ID, Name: capability.Name, Actor: capability.Actor,
			VerifiedBehavior: capability.VerifiedBehavior, ImplementationStatus: capability.ImplementationStatus,
			Summary:     capability.VerifiedBehavior,
			Evidence:    understandingEvidenceReferences(response.Evidence, capability.EvidencePaths, capability.VerifiedBehavior),
			Assumptions: append([]string(nil), response.Assumptions...), Confidence: capability.Confidence,
			EvidenceState: capability.KnowledgeState,
		})
	}
	return result
}

func understandingEvidenceReferences(evidence []repositorysnapshot.EvidenceReference, paths []string, observation string) []UnderstandingEvidenceReference {
	digests := make(map[string]string, len(evidence))
	for _, reference := range evidence {
		digests[reference.Path] = reference.ContentDigest
	}
	refs := make([]UnderstandingEvidenceReference, 0, len(paths))
	for _, path := range paths {
		refs = append(refs, UnderstandingEvidenceReference{Kind: "file", Source: path, Observation: observation, Digest: digests[path]})
	}
	return refs
}
