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
	// Redactor is retained in memory only and is never included in an advisory
	// request, response, receipt, or durable record. When set, it lets this
	// boundary remove the same configured secret material used by the daemon's
	// other safe-output surfaces.
	Redactor *security.Redactor
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
	redactor := config.Redactor
	if redactor == nil {
		redactor = newAdvisoryRedactor()
	}
	return &Service{
		catalog: config.Catalog, providers: providers, clock: config.Clock, newID: config.NewID,
		redactor: redactor,
	}, nil
}

func (s *Service) ExecuteAdvisorySession(ctx context.Context, request SessionRequest) (SessionResponse, error) {
	if s == nil || s.catalog == nil {
		return SessionResponse{}, ErrNotConfigured
	}
	if err := ValidateSessionRequestWithRedactor(request, s.redactor); err != nil {
		return SessionResponse{}, err
	}
	contextBundle, err := redactContext(s.redactor, request.Context)
	if err != nil {
		return SessionResponse{}, err
	}
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
	response := SessionResponse{ContractVersion: ContractVersion, Output: append(json.RawMessage(nil), output...), Receipt: receipt}
	if err := validateSessionResponse(request, response, s.redactor); err != nil {
		return SessionResponse{}, err
	}
	return response, nil
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
	return ValidateSessionRequestWithRedactor(request, nil)
}

// ValidateSessionRequestWithRedactor is the request preflight used by an
// authority that already owns the process-wide safe-output redactor. The
// redactor is an in-memory dependency; its configured values never cross this
// function's boundary.
func ValidateSessionRequestWithRedactor(request SessionRequest, redactor *security.Redactor) error {
	return validateSessionRequest(request, redactor)
}

