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
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/store"
)

const (
	UnderstandingContractV1 = "repository-understanding-v1"
	MaxProviderOutputBytes  = 64 << 10
	MaxAnalysisFindings     = 128
	MaxAnalysisStrings      = 128
	MaxAnalysisStringBytes  = 4 << 10
)

var providerSecretAssignment = regexp.MustCompile(`(?im)(^|[\s\{,])["']?[A-Za-z_][A-Za-z0-9_-]*(?:token|key|secret|password|credential|authorization)[A-Za-z0-9_-]*["']?\s*[:=]\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^,\r\n\}\s]+)`)

var (
	ErrUnknownRole           = errors.New("repositorysnapshot: unknown analysis role")
	ErrProviderNotConfigured = errors.New("repositorysnapshot: analysis provider is not configured")
	ErrProviderPolicy        = errors.New("repositorysnapshot: analysis provider does not expose the safe policy boundary")
	ErrProviderApproval      = errors.New("repositorysnapshot: provider approval was declined")
	ErrProviderOutput        = errors.New("repositorysnapshot: provider output is invalid")
	ErrProviderOutputBounds  = errors.New("repositorysnapshot: provider output exceeds bounds")
)

type AnalysisRole string

const (
	RoleCartographer         AnalysisRole = "cartographer"
	RoleProductArchaeologist AnalysisRole = "product_archaeologist"
	RoleQualityOperations    AnalysisRole = "quality_operations"
	RoleSynthesizer          AnalysisRole = "synthesizer"
)

func (r AnalysisRole) Valid() bool {
	switch r {
	case RoleCartographer, RoleProductArchaeologist, RoleQualityOperations, RoleSynthesizer:
		return true
	default:
		return false
	}
}

type KnowledgeState string

const (
	KnowledgeObserved    KnowledgeState = "observed"
	KnowledgeDeclared    KnowledgeState = "declared"
	KnowledgeInferred    KnowledgeState = "inferred"
	KnowledgeUnknown     KnowledgeState = "unknown"
	KnowledgeConflicting KnowledgeState = "conflicting"
)

func (s KnowledgeState) Valid() bool {
	switch s {
	case KnowledgeObserved, KnowledgeDeclared, KnowledgeInferred, KnowledgeUnknown, KnowledgeConflicting:
		return true
	default:
		return false
	}
}

type Finding struct {
	ID             string         `json:"id"`
	Statement      string         `json:"statement"`
	KnowledgeState KnowledgeState `json:"knowledge_state"`
	EvidencePaths  []string       `json:"evidence_paths,omitempty"`
}

// StructuredOutput is the only shape accepted from a provider. It deliberately
// contains conclusions, not transcripts, tool calls, or hidden reasoning.
type StructuredOutput struct {
	Role         AnalysisRole `json:"role"`
	Findings     []Finding    `json:"findings"`
	Capabilities []string     `json:"capabilities"`
	Assumptions  []string     `json:"assumptions"`
	Conflicts    []string     `json:"conflicts"`
	Unknowns     []string     `json:"unknowns"`
}

type EvidenceReference struct {
	Path          string `json:"path"`
	ContentDigest string `json:"content_digest"`
	Size          int    `json:"size"`
}

type ProviderMetadata struct {
	ID        string `json:"id"`
	Model     string `json:"model,omitempty"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code,omitempty"`
}

type AnalysisResponse struct {
	ContractVersion string              `json:"contract_version"`
	OperationID     string              `json:"operation_id"`
	Role            AnalysisRole        `json:"role"`
	ExactCommitSHA  string              `json:"exact_commit_sha"`
	Evidence        []EvidenceReference `json:"evidence"`
	Findings        []Finding           `json:"findings"`
	Capabilities    []string            `json:"capabilities"`
	Assumptions     []string            `json:"assumptions"`
	Conflicts       []string            `json:"conflicts"`
	Unknowns        []string            `json:"unknowns"`
	Provider        ProviderMetadata    `json:"provider"`
	Status          string              `json:"status"`
	ErrorCode       string              `json:"error_code,omitempty"`
	ResultDigest    string              `json:"result_digest"`
}

