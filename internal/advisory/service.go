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
	if err := validateRequest(request); err != nil {
		return SessionResponse{}, err
	}
	contextBundle := redactContext(s.redactor, request.Context)
	prompt := s.redactor.RedactString(request.Prompt)
	schema := append([]byte(nil), request.OutputSchema...)
	if err := validateSchema(schema); err != nil {
		return SessionResponse{}, err
	}

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
	if modelID == "" {
		modelID = "provider-default"
	}
	started := s.clock().UTC()
	sessionID := s.newID("execution-session")
	executionRequest := ExecutionRequest{
		ContractVersion: ContractVersion, ExecutionSessionID: sessionID,
		ProviderID: profile.ID, ModelID: modelID, Prompt: prompt, Context: contextBundle,
		OutputSchema: schema, SchemaVersion: request.SchemaVersion,
		Policy:      ExecutionPolicy{ReadOnly: true, WebResearchAllowed: request.WebResearch.Enabled},
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
	output := s.redactor.RedactBytes(result.Output)
	if len(output) == 0 || len(output) > MaxOutputBytes {
		return SessionResponse{}, ErrProviderOutputBounds
	}
	value, err := decodeJSON(output)
	if err != nil {
		return SessionResponse{}, fmt.Errorf("%w: malformed JSON", ErrStructuredOutput)
	}
	if err := validateOutput(schema, value, "$", 0); err != nil {
		return SessionResponse{}, fmt.Errorf("%w: %v", ErrStructuredOutput, err)
	}
	completed := s.clock().UTC()
	receipt := ExecutionReceipt{
		ReceiptID: s.newID("execution-receipt"), ExecutionSessionID: sessionID,
		ProviderID: profile.ID, ModelID: modelID, ContextDigest: digestJSON(contextBundle),
		PromptDigest: digestString(prompt), OutputDigest: digestBytes(output),
		SchemaVersion: request.SchemaVersion, ContractVersion: ContractVersion,
		StartedAt: started, CompletedAt: completed, Status: "completed",
	}
	return SessionResponse{ContractVersion: ContractVersion, Output: append(json.RawMessage(nil), output...), Receipt: receipt}, nil
}

func (s *Service) selectProvider(profiles []ProviderProfile, requested, model string, webResearch bool) (ProviderProfile, Provider, error) {
	requested = strings.TrimSpace(requested)
	model = strings.TrimSpace(model)
	for _, profile := range profiles {
		if !profile.Available || !capabilityEligible(profile.Capability, webResearch) || (requested != "" && profile.ID != requested) || (model != "" && profile.ModelID != "" && profile.ModelID != model) {
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

func capabilityEligible(capability ProviderCapability, webResearch bool) bool {
	return capability.AdvisorySessions && capability.ReadOnly && capability.StructuredOutput &&
		!capability.RepositoryWrites && !capability.BranchMutation && !capability.WorktreeMutation &&
		!capability.GitIntegration && !capability.SecretValueAccess && !capability.DecisionMutation &&
		!capability.HumanApproval && (!webResearch || capability.WebResearch)
}

func validateCapability(capability ProviderCapability, webResearch bool) error {
	if !capabilityEligible(capability, webResearch) {
		return ErrPolicyViolation
	}
	return nil
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
	if request.WebResearch.MaxSources < 0 || request.WebResearch.MaxSources > 32 || request.WebResearch.MaxBytes < 0 || request.WebResearch.MaxBytes > 4<<20 {
		return ErrInvalidRequest
	}
	if len(request.Context.Items) > MaxContextItems {
		return ErrInvalidRequest
	}
	total := 0
	for _, item := range request.Context.Items {
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
	}
	return nil
}

func redactContext(redactor *security.Redactor, bundle ContextBundle) ContextBundle {
	items := make([]ContextItem, len(bundle.Items))
	for index, item := range bundle.Items {
		items[index] = ContextItem{Key: item.Key, Value: redactor.RedactString(item.Value)}
	}
	return ContextBundle{Items: items}
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", " ", "-", " ").Replace(strings.TrimSpace(key)))
	for _, word := range strings.Fields(normalized) {
		if word == "secret" || word == "password" || word == "token" || word == "credential" || word == "private" || word == "authorization" || word == "cookie" || word == "apikey" || word == "key" {
			return true
		}
	}
	return false
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
