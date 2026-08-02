package advisory

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// The boundary intentionally supports a small, strict JSON Schema subset. A
// schema that asks for an unsupported validation feature is rejected rather
// than silently validated less strictly than its caller expects.
func validateSchema(data []byte, safety *SafetyPipeline) error {
	if safety == nil {
		safety = NewSafetyPipeline(newAdvisoryRedactor(), SafetyConfig{MaxInputBytes: MaxSchemaBytes})
	}
	if err := safety.InspectJSON(data); err != nil {
		switch {
		case errors.Is(err, ErrUnsafeContent):
			return ErrPolicyViolation
		case errors.Is(err, ErrSafetyBounds), errors.Is(err, ErrMalformedPayload):
			return fmt.Errorf("%w: malformed or bounded schema", ErrInvalidRequest)
		default:
			return fmt.Errorf("%w: malformed schema", ErrInvalidRequest)
		}
	}
	schema, err := decodeJSON(data)
	if err != nil {
		if errors.Is(err, errDuplicateJSONKey) {
			return fmt.Errorf("%w: schema contains duplicate JSON keys", ErrInvalidRequest)
		}
		return fmt.Errorf("%w: malformed schema", ErrInvalidRequest)
	}
	object, ok := schema.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: schema must be an object", ErrInvalidRequest)
	}
	if err := validateSchemaDefinition(object, "$", 0); err != nil {
		if errors.Is(err, ErrPolicyViolation) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return nil
}

func validateSchemaDefinition(schema map[string]any, path string, depth int) error {
	if depth > 32 {
		return fmt.Errorf("schema %s is too deep", path)
	}
	allowed := map[string]bool{
		"$schema": true, "$id": true, "title": true, "description": true,
		"type": true, "properties": true, "required": true,
		"additionalProperties": true, "items": true, "enum": true, "const": true,
		"minItems": true, "maxItems": true, "minLength": true, "maxLength": true,
	}
	for key := range schema {
		if !allowed[key] {
			return fmt.Errorf("schema %s uses unsupported keyword %q", path, key)
		}
	}
	for _, key := range []string{"$schema", "$id", "title", "description"} {
		if value, ok := schema[key]; ok {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("schema %s %s must be a string", path, key)
			}
		}
	}
	if value, ok := schema["type"]; ok {
		if !validSchemaType(value) {
			return fmt.Errorf("schema %s has invalid type", path)
		}
	}
	if value, ok := schema["additionalProperties"]; ok {
		allowedProperties, ok := value.(bool)
		if !ok || allowedProperties {
			return fmt.Errorf("schema %s must disallow additional properties", path)
		}
	}
	if value, ok := schema["required"]; ok {
		required, ok := value.([]any)
		if !ok {
			return fmt.Errorf("schema %s required must be an array", path)
		}
		seen := make(map[string]struct{}, len(required))
		for _, item := range required {
			name, ok := item.(string)
			if !ok || name == "" {
				return fmt.Errorf("schema %s contains an invalid required property", path)
			}
			if safetyKey(name) {
				return fmt.Errorf("%w: schema %s contains secret-shaped property %q", ErrPolicyViolation, path, name)
			}
			if _, exists := seen[name]; exists {
				return fmt.Errorf("schema %s repeats required property %q", path, name)
			}
			seen[name] = struct{}{}
		}
	}
	if value, ok := schema["properties"]; ok {
		properties, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("schema %s properties must be an object", path)
		}
		for name, child := range properties {
			if safetyKey(name) {
				return fmt.Errorf("%w: schema %s contains secret-shaped property %q", ErrPolicyViolation, path, name)
			}
			childSchema, ok := child.(map[string]any)
			if !ok {
				return fmt.Errorf("schema %s property %q is invalid", path, name)
			}
			if err := validateSchemaDefinition(childSchema, path+"."+name, depth+1); err != nil {
				return err
			}
		}
	}
	if value, ok := schema["items"]; ok {
		items, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("schema %s items must be an object", path)
		}
		if err := validateSchemaDefinition(items, path+"[]", depth+1); err != nil {
			return err
		}
	}
	for _, key := range []string{"enum"} {
		if value, ok := schema[key]; ok {
			values, ok := value.([]any)
			if !ok || len(values) == 0 {
				return fmt.Errorf("schema %s %s must be a non-empty array", path, key)
			}
		}
	}
	for _, key := range []string{"minItems", "maxItems", "minLength", "maxLength"} {
		if value, ok := schema[key]; ok {
			number, ok := value.(json.Number)
			if !ok {
				return fmt.Errorf("schema %s %s must be a non-negative integer", path, key)
			}
			parsed, err := number.Int64()
			if err != nil || parsed < 0 {
				return fmt.Errorf("schema %s %s must be a non-negative integer", path, key)
			}
		}
	}
	return nil
}