type UnderstandingRequest struct {
	RepositoryProfileID string                                `json:"repository_profile_id"`
	ExpectedCommitSHA   string                                `json:"expected_commit_sha"`
	Paths               []string                              `json:"paths,omitempty"`
	Role                AnalysisRole                          `json:"role"`
	ProviderID          string                                `json:"provider_id,omitempty"`
	Model               string                                `json:"model,omitempty"`
	IdempotencyKey      string                                `json:"idempotency_key"`
	Snapshot            *Response                             `json:"snapshot,omitempty"`
	PriorOutputs        map[AnalysisRole]PriorOutputReference `json:"prior_outputs,omitempty"`
}

// PriorOutputReference binds synthesis to an already persisted role result.
// The synthesizer never accepts caller-supplied role JSON as authoritative.
type PriorOutputReference struct {
	IdempotencyKey string `json:"idempotency_key"`
	ResultDigest   string `json:"result_digest"`
}

type ProviderRequest struct {
	Role           AnalysisRole
	ExactCommitSHA string
	WorkspacePath  string
	Prompt         string
	Model          string
}

type ProviderResult struct {
	ProviderID        string
	Model             string
	Output            []byte
	ApprovalRequested bool
}

type AnalysisProvider interface {
	Analyze(context.Context, ProviderRequest) (ProviderResult, error)
}

type UnderstandingOperation struct {
	ID                  string
	IdempotencyKey      string
	RepositoryProfileID string
	ExpectedCommitSHA   string
	Role                AnalysisRole
	RequestDigest       string
	ResultDigest        string
	Status              string
	Response            AnalysisResponse
	RequestedAt         time.Time
	CompletedAt         time.Time
}

type UnderstandingStore interface {
	LoadRepositoryUnderstanding(context.Context, string) (UnderstandingOperation, error)
	SaveRepositoryUnderstanding(context.Context, UnderstandingOperation) error
}

type UnderstandingConfig struct {
	Store           UnderstandingStore
	Catalog         Catalog
	Evidence        EvidenceReader
	Providers       map[string]AnalysisProvider
	DefaultProvider string
	Clock           func() time.Time
	NewID           func() string
}

type UnderstandingService struct {
	mu              sync.Mutex
	store           UnderstandingStore
	catalog         Catalog
	evidence        EvidenceReader
	providers       map[string]AnalysisProvider
	defaultProvider string
	clock           func() time.Time
	newID           func() string
}

