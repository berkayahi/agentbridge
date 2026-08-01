package advisory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DeterministicProvider is a process-local adapter for acceptance and
// development. It has no process, filesystem, network, credential, or mutable
// state dependency. A caller may provide a fixed JSON value for tests; the
// composed fixture instead derives a bounded value from the requested schema.
type DeterministicProvider struct {
	ProviderID string
	ModelID    string
	Output     []byte
}

func (p DeterministicProvider) Capability() ProviderCapability {
	return ProviderCapability{
		ID: p.ProviderID, AdvisorySessions: true, ReadOnly: true, StructuredOutput: true,
		WebResearch: false,
	}
}

func (p DeterministicProvider) ConfiguredModel() string { return p.ModelID }

func (p DeterministicProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecutionResult{}, err
	}
	if request.Policy != effectivePolicy() || request.WebResearch.Enabled {
		return ExecutionResult{}, ErrPolicyViolation
	}
	if err := validateSchema(request.OutputSchema); err != nil {
		return ExecutionResult{}, err
	}
	output := append([]byte(nil), p.Output...)
	if len(output) == 0 {
		value, err := fixtureValue(request.OutputSchema, 0)
		if err != nil {
			return ExecutionResult{}, fmt.Errorf("generate deterministic output: %w", err)
		}
		output, err = json.Marshal(value)
		if err != nil {
			return ExecutionResult{}, fmt.Errorf("marshal deterministic output: %w", err)
		}
	}
	value, err := decodeJSON(output)
	if err != nil {
		return ExecutionResult{}, ErrStructuredOutput
	}
	if err := rejectSecretKeys(value, "$"); err != nil {
		return ExecutionResult{}, err
	}
	return ExecutionResult{ProviderID: p.ProviderID, ModelID: p.ModelID, Output: output}, nil
}

const maxFixtureCollectionItems = 64

func fixtureValue(schemaData []byte, depth int) (any, error) {
	if depth > 32 {
		return nil, errors.New("schema is too deep")
	}
	decoded, err := decodeJSON(schemaData)
	if err != nil {
		return nil, err
	}
	schema, ok := decoded.(map[string]any)
	if !ok {
		return nil, errors.New("schema must be an object")
	}
	return fixtureValueFromSchema(schema, depth)
}

func fixtureValueFromSchema(schema map[string]any, depth int) (any, error) {
	if depth > 32 {
		return nil, errors.New("schema is too deep")
	}
	if value, ok := schema["const"]; ok {
		return value, nil
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		return values[0], nil
	}
	schemaType, ok := schema["type"]
	if !ok {
		if _, hasProperties := schema["properties"]; hasProperties {
			return fixtureObject(schema, depth)
		}
		if _, hasItems := schema["items"]; hasItems {
			return fixtureArray(schema, depth)
		}
		return nil, nil
	}
	if types, ok := schemaType.([]any); ok {
		for _, value := range types {
			name, ok := value.(string)
			if !ok {
				continue
			}
			candidate := make(map[string]any, len(schema)+1)
			for key, child := range schema {
				candidate[key] = child
			}
			candidate["type"] = name
			fixture, fixtureErr := fixtureValueFromSchema(candidate, depth)
			if fixtureErr == nil {
				return fixture, nil
			}
		}
		return nil, errors.New("schema union has no supported type")
	}
	name, ok := schemaType.(string)
	if !ok {
		return nil, errors.New("schema type is invalid")
	}
	switch name {
	case "null":
		return nil, nil
	case "boolean":
		return false, nil
	case "integer", "number":
		return json.Number("0"), nil
	case "string":
		return fixtureString(schema)
	case "object":
		return fixtureObject(schema, depth)
	case "array":
		return fixtureArray(schema, depth)
	default:
		return nil, fmt.Errorf("unsupported schema type %q", name)
	}
}

func fixtureObject(schema map[string]any, depth int) (map[string]any, error) {
	properties, _ := schema["properties"].(map[string]any)
	result := make(map[string]any, len(properties))
	for name, raw := range properties {
		child, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("property %q is invalid", name)
		}
		value, err := fixtureValueFromSchema(child, depth+1)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", name, err)
		}
		result[name] = value
	}
	return result, nil
}

func fixtureArray(schema map[string]any, depth int) ([]any, error) {
	count, err := fixtureCount(schema, "minItems")
	if err != nil {
		return nil, err
	}
	if count > maxFixtureCollectionItems {
		return nil, fmt.Errorf("%s exceeds deterministic fixture bound", "minItems")
	}
	items, _ := schema["items"].(map[string]any)
	result := make([]any, 0, count)
	for index := 0; index < count; index++ {
		if items == nil {
			result = append(result, nil)
			continue
		}
		value, err := fixtureValueFromSchema(items, depth+1)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", index, err)
		}
		result = append(result, value)
	}
	return result, nil
}

func fixtureString(schema map[string]any) (string, error) {
	min, err := fixtureLength(schema, "minLength")
	if err != nil {
		return "", err
	}
	max, hasMax, err := optionalFixtureLength(schema, "maxLength")
	if err != nil {
		return "", err
	}
	if hasMax && min > max {
		return "", errors.New("minLength exceeds maxLength")
	}
	value := "deterministic-fixture"
	if len([]rune(value)) < min {
		value = strings.Repeat("x", min)
	}
	if hasMax && len([]rune(value)) > max {
		value = string([]rune(value)[:max])
	}
	return value, nil
}

func fixtureCount(schema map[string]any, key string) (int, error) {
	value, ok := schema[key]
	if !ok {
		return 0, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s is not an integer", key)
	}
	parsed, err := number.Int64()
	if err != nil || parsed < 0 || parsed > int64(maxFixtureCollectionItems) {
		return 0, fmt.Errorf("%s exceeds deterministic fixture bound", key)
	}
	return int(parsed), nil
}

func fixtureLength(schema map[string]any, key string) (int, error) {
	value, ok := schema[key]
	if !ok {
		return 0, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s is not an integer", key)
	}
	parsed, err := number.Int64()
	if err != nil || parsed < 0 || parsed > MaxOutputBytes {
		return 0, fmt.Errorf("%s exceeds deterministic fixture bound", key)
	}
	return int(parsed), nil
}

func optionalFixtureLength(schema map[string]any, key string) (int, bool, error) {
	value, ok := schema[key]
	if !ok {
		return 0, false, nil
	}
	length, err := fixtureLength(map[string]any{key: value}, key)
	return length, true, err
}
