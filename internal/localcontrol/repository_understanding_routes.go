package localcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	"github.com/google/uuid"
)

// UnderstandingRoleRequest is the AgentBridge-side wire contract consumed by
// Platform's role adapter. PriorOutputs are accepted for compatibility with
// the adapter, but the service never uses them as authoritative input.
type UnderstandingRoleRequest struct {
	ProjectID           string                      `json:"project_id"`
	Role                string                      `json:"role"`
	RepositoryProfileID string                      `json:"repository_profile_id"`
	SnapshotCommit      string                      `json:"snapshot_commit"`
	SnapshotDigest      string                      `json:"snapshot_digest"`
	SnapshotOperationID string                      `json:"snapshot_operation_id,omitempty"`
	Snapshot            repositorysnapshot.Response `json:"snapshot"`
	PriorOutputs        []json.RawMessage           `json:"prior_outputs,omitempty"`
}

type UnderstandingRoleResponse struct {
	Output UnderstandingRoleOutput `json:"output"`
}

// UnderstandingRoleOutput is intentionally a small compatibility projection
// of the Platform role DTO. The authoritative AgentBridge result remains the
// persisted AnalysisResponse and its exact result digest.
type UnderstandingRoleOutput struct {
	ProviderAgent string                        `json:"provider_agent,omitempty"`
	Role          string                        `json:"role"`
	Claims        []UnderstandingRoleClaim      `json:"claims"`
	Capabilities  []UnderstandingRoleCapability `json:"capabilities"`
	Summary       string                        `json:"summary"`
}

type UnderstandingRoleClaim struct {
	ID               uuid.UUID                         `json:"id"`
	Key              string                            `json:"key"`
	Summary          string                            `json:"summary"`
	Evidence         []UnderstandingEvidenceReference  `json:"evidence"`
	Assumptions      []string                          `json:"assumptions"`
	Confidence       float64                           `json:"confidence"`
	EvidenceState    repositorysnapshot.KnowledgeState `json:"evidence_state"`
	ReviewState      string                            `json:"review_state"`
	RepositoryCommit string                            `json:"repository_commit"`
	Role             string                            `json:"role"`
	Agent            string                            `json:"agent"`
	EvidenceDigest   string                            `json:"evidence_digest"`
}

type UnderstandingRoleCapability struct {
	ID                     uuid.UUID                         `json:"id"`
	Key                    string                            `json:"key"`
	Name                   string                            `json:"name"`
	Actor                  string                            `json:"actor"`
	VerifiedBehavior       string                            `json:"verified_behavior"`
	ImplementationStatus   string                            `json:"implementation_status"`
	Summary                string                            `json:"summary"`
	Evidence               []UnderstandingEvidenceReference  `json:"evidence"`
	Assumptions            []string                          `json:"assumptions"`
	Confidence             float64                           `json:"confidence"`
	PublicEligible         bool                              `json:"public_eligible"`
	MarketingClaimEligible bool                              `json:"marketing_claim_eligible"`
	EvidenceState          repositorysnapshot.KnowledgeState `json:"evidence_state"`
	ReviewState            string                            `json:"review_state"`
	RepositoryCommit       string                            `json:"repository_commit"`
	Role                   string                            `json:"role"`
	Agent                  string                            `json:"agent"`
	EvidenceDigest         string                            `json:"evidence_digest"`
}

type UnderstandingEvidenceReference struct {
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	Observation string `json:"observation"`
	Digest      string `json:"digest,omitempty"`
}

func (s *Service) UnderstandRepositoryRole(ctx context.Context, request UnderstandingRoleRequest) (UnderstandingRoleResponse, error) {
	return s.understandPlatformRole(ctx, request, false)
}

func (s *Service) SynthesizeRepositoryUnderstanding(ctx context.Context, request UnderstandingRoleRequest) (UnderstandingRoleResponse, error) {
	return s.understandPlatformRole(ctx, request, true)
}

