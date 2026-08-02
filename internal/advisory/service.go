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
	safety    *SafetyPipeline
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
		redactor: redactor, safety: NewSafetyPipeline(redactor, SafetyConfig{MaxInputBytes: MaxContextTotalBytes}),
	}, nil
}

func (s *Service) ExecuteAdvisorySession(ctx context.Context, request SessionRequest) (SessionResponse, error) {
	if s == nil || s.catalog == nil {
		return SessionResponse{}, ErrNotConfigured
	}
	prepared, err := prepareSessionRequest(request, s.safety)
	if err != nil {
		return SessionResponse{}, err
	}
	contextBundle := prepared.Context
	prompt := prepared.Prompt
	schema := append([]byte(nil), prepared.OutputSchema...)
	policy := effectivePolicy()
	schemaDigest := digestBytes(schema)
	policyDigest := digestPolicy(policy, prepared.WebResearch)

	profiles, err := s.catalog.ProviderProfiles(ctx)
	if err != nil {
		return SessionResponse{}, err
	}
	profile, provider, err := s.selectProvider(profiles, prepared.ProviderID, prepared.ModelID, prepared.WebResearch.Enabled)
	if err != nil {
		return SessionResponse{}, err
	}
	capability := provider.Capability()
	if err := validateCapability(capability, prepared.WebResearch.Enabled); err != nil {
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
		WebResearch: prepared.WebResearch,
	}
	result, err := provider.Execute(ctx, executionRequest)
	if err != nil {
		return SessionResponse{}, safeProviderExecutionError(s.safety, err)
	}
	if err := inspectProviderDiagnostics(s.safety, result.Stdout, result.Stderr); err != nil {
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
	output, err := sanitizeStructuredOutput(s.safety, result.Output)
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
	if err := validateSessionResponse(prepared, response, s.safety); err != nil {
		return SessionResponse{}, err
	}
	return response, nil
}

func (s *Service) selectProvider(profiles []ProviderProfile, requested, model string, webResearch bool) (ProviderProfile, Provider, error) {
	requested = strings.TrimSpace(requested)
	model = strings.TrimSpace(model)
	for _, profile := range profiles {
		if !profile.Available || profile.Capabilities.ProviderID != profile.ID || !capabilityEligible(profile.Capabilities, webResearch) || (requested != "" && profile.ID != requested) || (model != "" && !profileSupportsModel(profile, model)) {
			continue
		}
		provider, ok := s.providers[profile.ID]
		if !ok || provider == nil || provider.Capability().ProviderID != profile.ID || !sameCapability(profile.Capabilities, provider.Capability()) || !capabilityEligible(provider.Capability(), webResearch) {
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
	return capability.AdvisoryEligible(webResearch)
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

func sameCapability(left, right ProviderCapability) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
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
	return validateSessionRequest(request, NewSafetyPipeline(redactor, SafetyConfig{MaxInputBytes: MaxContextTotalBytes}))
}

// SanitizeSessionRequestWithRedactor returns the provider-safe request copy.
// Prompt and context values may be redacted in memory; schema and structural
// violations are rejected. Callers that persist or dispatch a request must use
// this returned copy rather than retaining the caller-owned original.
func SanitizeSessionRequestWithRedactor(request SessionRequest, redactor *security.Redactor) (SessionRequest, error) {
	return prepareSessionRequest(request, NewSafetyPipeline(redactor, SafetyConfig{MaxInputBytes: MaxContextTotalBytes}))
}

func validateSessionRequest(request SessionRequest, safety *SafetyPipeline) error {
	_, err := prepareSessionRequest(request, safety)
	return err
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
	for _, item := range request.Context.Items {
		if strings.TrimSpace(item.Key) == "" || len(item.Key) > MaxContextKeyBytes || len(item.Value) > MaxContextValueBytes || !utf8.ValidString(item.Key) || !utf8.ValidString(item.Value) {
			return ErrInvalidRequest
		}
		total += len(item.Key) + len(item.Value)
		if total > MaxContextTotalBytes {
			return ErrInvalidRequest
		}
	}
	return nil
}

func prepareSessionRequest(request SessionRequest, safety *SafetyPipeline) (SessionRequest, error) {
	if err := validateRequest(request); err != nil {
		return SessionRequest{}, err
	}
	if safety == nil {
		safety = NewSafetyPipeline(nil, SafetyConfig{MaxInputBytes: MaxContextTotalBytes})
	}
	for _, value := range []string{request.ProviderID, request.ModelID, request.SchemaVersion, request.IdempotencyKey} {
		if value == "" {
			continue
		}
		if _, err := safety.SanitizeText(value, 0); err != nil {
			return SessionRequest{}, mapSafetyInputError(err)
		}
	}
	if err := validateSchema(request.OutputSchema, safety); err != nil {
		return SessionRequest{}, err
	}
	prompt, err := safety.SanitizeText(request.Prompt, 0)
	if err != nil {
		return SessionRequest{}, mapSafetyInputError(err)
	}
	items := make([]ContextItem, len(request.Context.Items))
	total := 0
	for index, item := range request.Context.Items {
		if safetyKey(item.Key) {
			return SessionRequest{}, ErrPolicyViolation
		}
		if _, err := safety.SanitizeText(item.Key, 0); err != nil {
			return SessionRequest{}, mapSafetyInputError(err)
		}
		value, err := safety.SanitizeText(item.Value, 0)
		if err != nil {
			return SessionRequest{}, mapSafetyInputError(err)
		}
		if len(value) > MaxContextValueBytes {
			return SessionRequest{}, ErrInvalidRequest
		}
		total += len(item.Key) + len(value)
		if total > MaxContextTotalBytes {
			return SessionRequest{}, ErrInvalidRequest
		}
		items[index] = ContextItem{Key: item.Key, Value: value}
	}
	request.Prompt = prompt
	request.Context = ContextBundle{Items: items}
	return request, nil
}

// SanitizeSessionResponse applies the advisory output boundary before a
// response is handed to a durability or transport adapter. It is deliberately
// independent of a request schema so local-control replay cannot return a
// secret-shaped field even when an authority implementation is misconfigured.
func SanitizeSessionResponse(response SessionResponse) (SessionResponse, error) {
	safety := NewSafetyPipeline(newAdvisoryRedactor(), SafetyConfig{MaxInputBytes: MaxOutputBytes})
	output, err := sanitizeStructuredOutput(safety, response.Output)
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
	return validateSessionResponse(request, response, NewSafetyPipeline(redactor, SafetyConfig{MaxInputBytes: MaxContextTotalBytes}))
}

func validateSessionResponse(request SessionRequest, response SessionResponse, safety *SafetyPipeline) error {
	prepared, err := prepareSessionRequest(request, safety)
	if err != nil {
		return err
	}
	request = prepared
	if safety == nil {
		safety = NewSafetyPipeline(newAdvisoryRedactor(), SafetyConfig{MaxInputBytes: MaxContextTotalBytes})
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
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("%w: receipt cannot be encoded", ErrReceiptIntegrity)
	}
	if err := safety.InspectJSON(receiptJSON); err != nil {
		return fmt.Errorf("%w: receipt safety validation failed", ErrReceiptIntegrity)
	}
	safeOutput, err := sanitizeStructuredOutput(safety, response.Output)
	if err != nil {
		return err
	}
	if !bytes.Equal(safeOutput, response.Output) {
		return fmt.Errorf("%w: output is not sanitized", ErrReceiptIntegrity)
	}
	checks := map[string]string{
		"context digest": digestJSON(request.Context),
		"prompt digest":  digestString(request.Prompt),
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
	value, err := decodeJSON(response.Output)
	if err != nil {
		return fmt.Errorf("%w: output is malformed", ErrReceiptIntegrity)
	}
	if err := validateOutput(request.OutputSchema, value, "$", 0); err != nil {
		return fmt.Errorf("%w: output does not match the request schema: %v", ErrReceiptIntegrity, err)
	}
	return nil
}

func sanitizeStructuredOutput(safety *SafetyPipeline, raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaxOutputBytes {
		return nil, ErrProviderOutputBounds
	}
	if safety == nil {
		safety = NewSafetyPipeline(newAdvisoryRedactor(), SafetyConfig{MaxInputBytes: MaxOutputBytes})
	}
	output, err := safety.SanitizeJSON(raw)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnsafeContent):
			return nil, ErrPolicyViolation
		case errors.Is(err, ErrSafetyBounds):
			return nil, ErrProviderOutputBounds
		case errors.Is(err, ErrMalformedPayload):
			return nil, fmt.Errorf("%w: malformed JSON", ErrStructuredOutput)
		default:
			return nil, fmt.Errorf("%w: output safety validation failed", ErrStructuredOutput)
		}
	}
	if len(output) == 0 || len(output) > MaxOutputBytes {
		return nil, ErrProviderOutputBounds
	}
	return append([]byte(nil), output...), nil
}

func mapSafetyInputError(err error) error {
	switch {
	case errors.Is(err, ErrUnsafeContent):
		return ErrPolicyViolation
	case errors.Is(err, ErrMalformedPayload), errors.Is(err, ErrSafetyBounds):
		return ErrInvalidRequest
	default:
		return err
	}
}

func inspectProviderDiagnostics(safety *SafetyPipeline, stdout, stderr []byte) error {
	for _, diagnostic := range [][]byte{stdout, stderr} {
		if len(diagnostic) == 0 {
			continue
		}
		if len(diagnostic) > MaxOutputBytes {
			return ErrProviderOutputBounds
		}
		if _, err := safety.SanitizeText(string(diagnostic), 0); err != nil {
			return ErrPolicyViolation
		}
	}
	return nil
}

func safeProviderExecutionError(safety *SafetyPipeline, err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrInvalidRequest, ErrPolicyViolation, ErrStructuredOutput, ErrProviderIdentity, ErrProviderOutputBounds, ErrReceiptIntegrity, ErrProviderExecution} {
		if errors.Is(err, known) {
			return known
		}
	}
	if safety != nil {
		_, _ = safety.SanitizeText(err.Error(), 0)
	}
	return ErrProviderExecution
}

func newAdvisoryRedactor() *security.Redactor {
	return security.NewRedactor(security.Config{MaxFieldRunes: MaxContextValueBytes, MaxPayloadRunes: MaxOutputBytes})
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