func NewUnderstandingService(config UnderstandingConfig) (*UnderstandingService, error) {
	if config.Store == nil || config.Catalog == nil {
		return nil, ErrInvalidRequest
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.NewID == nil {
		config.NewID = newUnderstandingID
	}
	return &UnderstandingService{
		store: config.Store, catalog: config.Catalog, evidence: config.Evidence,
		providers: config.Providers, defaultProvider: config.DefaultProvider,
		clock: config.Clock, newID: config.NewID,
	}, nil
}

func (s *UnderstandingService) Understand(ctx context.Context, request UnderstandingRequest) (AnalysisResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalized, err := normalizeUnderstandingRequest(request)
	if err != nil {
		return AnalysisResponse{}, err
	}
	requestDigest := digestUnderstandingRequest(normalized)
	if existing, err := s.store.LoadRepositoryUnderstanding(ctx, normalized.IdempotencyKey); err == nil {
		if existing.RequestDigest != requestDigest {
			return AnalysisResponse{}, ErrConflict
		}
		if err := validateUnderstandingOperation(existing); err != nil {
			return AnalysisResponse{}, err
		}
		return existing.Response, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return AnalysisResponse{}, fmt.Errorf("load repository understanding operation: %w", err)
	}

	profile, err := s.catalog.ResolveRepositoryProfile(ctx, normalized.RepositoryProfileID)
	if err != nil {
		return AnalysisResponse{}, err
	}
	if profile.ProfileID != normalized.RepositoryProfileID || !filepath.IsAbs(profile.CheckoutPath) {
		return AnalysisResponse{}, ErrNotConfigured
	}
	providerID := normalized.ProviderID
	if providerID == "" {
		providerID = s.defaultProvider
	}
	response := AnalysisResponse{
		ContractVersion: UnderstandingContractV1, OperationID: s.newID(), Role: normalized.Role,
		ExactCommitSHA: normalized.ExpectedCommitSHA, Evidence: []EvidenceReference{},
		Findings: []Finding{}, Capabilities: []string{}, Assumptions: []string{},
		Conflicts: []string{}, Unknowns: []string{}, Status: "not_configured", ErrorCode: "provider_not_configured",
		Provider: ProviderMetadata{ID: providerID, Status: "not_configured", ErrorCode: "provider_not_configured"},
	}
	analysisProvider := s.providers[providerID]
	if analysisProvider == nil {
		return s.persistUnderstanding(ctx, normalized, requestDigest, response)
	}

	workspace, evidence, err := s.prepareWorkspace(ctx, profile, normalized)
	if err != nil {
		return AnalysisResponse{}, err
	}
	defer os.RemoveAll(workspace)
	response.Evidence = evidence
	providerResult, err := analysisProvider.Analyze(ctx, ProviderRequest{
		Role: normalized.Role, ExactCommitSHA: normalized.ExpectedCommitSHA,
		WorkspacePath: workspace, Prompt: analysisPrompt(normalized.Role), Model: normalized.Model,
	})
	if err != nil {
		return AnalysisResponse{}, err
	}
	if providerResult.ApprovalRequested {
		return AnalysisResponse{}, ErrProviderApproval
	}
	output, err := decodeStructuredOutput(providerResult.Output, normalized.Role)
	if err != nil {
		return AnalysisResponse{}, err
	}
	if err := validateStructuredOutput(output, normalized.Role, evidence); err != nil {
		return AnalysisResponse{}, err
	}
	response.Findings, response.Capabilities, response.Assumptions = output.Findings, output.Capabilities, output.Assumptions
	response.Conflicts, response.Unknowns = output.Conflicts, output.Unknowns
	response.Provider = ProviderMetadata{ID: providerID, Model: providerResult.Model, Status: "completed"}
	response.Status = "completed"
	response.ErrorCode = ""
	return s.persistUnderstanding(ctx, normalized, requestDigest, response)
}

// LoadOperation returns a validated durable role result for a higher-level
// adapter. Callers receive the persisted response, never a caller-supplied
// replacement, so synthesis can bind prior outputs by digest.
func (s *UnderstandingService) LoadOperation(ctx context.Context, idempotencyKey string) (UnderstandingOperation, error) {
	if s == nil || strings.TrimSpace(idempotencyKey) == "" {
		return UnderstandingOperation{}, ErrInvalidRequest
	}
	operation, err := s.store.LoadRepositoryUnderstanding(ctx, idempotencyKey)
	if err != nil {
		return UnderstandingOperation{}, err
	}
	if err := validateUnderstandingOperation(operation); err != nil {
		return UnderstandingOperation{}, err
	}
	return operation, nil
}

func (s *UnderstandingService) prepareWorkspace(ctx context.Context, profile ConfiguredRepository, request UnderstandingRequest) (string, []EvidenceReference, error) {
	if request.Role != RoleSynthesizer {
		if s.evidence == nil {
			return "", nil, ErrNotConfigured
		}
		packet, err := s.evidence.RetrieveEvidence(ctx, profile, EvidenceRequest{
			RepositoryProfileID: request.RepositoryProfileID, ExpectedCommitSHA: request.ExpectedCommitSHA, Paths: request.Paths,
		})
		if err != nil {
			return "", nil, err
		}
		if packet.RepositoryProfileID != request.RepositoryProfileID {
			return "", nil, ErrNotConfigured
		}
		if packet.ExactCommitSHA != request.ExpectedCommitSHA {
			return "", nil, ErrCommitMismatch
		}
		workspace, err := os.MkdirTemp("", "agentbridge-understanding-")
		if err != nil {
			return "", nil, fmt.Errorf("create disposable evidence workspace: %w", err)
		}
		evidence := make([]EvidenceReference, 0, len(packet.Files))
		for _, file := range packet.Files {
			target := filepath.Join(workspace, filepath.FromSlash(file.Path))
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil || !filepath.IsAbs(target) {
				_ = os.RemoveAll(workspace)
				return "", nil, ErrEvidenceMissing
			}
			if err := os.WriteFile(target, []byte(file.Content), 0o600); err != nil {
				_ = os.RemoveAll(workspace)
				return "", nil, fmt.Errorf("populate disposable evidence workspace: %w", err)
			}
			evidence = append(evidence, EvidenceReference{Path: file.Path, ContentDigest: file.ContentDigest, Size: file.Size})
		}
		return workspace, evidence, nil
	}
	if request.Snapshot == nil || request.Snapshot.ExactCommitSHA != request.ExpectedCommitSHA || request.Snapshot.Repository.ProfileID != request.RepositoryProfileID {
		return "", nil, ErrCommitMismatch
	}
	if err := VerifyResponseDigest(*request.Snapshot); err != nil {
		return "", nil, ErrCommitMismatch
	}
	roles := []AnalysisRole{RoleCartographer, RoleProductArchaeologist, RoleQualityOperations}
	prior := make(map[AnalysisRole]UnderstandingOperation, len(roles))
	for _, role := range roles {
		ref, ok := request.PriorOutputs[role]
		if !ok || ref.IdempotencyKey == "" || ref.ResultDigest == "" {
			return "", nil, fmt.Errorf("synthesizer input %s: %w", role, ErrInvalidRequest)
		}
		operation, err := s.store.LoadRepositoryUnderstanding(ctx, ref.IdempotencyKey)
		if err != nil {
			return "", nil, fmt.Errorf("load synthesizer input %s: %w", role, ErrInvalidRequest)
		}
		if operation.Role != role || operation.Status != "completed" || operation.RepositoryProfileID != request.RepositoryProfileID || operation.ExpectedCommitSHA != request.ExpectedCommitSHA || operation.ResultDigest != ref.ResultDigest {
			return "", nil, fmt.Errorf("synthesizer input %s: %w", role, ErrConflict)
		}
		if err := validateUnderstandingOperation(operation); err != nil {
			return "", nil, fmt.Errorf("synthesizer input %s: %w", role, err)
		}
		prior[role] = operation
	}
	workspace, err := os.MkdirTemp("", "agentbridge-understanding-")
	if err != nil {
		return "", nil, fmt.Errorf("create disposable synthesis workspace: %w", err)
	}
	encoded, err := json.Marshal(request.Snapshot)
	if err != nil {
		_ = os.RemoveAll(workspace)
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(workspace, "m1-snapshot.json"), encoded, 0o600); err != nil {
		_ = os.RemoveAll(workspace)
		return "", nil, err
	}
	digest := sha256.Sum256(encoded)
	evidence := []EvidenceReference{{Path: "m1-snapshot.json", ContentDigest: "sha256:" + hex.EncodeToString(digest[:]), Size: len(encoded)}}
	for _, role := range roles {
		encoded, err := json.Marshal(prior[role].Response)
		if err != nil {
			_ = os.RemoveAll(workspace)
			return "", nil, err
		}
		name := "prior-" + string(role) + ".json"
		if err := os.WriteFile(filepath.Join(workspace, name), encoded, 0o600); err != nil {
			_ = os.RemoveAll(workspace)
			return "", nil, err
		}
		digest := sha256.Sum256(encoded)
		evidence = append(evidence, EvidenceReference{Path: name, ContentDigest: "sha256:" + hex.EncodeToString(digest[:]), Size: len(encoded)})
	}
	return workspace, evidence, nil
}

func (s *UnderstandingService) persistUnderstanding(ctx context.Context, request UnderstandingRequest, requestDigest string, response AnalysisResponse) (AnalysisResponse, error) {
	var err error
	response.ResultDigest, err = digestAnalysisResponse(response)
	if err != nil {
		return AnalysisResponse{}, err
	}
	now := s.clock().UTC()
	operation := UnderstandingOperation{
		ID: response.OperationID, IdempotencyKey: request.IdempotencyKey,
		RepositoryProfileID: request.RepositoryProfileID, ExpectedCommitSHA: request.ExpectedCommitSHA,
		Role: request.Role, RequestDigest: requestDigest, ResultDigest: response.ResultDigest,
		Status: response.Status, Response: response, RequestedAt: now, CompletedAt: now,
	}
	if err := s.store.SaveRepositoryUnderstanding(ctx, operation); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return AnalysisResponse{}, fmt.Errorf("save repository understanding operation: %w", err)
		}
		existing, loadErr := s.store.LoadRepositoryUnderstanding(ctx, request.IdempotencyKey)
		if loadErr != nil || existing.RequestDigest != requestDigest {
			return AnalysisResponse{}, ErrConflict
		}
		if err := validateUnderstandingOperation(existing); err != nil {
			return AnalysisResponse{}, err
		}
		return existing.Response, nil
	}
	return response, nil
}