func (s *Service) understandPlatformRole(ctx context.Context, request UnderstandingRoleRequest, synthesize bool) (UnderstandingRoleResponse, error) {
	if s == nil || s.understanding == nil || strings.TrimSpace(request.ProjectID) == "" || len(request.ProjectID) > 128 {
		return UnderstandingRoleResponse{}, ErrNotConfigured
	}
	snapshot, err := s.loadVerifiedSnapshot(ctx, request)
	if err != nil {
		return UnderstandingRoleResponse{}, err
	}
	role, err := platformRole(request.Role)
	if err != nil {
		return UnderstandingRoleResponse{}, err
	}
	commit := snapshot.ExactCommitSHA
	digest := snapshot.ResultDigest
	analysis := repositorysnapshot.UnderstandingRequest{
		RepositoryProfileID: request.RepositoryProfileID, ExpectedCommitSHA: commit, Role: role,
		ProviderID: "", IdempotencyKey: platformUnderstandingKey(request.ProjectID, commit, digest, role),
		Snapshot: nil,
	}
	if synthesize {
		analysis.Snapshot = &snapshot
		analysis.PriorOutputs = make(map[repositorysnapshot.AnalysisRole]repositorysnapshot.PriorOutputReference, 3)
		for _, priorRole := range []repositorysnapshot.AnalysisRole{repositorysnapshot.RoleCartographer, repositorysnapshot.RoleProductArchaeologist, repositorysnapshot.RoleQualityOperations} {
			priorKey := platformUnderstandingKey(request.ProjectID, commit, digest, priorRole)
			prior, err := s.understandingResult(ctx, priorKey)
			if err != nil {
				return UnderstandingRoleResponse{}, err
			}
			analysis.PriorOutputs[priorRole] = repositorysnapshot.PriorOutputReference{IdempotencyKey: priorKey, ResultDigest: prior.ResultDigest}
		}
	} else {
		analysis.Paths, err = snapshotEvidencePaths(snapshot)
		if err != nil {
			return UnderstandingRoleResponse{}, err
		}
	}
	result, err := s.understanding.Understand(ctx, analysis)
	if err != nil {
		return UnderstandingRoleResponse{}, err
	}
	if result.Status != "completed" {
		return UnderstandingRoleResponse{}, repositorysnapshot.ErrProviderNotConfigured
	}
	return UnderstandingRoleResponse{Output: projectUnderstandingRole(result, digest)}, nil
}

func (s *Service) loadVerifiedSnapshot(ctx context.Context, request UnderstandingRoleRequest) (repositorysnapshot.Response, error) {
	reader, ok := s.snapshots.(RepositorySnapshotOperationReader)
	operationID := strings.TrimSpace(request.SnapshotOperationID)
	if operationID == "" {
		operationID = strings.TrimSpace(request.Snapshot.OperationID)
	}
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
	if operation.ID != operationID ||
		operation.RepositoryProfileID != request.RepositoryProfileID ||
		operation.ExactCommitSHA != request.SnapshotCommit ||
		operation.ResultDigest != request.SnapshotDigest ||
		operation.Response.OperationID != operationID ||
		operation.Response.Repository.ProfileID != request.RepositoryProfileID ||
		operation.Response.ExactCommitSHA != request.SnapshotCommit ||
		operation.Response.ResultDigest != request.SnapshotDigest {
		return repositorysnapshot.Response{}, repositorysnapshot.ErrCommitMismatch
	}
	if request.Snapshot.OperationID != "" {
		if request.Snapshot.OperationID != operationID ||
			request.Snapshot.Repository.ProfileID != request.RepositoryProfileID ||
			request.Snapshot.ExactCommitSHA != request.SnapshotCommit ||
			request.Snapshot.ResultDigest != request.SnapshotDigest {
			return repositorysnapshot.Response{}, repositorysnapshot.ErrCommitMismatch
		}
		if err := repositorysnapshot.VerifyResponseDigest(request.Snapshot); err != nil {
			return repositorysnapshot.Response{}, repositorysnapshot.ErrCommitMismatch
		}
	}
	if err := repositorysnapshot.VerifyResponseDigest(operation.Response); err != nil {
		return repositorysnapshot.Response{}, repositorysnapshot.ErrCommitMismatch
	}
	return operation.Response, nil
}