func validateSessionRequest(request SessionRequest, redactor *security.Redactor) error {
	if err := validateRequest(request); err != nil {
		return err
	}
	return validateSchema(request.OutputSchema, redactor)
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
		if errors.Is(err, errDuplicateJSONKey) {
			return fmt.Errorf("%w: context contains duplicate JSON keys", ErrInvalidRequest)
		}
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

func redactContext(redactor *security.Redactor, bundle ContextBundle) (ContextBundle, error) {
	if redactor == nil {
		redactor = newAdvisoryRedactor()
	}
	items := make([]ContextItem, len(bundle.Items))
	for index, item := range bundle.Items {
		value, err := redactContextValue(redactor, item.Value, fmt.Sprintf("$.context.items[%d].value", index))
		if err != nil {
			return ContextBundle{}, err
		}
		items[index] = ContextItem{Key: item.Key, Value: value}
	}
	return ContextBundle{Items: items}, nil
}

func redactContextValue(redactor *security.Redactor, raw, path string) (string, error) {
	decoded, err := decodeJSON([]byte(raw))
	if err != nil {
		if errors.Is(err, errDuplicateJSONKey) {
			return "", fmt.Errorf("%w: context contains duplicate JSON keys", ErrInvalidRequest)
		}
		return redactContextScalar(redactor, raw, path)
	}
	changed, err := redactJSONValue(redactor, &decoded, path)
	if err != nil {
		return "", err
	}
	if !changed {
		return raw, nil
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "", fmt.Errorf("%w: encode redacted context", ErrPolicyViolation)
	}
	return string(encoded), nil
}

func redactJSONValue(redactor *security.Redactor, value *any, path string) (bool, error) {
	switch current := (*value).(type) {
	case map[string]any:
		changed := false
		for name, child := range current {
			if sensitiveKey(name) {
				return false, fmt.Errorf("%w: JSON %s contains secret-shaped key %q", ErrPolicyViolation, path, name)
			}
			childChanged, err := redactJSONValue(redactor, &child, path+"."+name)
			if err != nil {
				return false, err
			}
			if childChanged {
				current[name] = child
				changed = true
			}
		}
		return changed, nil
	case []any:
		changed := false
		for index := range current {
			childChanged, err := redactJSONValue(redactor, &current[index], fmt.Sprintf("%s[%d]", path, index))
			if err != nil {
				return false, err
			}
			changed = changed || childChanged
		}
		return changed, nil
	case string:
		redacted, err := redactContextScalar(redactor, current, path)
		if err != nil {
			return false, err
		}
		if redacted == current {
			return false, nil
		}
		*value = redacted
		return true, nil
	default:
		return false, nil
	}
}

func redactContextScalar(redactor *security.Redactor, value, path string) (string, error) {
	redacted := redactor.RedactString(value)
	if secretLikeText(value, redactor) && !strings.Contains(redacted, "[REDACTED:") {
		return "", fmt.Errorf("%w: JSON %s contains unredactable secret-like text", ErrPolicyViolation, path)
	}
	return redacted, nil
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
	return response, nil
}

// ValidateSessionResponse checks a receipt and its output against the request.
// It is safe to use for cached responses: it never repairs a digest or returns
// a response whose receipt does not bind to the supplied request.
func ValidateSessionResponse(request SessionRequest, response SessionResponse) error {
	return ValidateSessionResponseWithRedactor(request, response, nil)
}

// ValidateSessionResponseWithRedactor uses the same in-memory redactor that
// produced the provider-bound prompt and context. Secret material is used only
// to derive safe digests and is never returned or persisted.
func ValidateSessionResponseWithRedactor(request SessionRequest, response SessionResponse, redactor *security.Redactor) error {
	return validateSessionResponse(request, response, redactor)
}

func validateSessionResponse(request SessionRequest, response SessionResponse, redactor *security.Redactor) error {
	if err := validateSessionRequest(request, redactor); err != nil {
		return err
	}
	if redactor == nil {
		redactor = newAdvisoryRedactor()
	}
	receipt := response.Receipt
	if response.ContractVersion != ContractVersion || receipt.ContractVersion != ContractVersion {
		return fmt.Errorf("%w: contract version mismatch", ErrReceiptIntegrity)
	}
	if receipt.Status != "completed" {
		return fmt.Errorf("%w: receipt status is not completed", ErrReceiptIntegrity)
	}
	for name, value := range map[string]string{
		"receipt id": receipt.ReceiptID, "execution session id": receipt.ExecutionSessionID,
		"provider id": receipt.ProviderID, "model id": receipt.ModelID,
		"context digest": receipt.ContextDigest, "prompt digest": receipt.PromptDigest,
		"schema digest": receipt.SchemaDigest, "policy digest": receipt.PolicyDigest,
		"output digest": receipt.OutputDigest, "schema version": receipt.SchemaVersion,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: receipt %s is empty", ErrReceiptIntegrity, name)
		}
	}
	if receipt.SchemaVersion != request.SchemaVersion {
		return fmt.Errorf("%w: schema version mismatch", ErrReceiptIntegrity)
	}
	if requested := strings.TrimSpace(request.ProviderID); requested != "" && receipt.ProviderID != requested {
		return fmt.Errorf("%w: provider mismatch", ErrReceiptIntegrity)
	}
	if requested := strings.TrimSpace(request.ModelID); requested != "" && receipt.ModelID != requested {
		return fmt.Errorf("%w: model mismatch", ErrReceiptIntegrity)
	}
	if receipt.StartedAt.IsZero() || receipt.CompletedAt.IsZero() || receipt.CompletedAt.Before(receipt.StartedAt) {
		return fmt.Errorf("%w: receipt timestamps are invalid", ErrReceiptIntegrity)
	}

	contextBundle, err := redactContext(redactor, request.Context)
	if err != nil {
		return err
	}
	checks := map[string]string{
		"context digest": digestJSON(contextBundle),
		"prompt digest":  digestString(redactor.RedactString(request.Prompt)),
		"schema digest":  digestBytes(request.OutputSchema),
		"policy digest":  digestPolicy(effectivePolicy(), request.WebResearch),
		"output digest":  digestBytes(response.Output),
	}
	actual := map[string]string{
		"context digest": receipt.ContextDigest, "prompt digest": receipt.PromptDigest,
		"schema digest": receipt.SchemaDigest, "policy digest": receipt.PolicyDigest,
		"output digest": receipt.OutputDigest,
	}
	for name, expected := range checks {
		if actual[name] != expected {
			return fmt.Errorf("%w: %s mismatch", ErrReceiptIntegrity, name)
		}
	}

	safeOutput, err := sanitizeStructuredOutput(redactor, response.Output)
	if err != nil {
		return err
	}
	if !bytes.Equal(safeOutput, response.Output) {
		return fmt.Errorf("%w: output is not sanitized", ErrReceiptIntegrity)
	}
	value, err := decodeJSON(response.Output)
	if err != nil {
		return fmt.Errorf("%w: output is malformed", ErrReceiptIntegrity)
	}
	if err := validateOutput(request.OutputSchema, value, "$", 0); err != nil {
		return fmt.Errorf("%w: output does not match the request schema: %v", ErrReceiptIntegrity, err)
	}
	return nil
}

func sanitizeStructuredOutput(redactor *security.Redactor, raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaxOutputBytes {
		return nil, ErrProviderOutputBounds
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed JSON", ErrStructuredOutput)
	}
	if redactor == nil {
		redactor = newAdvisoryRedactor()
	}
	changed, err := redactJSONValue(redactor, &value, "$")
	if err != nil {
		return nil, err
	}
	output := raw
	if changed {
		output, err = json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("%w: encode redacted JSON", ErrStructuredOutput)
		}
	}
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

func newAdvisoryRedactor() *security.Redactor {
	return security.NewRedactor(security.Config{MaxFieldRunes: MaxContextValueBytes, MaxPayloadRunes: MaxOutputBytes})
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

var errDuplicateJSONKey = errors.New("advisory: duplicate JSON object key")

func decodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeJSONValue(decoder, "$")
	if err != nil {
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

func decodeJSONValue(decoder *json.Decoder, path string) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return nil, fmt.Errorf("%w: %s.%q", errDuplicateJSONKey, path, key)
			}
			seen[key] = struct{}{}
			child, err := decodeJSONValue(decoder, path+"."+key)
			if err != nil {
				return nil, err
			}
			object[key] = child
		}
		end, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if end != json.Delim('}') {
			return nil, errors.New("object is not closed")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for index := 0; decoder.More(); index++ {
			child, err := decodeJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index))
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		end, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if end != json.Delim(']') {
			return nil, errors.New("array is not closed")
		}
		return array, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
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