func normalizeUnderstandingRequest(request UnderstandingRequest) (UnderstandingRequest, error) {
	request.RepositoryProfileID = strings.TrimSpace(request.RepositoryProfileID)
	request.ExpectedCommitSHA = strings.ToLower(strings.TrimSpace(request.ExpectedCommitSHA))
	request.ProviderID = strings.TrimSpace(request.ProviderID)
	request.Model = strings.TrimSpace(request.Model)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if !safeIdentifier.MatchString(request.RepositoryProfileID) || !fullObjectID.MatchString(request.ExpectedCommitSHA) || !request.Role.Valid() || !safeIdentifier.MatchString(request.IdempotencyKey) || (request.ProviderID != "" && !safeIdentifier.MatchString(request.ProviderID)) || len(request.Model) > 128 {
		return UnderstandingRequest{}, ErrInvalidRequest
	}
	if request.Role == RoleSynthesizer {
		if len(request.Paths) != 0 {
			return UnderstandingRequest{}, ErrInvalidRequest
		}
	} else if len(request.Paths) == 0 || len(request.Paths) > MaxEvidencePaths {
		return UnderstandingRequest{}, ErrInvalidRequest
	} else {
		for index, value := range request.Paths {
			cleaned, err := normalizeEvidencePath(value)
			if err != nil {
				return UnderstandingRequest{}, err
			}
			request.Paths[index] = cleaned
		}
	}
	if len(request.PriorOutputs) > 3 {
		return UnderstandingRequest{}, ErrInvalidRequest
	}
	if request.Role == RoleSynthesizer && len(request.PriorOutputs) != 3 {
		return UnderstandingRequest{}, ErrInvalidRequest
	}
	if request.Role != RoleSynthesizer && len(request.PriorOutputs) != 0 {
		return UnderstandingRequest{}, ErrInvalidRequest
	}
	return request, nil
}

