package advisory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/berkayahi/agentbridge/internal/security"
)

type Config struct {
	Catalog   ProviderCatalog
	Providers map[string]Provider
	Clock     func() time.Time
	NewID     func(string) string
}

type Service struct {
	catalog   ProviderCatalog
	providers map[string]Provider
	clock     func() time.Time
	newID     func(string) string
	redactor  *security.Redactor
}

func New(config Config) (*Service, error) {
	if config.Catalog == nil || len(config.Providers) == 0 {
		return nil, ErrInvalidRequest
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.NewID == nil {
		config.NewID = func(prefix string) string {
			sum := sha256.Sum256([]byte(prefix + "\x00" + config.Clock().UTC().Format(time.RFC3339Nano)))
			return prefix + "-" + hex.EncodeToString(sum[:8])
		}
	}
	providers := make(map[string]Provider, len(config.Providers))
	for id, provider := range config.Providers {
		if strings.TrimSpace(id) == "" || provider == nil {
			return nil, ErrInvalidRequest
		}
		providers[id] = provider
	}
	return &Service{
		catalog: config.Catalog, providers: providers, clock: config.Clock, newID: config.NewID,
		redactor: security.NewRedactor(security.Config{MaxFieldRunes: MaxContextValueBytes, MaxPayloadRunes: MaxOutputBytes}),
	}, nil
}

func (s *Service) ExecuteAdvisorySession(ctx context.Context, request SessionRequest) (SessionResponse, error) {
	if s == nil || s.catalog == nil {
		return SessionResponse{}, ErrNotConfigured
	}
	if err := ValidateSessionRequest(request); err != nil {
		return SessionResponse{}, err
	}
	contextBundle := redactContext(s.redactor, request.Context)
	prompt := s.redactor.RedactString(request.Prompt)
	schema := append([]byte(nil), request.OutputSchema...)
	policy := effectivePolicy()
	schemaDigest := digestBytes(schema)
	policyDigest := digestPolicy(policy, request.WebResearch)

	profiles, err := s.catalog.ProviderProfiles(ctx)
	if err != nil {
		return SessionResponse{}, err
	}
	profile, provider, err := s.selectProvider(profiles, request.ProviderID, request.ModelID, request.WebResearch.Enabled)
	if err != nil {
		return SessionResponse{}, err
	}
	capability := provider.Capability()
	if err := validateCapability(capability, request.WebResearch.Enabled); err != nil {
		return SessionResponse{}, err
	}
	modelID := strings.TrimSpace(request.ModelID)
	if modelID == "" {
		modelID = strings.TrimSpace(profile.ModelID)
	}
	if modelID == "" && len(profile.ModelIDs) > 0 {
		modelID = strings.TrimSpace(profile.ModelIDs[0])
	}
	if modelID == "" {
		return SessionResponse{}, ErrNotConfigured
	}
	started := s.clock().UTC()
	sessionID := s.newID("execution-session")
	executionRequest := ExecutionRequest{
		ContractVersion: ContractVersion, ExecutionSessionID: sessionID,
		ProviderID: profile.ID, ModelID: modelID, Prompt: prompt, Context: contextBundle,
		OutputSchema: schema, SchemaDigest: schemaDigest, SchemaVersion: request.SchemaVersion,
		Policy: policy, PolicyDigest: policyDigest,
		WebResearch: request.WebResearch,
	}
	result, err := provider.Execute(ctx, executionRequest)
	if err != nil {
		return SessionResponse{}, err
	}
	if result.ProviderID != "" && result.ProviderID != profile.ID {
		return SessionResponse{}, ErrProviderIdentity
	}
	if result.ModelID != "" && result.ModelID != modelID {
		return SessionResponse{}, ErrProviderIdentity
	}
	if len(result.Output) == 0 || len(result.Output) > MaxOutputBytes {
		return SessionResponse{}, ErrProviderOutputBounds
	}
	output, err := sanitizeStructuredOutput(s.redactor, result.Output)
	if err != nil {
		return SessionResponse{}, err
	}
	value, err := decodeJSON(output)
	if err != nil {
		return SessionResponse{}, fmt.Errorf("%w: malformed JSON", ErrStructuredOutput)
	}
	if err := validateOutput(schema, value, "$", 0); err != nil {
		if errors.Is(err, ErrPolicyViolation) {
			return SessionResponse{}, err
		}
		return SessionResponse{}, fmt.Errorf("%w: %v", ErrStructuredOutput, err)
	}
	completed := s.clock().UTC()
	receipt := ExecutionReceipt{
		ReceiptID: s.newID("execution-receipt"), ExecutionSessionID: sessionID,
		ProviderID: profile.ID, ModelID: modelID, ContextDigest: digestJSON(contextBundle),
		PromptDigest: digestString(prompt), SchemaDigest: schemaDigest, PolicyDigest: policyDigest,
		OutputDigest:  digestBytes(output),
		SchemaVersion: request.SchemaVersion, ContractVersion: ContractVersion,
		StartedAt: started, CompletedAt: completed, Status: "completed",
	}
	return SessionResponse{ContractVersion: ContractVersion, Output: append(json.RawMessage(nil), output...), Receipt: receipt}, nil
}

func (s *Service) selectProvider(profiles []ProviderProfile, requested, model string, webResearch bool) (ProviderProfile, Provider, error) {
	requested = strings.TrimSpace(requested)
	model = strings.TrimSpace(model)
	for _, profile := range profiles {
		if !profile.Available || !capabilityEligible(profile.Capability, webResearch) || (requested != "" && profile.ID != requested) || (model != "" && !profileSupportsModel(profile, model)) {
			continue
		}
		provider, ok := s.providers[profile.ID]
		if !ok || provider == nil || (provider.Capability().ID != "" && provider.Capability().ID != profile.ID) || !capabilityEligible(provider.Capability(), webResearch) {
			continue
		}
		return profile, provider, nil
	}
	if requested != "" {
		return ProviderProfile{}, nil, fmt.Errorf("provider %q: %w", requested, ErrNotConfigured)
	}
	return ProviderProfile{}, nil, ErrNotConfigured
}

func profileSupportsModel(profile ProviderProfile, model string) bool {
	if model == "" {
		return true
	}
	if profile.ModelID == model {
		return true
	}
	for _, value := range profile.ModelIDs {
		if strings.TrimSpace(value) == model {
			return true
		}
	}
	for _, value := range profile.ModelAliases {
		if strings.TrimSpace(value) == model {
			return true
		}
	}
	return false
}

func capabilityEligible(capability ProviderCapability, webResearch bool) bool {
	return !webResearch && capability.AdvisorySessions && capability.ReadOnly && capability.StructuredOutput &&
		!capability.RepositoryWrites && !capability.BranchMutation && !capability.WorktreeMutation &&
		!capability.GitIntegration && !capability.SecretValueAccess && !capability.DecisionMutation &&
		!capability.HumanApproval
}

// CapabilityEligible reports whether a provider can be used by an advisory
// session. Web research is intentionally not eligible because this boundary
// has no web adapter to enforce bounded source and byte limits.
func CapabilityEligible(capability ProviderCapability, webResearch bool) bool {
	return capabilityEligible(capability, webResearch)
}

func validateCapability(capability ProviderCapability, webResearch bool) error {
	if !capabilityEligible(capability, webResearch) {
		return ErrPolicyViolation
	}
	return nil
}

// ValidateSessionRequest checks the complete request boundary without
// redacting or invoking a provider. Local-control uses this same preflight so
// an invalid request cannot be replayed, persisted, or handed to an authority
// implementation that omits its own validation.
func ValidateSessionRequest(request SessionRequest) error {
	if err := validateRequest(request); err != nil {
		return err
	}
	return validateSchema(request.OutputSchema)
}

func validateRequest(request SessionRequest) error {
	if !validID(request.IdempotencyKey) || strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > MaxPromptBytes || !utf8.ValidString(request.Prompt) {
		return ErrInvalidRequest
	}
	if strings.TrimSpace(request.SchemaVersion) == "" || len(request.SchemaVersion) > MaxSchemaVersionBytes || !utf8.ValidString(request.SchemaVersion) {
		return ErrInvalidRequest
	}
	if len(request.OutputSchema) == 0 || len(request.OutputSchema) > MaxSchemaBytes {
		return ErrInvalidRequest
	}
	if err := validateWebResearch(request.WebResearch); err != nil {
		return err
	}
	if len(request.Context.Items) > MaxContextItems {
		return ErrInvalidRequest
	}
	total := 0
	for index, item := range request.Context.Items {
		if strings.TrimSpace(item.Key) == "" || len(item.Key) > MaxContextKeyBytes || len(item.Value) > MaxContextValueBytes || !utf8.ValidString(item.Key) || !utf8.ValidString(item.Value) {
			return ErrInvalidRequest
		}
		if sensitiveKey(item.Key) {
			return fmt.Errorf("context key %q: %w", item.Key, ErrPolicyViolation)
		}
		total += len(item.Key) + len(item.Value)
		if total > MaxContextTotalBytes {
			return ErrInvalidRequest
		}
		if err := validateContextValue(item.Value, fmt.Sprintf("$.context.items[%d].value", index)); err != nil {
			return err
		}
	}
	return nil
}

func validateContextValue(value, path string) error {
	decoded, err := decodeJSON([]byte(value))
	if err != nil {
		return nil
	}
	switch decoded.(type) {
	case map[string]any, []any:
		// Structured context is still allowed when it is generic and safe, but
		// sensitive nested fields are rejected rather than redacted and sent on.
		return rejectSecretKeys(decoded, path)
	default:
		return nil
	}
}

func redactContext(redactor *security.Redactor, bundle ContextBundle) ContextBundle {
	items := make([]ContextItem, len(bundle.Items))
	for index, item := range bundle.Items {
		items[index] = ContextItem{Key: item.Key, Value: redactor.RedactString(item.Value)}
	}
	return ContextBundle{Items: items}
}

// SanitizeSessionResponse applies the advisory output boundary before a
// response is handed to a durability or transport adapter. It is deliberately
// independent of a request schema so local-control replay cannot return a
// secret-shaped field even when an authority implementation is misconfigured.
func SanitizeSessionResponse(response SessionResponse) (SessionResponse, error) {
	output, err := sanitizeStructuredOutput(nil, response.Output)
	if err != nil {
		return SessionResponse{}, err
	}
	response.Output = output
	response.Receipt.OutputDigest = digestBytes(output)
	return response, nil
}

func sanitizeStructuredOutput(redactor *security.Redactor, raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaxOutputBytes {
		return nil, ErrProviderOutputBounds
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed JSON", ErrStructuredOutput)
	}
	if err := rejectSecretKeys(value, "$"); err != nil {
		return nil, err
	}
	if redactor == nil {
		redactor = security.NewRedactor(security.Config{MaxFieldRunes: MaxContextValueBytes, MaxPayloadRunes: MaxOutputBytes})
	}
	output := redactor.RedactBytes(raw)
	if len(output) == 0 || len(output) > MaxOutputBytes {
		return nil, ErrProviderOutputBounds
	}
	redactedValue, err := decodeJSON(output)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed redacted JSON", ErrStructuredOutput)
	}
	if err := rejectSecretKeys(redactedValue, "$"); err != nil {
		return nil, err
	}
	return append([]byte(nil), output...), nil
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	for _, word := range strings.FieldsFunc(normalized, func(r rune) bool {
		return r == '_' || r == '-' || r == ' ' || r == '.'
	}) {
		if word == "secret" || word == "password" || word == "token" || word == "credential" || word == "private" || word == "authorization" || word == "cookie" || word == "apikey" || word == "key" {
			return true
		}
	}
	compact := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, normalized)
	return strings.Contains(compact, "secret") || strings.Contains(compact, "password") ||
		strings.Contains(compact, "token") || strings.Contains(compact, "credential") ||
		strings.Contains(compact, "authorization") || strings.Contains(compact, "cookie") ||
		strings.Contains(compact, "apikey") || compact == "privatekey"
}

func validateWebResearch(policy WebResearchPolicy) error {
	if policy.MaxSources < 0 || policy.MaxSources > 32 || policy.MaxBytes < 0 || policy.MaxBytes > 4<<20 {
		return ErrInvalidRequest
	}
	if !policy.Enabled {
		if policy.MaxSources != 0 || policy.MaxBytes != 0 {
			return ErrInvalidRequest
		}
		return nil
	}
	if policy.MaxSources == 0 || policy.MaxBytes == 0 {
		return ErrInvalidRequest
	}
	return ErrPolicyViolation
}

func effectivePolicy() ExecutionPolicy {
	return ExecutionPolicy{ReadOnly: true}
}

func digestPolicy(policy ExecutionPolicy, web WebResearchPolicy) string {
	return digestJSON(struct {
		Policy      ExecutionPolicy   `json:"policy"`
		WebResearch WebResearchPolicy `json:"web_research"`
	}{Policy: policy, WebResearch: web})
}

func validID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func decodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return value, nil
}

func digestString(value string) string { return digestBytes([]byte(value)) }

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func digestJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return digestBytes(encoded)
}