func validSchemaType(value any) bool {
	if name, ok := value.(string); ok {
		return validTypeName(name)
	}
	values, ok := value.([]any)
	if !ok || len(values) == 0 {
		return false
	}
	for _, item := range values {
		name, ok := item.(string)
		if !ok || !validTypeName(name) {
			return false
		}
	}
	return true
}

func validTypeName(value string) bool {
	switch value {
	case "null", "boolean", "object", "array", "number", "integer", "string":
		return true
	default:
		return false
	}
}

func validateOutput(schemaData []byte, value any, path string, depth int) error {
	decoded, err := decodeJSON(schemaData)
	if err != nil {
		return err
	}
	schema, ok := decoded.(map[string]any)
	if !ok {
		return errors.New("schema must be an object")
	}
	return validateValue(schema, value, path, depth)
}

func validateValue(schema map[string]any, value any, path string, depth int) error {
	if depth > 32 {
		return fmt.Errorf("%s is too deep", path)
	}
	if typeValue, ok := schema["type"]; ok && !matchesType(typeValue, value) {
		return fmt.Errorf("%s has the wrong type", path)
	}
	if enumValue, ok := schema["enum"].([]any); ok {
		matched := false
		for _, candidate := range enumValue {
			if reflect.DeepEqual(candidate, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s is not an allowed value", path)
		}
	}
	if constant, ok := schema["const"]; ok && !reflect.DeepEqual(constant, value) {
		return fmt.Errorf("%s does not match const", path)
	}
	if object, ok := value.(map[string]any); ok {
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, item := range required {
			name := item.(string)
			if _, exists := object[name]; !exists {
				return fmt.Errorf("%s is missing required property %q", path, name)
			}
		}
		for name, childValue := range object {
			child, exists := properties[name]
			if !exists {
				return fmt.Errorf("%s contains additional property %q", path, name)
			}
			childSchema := child.(map[string]any)
			if err := validateValue(childSchema, childValue, path+"."+name, depth+1); err != nil {
				return err
			}
		}
	}
	if array, ok := value.([]any); ok {
		if err := validateCount(schema, "minItems", len(array), path); err != nil {
			return err
		}
		if err := validateMaxCount(schema, "maxItems", len(array), path); err != nil {
			return err
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for index, childValue := range array {
				if err := validateValue(items, childValue, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
					return err
				}
			}
		}
	}
	if text, ok := value.(string); ok {
		if err := validateCount(schema, "minLength", len([]rune(text)), path); err != nil {
			return err
		}
		if err := validateMaxCount(schema, "maxLength", len([]rune(text)), path); err != nil {
			return err
		}
	}
	return nil
}

func matchesType(schemaType any, value any) bool {
	types := []string{}
	switch typed := schemaType.(type) {
	case string:
		types = []string{typed}
	case []any:
		for _, item := range typed {
			types = append(types, item.(string))
		}
	}
	for _, name := range types {
		switch name {
		case "null":
			if value == nil {
				return true
			}
		case "boolean":
			if _, ok := value.(bool); ok {
				return true
			}
		case "object":
			if _, ok := value.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := value.([]any); ok {
				return true
			}
		case "number":
			if _, ok := value.(json.Number); ok {
				return true
			}
		case "integer":
			if number, ok := value.(json.Number); ok {
				if _, err := number.Int64(); err == nil {
					return true
				}
			}
		case "string":
			if _, ok := value.(string); ok {
				return true
			}
		}
	}
	return false
}

func validateCount(schema map[string]any, key string, actual int, path string) error {
	value, ok := schema[key].(json.Number)
	if !ok {
		return nil
	}
	want, err := value.Int64()
	if err != nil || int64(actual) < want {
		return fmt.Errorf("%s is shorter than %s", path, key)
	}
	return nil
}

func validateMaxCount(schema map[string]any, key string, actual int, path string) error {
	value, ok := schema[key].(json.Number)
	if !ok {
		return nil
	}
	want, err := value.Int64()
	if err != nil || int64(actual) > want {
		return fmt.Errorf("%s exceeds %s", path, key)
	}
	return nil
}