func digestUnderstandingRequest(request UnderstandingRequest) string {
	encoded, _ := json.Marshal(request)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func digestAnalysisResponse(response AnalysisResponse) (string, error) {
	value := response
	value.OperationID, value.ResultDigest = "", ""
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode analysis result digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func decodeStructuredOutput(contents []byte, role AnalysisRole) (StructuredOutput, error) {
	if len(contents) == 0 || len(contents) > MaxProviderOutputBytes {
		return StructuredOutput{}, ErrProviderOutputBounds
	}
	if providerOutputContainsUnsafeContent(contents) {
		return StructuredOutput{}, ErrProviderOutput
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	var output StructuredOutput
	if err := decoder.Decode(&output); err != nil {
		return StructuredOutput{}, ErrProviderOutput
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return StructuredOutput{}, ErrProviderOutput
	}
	if output.Role != role {
		return StructuredOutput{}, ErrProviderOutput
	}
	return output, nil
}

func validateStructuredOutput(output StructuredOutput, role AnalysisRole, evidence []EvidenceReference) error {
	if output.Role != role || len(output.Findings) > MaxAnalysisFindings || len(output.Capabilities) > MaxAnalysisStrings || len(output.Assumptions) > MaxAnalysisStrings || len(output.Conflicts) > MaxAnalysisStrings || len(output.Unknowns) > MaxAnalysisStrings {
		return ErrProviderOutput
	}
	allowed := make(map[string]struct{}, len(evidence))
	for _, ref := range evidence {
		allowed[ref.Path] = struct{}{}
	}
	for _, finding := range output.Findings {
		if finding.ID == "" || len(finding.ID) > 128 || finding.Statement == "" || len(finding.Statement) > MaxAnalysisStringBytes || !finding.KnowledgeState.Valid() {
			return ErrProviderOutput
		}
		if unsafeProviderString(finding.ID) || unsafeProviderString(finding.Statement) {
			return ErrProviderOutput
		}
		if evidence != nil {
			for _, evidencePath := range finding.EvidencePaths {
				if _, ok := allowed[evidencePath]; !ok || unsafeProviderString(evidencePath) {
					return ErrProviderOutput
				}
			}
		}
	}
	for _, values := range [][]string{output.Capabilities, output.Assumptions, output.Conflicts, output.Unknowns} {
		for _, value := range values {
			if value == "" || len(value) > MaxAnalysisStringBytes || unsafeProviderString(value) {
				return ErrProviderOutput
			}
		}
	}
	return nil
}

func providerOutputContainsUnsafeContent(contents []byte) bool {
	for _, value := range contents {
		if value < 0x20 && value != '\t' && value != '\n' && value != '\r' {
			return true
		}
	}
	for index := 0; index+1 < len(contents); index++ {
		if contents[index] != '\\' {
			continue
		}
		next := contents[index+1]
		if strings.ContainsRune("btnfr", rune(next)) {
			return true
		}
		if next != 'u' || index+5 >= len(contents) || contents[index+2] != '0' || contents[index+3] != '0' || (contents[index+4] != '0' && contents[index+4] != '1') || !strings.ContainsRune("0123456789abcdefABCDEF", rune(contents[index+5])) {
			continue
		}
		return true
	}
	return privateKeyLike(string(contents)) || bearerValue.Match(contents) || secretAssignment.Match(contents) || secretEnvAssignment.Match(contents) || providerSecretAssignment.Match(contents)
}

func unsafeProviderString(value string) bool {
	for _, character := range value {
		if character < 0x20 {
			return true
		}
	}
	return privateKeyLike(value) || bearerValue.MatchString(value) || secretAssignment.MatchString(value) || secretEnvAssignment.MatchString(value) || providerSecretAssignment.MatchString(value)
}

func privateKeyLike(value string) bool {
	upper := strings.ToUpper(value)
	return strings.Contains(upper, "-----BEGIN ") && strings.Contains(upper, "PRIVATE KEY-----")
}

func validateUnderstandingOperation(operation UnderstandingOperation) error {
	if operation.ID == "" || operation.Status == "" || operation.Response.ContractVersion != UnderstandingContractV1 || operation.Response.OperationID != operation.ID || operation.Response.Role != operation.Role || operation.Response.ExactCommitSHA != operation.ExpectedCommitSHA || operation.Response.ResultDigest != operation.ResultDigest {
		return ErrProviderOutput
	}
	digest, err := digestAnalysisResponse(operation.Response)
	if err != nil || digest != operation.ResultDigest {
		return ErrProviderOutput
	}
	return nil
}

func analysisPrompt(role AnalysisRole) string {
	return fmt.Sprintf("You are the %s repository-understanding role. Use only files in the disposable evidence workspace. Return one JSON object matching the required schema. Do not use outside knowledge. Mark each statement exactly observed, declared, inferred, unknown, or conflicting and cite only supplied evidence paths.", role)
}

func structuredOutput(response AnalysisResponse) StructuredOutput {
	return StructuredOutput{
		Role: response.Role, Findings: response.Findings, Capabilities: response.Capabilities,
		Assumptions: response.Assumptions, Conflicts: response.Conflicts, Unknowns: response.Unknowns,
	}
}

func newUnderstandingID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("repository-understanding-%d", time.Now().UnixNano())
	}
	return "repository-understanding-" + base64.RawURLEncoding.EncodeToString(value)
}