func (s *Service) understandingResult(ctx context.Context, key string) (repositorysnapshot.UnderstandingOperation, error) {
	// The service boundary intentionally exposes only Understand. Prior results
	// are loaded by the underlying durable operation store through this narrow
	// helper when the configured authority is the repository understanding
	// service.
	if value, ok := s.understanding.(*repositorysnapshot.UnderstandingService); ok {
		return value.LoadOperation(ctx, key)
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

// Role analysis may use only committed paths that are safe to materialize in
// the disposable provider workspace. M1 can observe an .env.example file for
// architecture metadata, but that file may still contain a canary or secret-
// shaped assignment; it must never be re-read for provider analysis.
func safeRoleEvidencePath(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	base := lower
	if slash := strings.LastIndexByte(base, '/'); slash >= 0 {
		base = base[slash+1:]
	}
	if strings.HasPrefix(base, ".env") || strings.Contains(base, "secret") || strings.Contains(base, "credential") ||
		base == "password" || base == "token" || base == "id_rsa" || base == "id_ed25519" ||
		strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".p12") || strings.HasSuffix(base, ".pfx") {
		return false
	}
	return true
}

func platformRole(value string) (repositorysnapshot.AnalysisRole, error) {
	switch strings.TrimSpace(value) {
	case "Repository Cartographer":
		return repositorysnapshot.RoleCartographer, nil
	case "Product Archaeologist":
		return repositorysnapshot.RoleProductArchaeologist, nil
	case "Quality/Operations Analyst":
		return repositorysnapshot.RoleQualityOperations, nil
	case "Baseline Synthesizer":
		return repositorysnapshot.RoleSynthesizer, nil
	default:
		return "", repositorysnapshot.ErrUnknownRole
	}
}

func platformUnderstandingKey(projectID, commit, digest string, role repositorysnapshot.AnalysisRole) string {
	seed := strings.Join([]string{projectID, commit, digest, string(role)}, "\x00")
	sum := sha256.Sum256([]byte(seed))
	return "platform-understanding-" + hex.EncodeToString(sum[:16])
}

func projectUnderstandingRole(response repositorysnapshot.AnalysisResponse, snapshotDigest string) UnderstandingRoleOutput {
	roleName := platformRoleName(response.Role)
	output := UnderstandingRoleOutput{ProviderAgent: response.Provider.ID, Role: roleName, Claims: []UnderstandingRoleClaim{}, Capabilities: []UnderstandingRoleCapability{}}
	for index, finding := range response.Findings {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("%s:finding:%d:%s", response.ResultDigest, index, finding.ID)))
		refs := make([]UnderstandingEvidenceReference, 0, len(finding.EvidencePaths))
		for _, path := range finding.EvidencePaths {
			refs = append(refs, UnderstandingEvidenceReference{Kind: "file", Source: path, Observation: finding.Statement, Digest: snapshotDigest})
		}
		output.Claims = append(output.Claims, UnderstandingRoleClaim{ID: id, Key: "finding-" + fmt.Sprint(index), Summary: finding.Statement, Evidence: refs, Assumptions: append([]string(nil), response.Assumptions...), EvidenceState: finding.KnowledgeState, ReviewState: "pending", RepositoryCommit: response.ExactCommitSHA, Role: roleName, Agent: response.Provider.ID, EvidenceDigest: snapshotDigest})
	}
	for index, capability := range response.Capabilities {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("%s:capability:%d", response.ResultDigest, index)))
		output.Capabilities = append(output.Capabilities, UnderstandingRoleCapability{ID: id, Key: "capability-" + fmt.Sprint(index), Name: capability, Actor: "unknown", VerifiedBehavior: capability, ImplementationStatus: "unknown", Summary: capability, Evidence: []UnderstandingEvidenceReference{}, Assumptions: append([]string(nil), response.Assumptions...), EvidenceState: repositorysnapshot.KnowledgeUnknown, ReviewState: "pending", RepositoryCommit: response.ExactCommitSHA, Role: roleName, Agent: response.Provider.ID, EvidenceDigest: snapshotDigest})
	}
	return output
}

func platformRoleName(role repositorysnapshot.AnalysisRole) string {
	switch role {
	case repositorysnapshot.RoleCartographer:
		return "Repository Cartographer"
	case repositorysnapshot.RoleProductArchaeologist:
		return "Product Archaeologist"
	case repositorysnapshot.RoleQualityOperations:
		return "Quality/Operations Analyst"
	case repositorysnapshot.RoleSynthesizer:
		return "Baseline Synthesizer"
	default:
		return string(role)
	}
}
