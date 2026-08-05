package configbundle

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type sourceSchemaValidator struct {
	document map[string]any
}

func newSourceSchemaValidator(t *testing.T) sourceSchemaValidator {
	t.Helper()
	data, err := SourceSchema()
	if err != nil {
		t.Fatalf("SourceSchema() error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode source schema: %v", err)
	}
	return sourceSchemaValidator{document: document}
}

func (validator sourceSchemaValidator) Validate(value any) error {
	return validator.validate(validator.document, value, "$")
}

func sourceSchemaValue(t *testing.T, source string) any {
	t.Helper()
	var value any
	if err := yaml.Unmarshal([]byte(source), &value); err != nil {
		t.Fatalf("decode source YAML: %v", err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode source JSON: %v", err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode source JSON: %v", err)
	}
	return value
}

func (validator sourceSchemaValidator) validate(schema map[string]any, value any, path string) error {
	if ref, ok := schema["$ref"].(string); ok {
		const prefix = "#/$defs/"
		if !strings.HasPrefix(ref, prefix) {
			return fmt.Errorf("%s: unsupported schema reference %q", path, ref)
		}
		definitions := validator.document["$defs"].(map[string]any)
		definition, ok := definitions[strings.TrimPrefix(ref, prefix)].(map[string]any)
		if !ok {
			return fmt.Errorf("%s: unresolved schema reference %q", path, ref)
		}
		if err := validator.validate(definition, value, path); err != nil {
			return err
		}
	}
	if expected, exists := schema["const"]; exists && !equalJSON(expected, value) {
		return fmt.Errorf("%s: value does not equal const", path)
	}
	if enums, ok := schema["enum"].([]any); ok {
		matched := false
		for _, candidate := range enums {
			matched = matched || equalJSON(candidate, value)
		}
		if !matched {
			return fmt.Errorf("%s: value is not in enum", path)
		}
	}
	if err := validator.validateCombinations(schema, value, path); err != nil {
		return err
	}
	typeName, _ := schema["type"].(string)
	if typeName == "" && hasObjectKeywords(schema) {
		if object, ok := value.(map[string]any); ok {
			return validator.validateObject(schema, object, path)
		}
	}
	switch typeName {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: value is not an object", path)
		}
		return validator.validateObject(schema, object, path)
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: value is not an array", path)
		}
		return validator.validateArray(schema, array, path)
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s: value is not a string", path)
		}
		return validateSchemaString(schema, text, path)
	case "integer":
		number, ok := value.(float64)
		if !ok || math.Trunc(number) != number {
			return fmt.Errorf("%s: value is not an integer", path)
		}
		return validateSchemaNumber(schema, number, path)
	case "number":
		number, ok := value.(float64)
		if !ok {
			return fmt.Errorf("%s: value is not a number", path)
		}
		return validateSchemaNumber(schema, number, path)
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: value is not a boolean", path)
		}
	}
	return nil
}

func hasObjectKeywords(schema map[string]any) bool {
	for _, keyword := range []string{"properties", "required", "additionalProperties", "propertyNames"} {
		if _, ok := schema[keyword]; ok {
			return true
		}
	}
	return false
}

func (validator sourceSchemaValidator) validateCombinations(schema map[string]any, value any, path string) error {
	if alternatives, ok := schema["oneOf"].([]any); ok {
		matches := 0
		for _, alternative := range alternatives {
			if validator.validate(alternative.(map[string]any), value, path) == nil {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("%s: value matches %d oneOf branches", path, matches)
		}
	}
	if alternatives, ok := schema["anyOf"].([]any); ok {
		for _, alternative := range alternatives {
			if validator.validate(alternative.(map[string]any), value, path) == nil {
				return nil
			}
		}
		return fmt.Errorf("%s: value does not match anyOf", path)
	}
	if excluded, ok := schema["not"].(map[string]any); ok && validator.validate(excluded, value, path) == nil {
		return fmt.Errorf("%s: value matches excluded schema", path)
	}
	return nil
}

func (validator sourceSchemaValidator) validateObject(schema map[string]any, object map[string]any, path string) error {
	for _, field := range stringsFromJSON(schema["required"]) {
		if _, ok := object[field]; !ok {
			return fmt.Errorf("%s.%s: required property is missing", path, field)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for field, value := range object {
		property, known := properties[field].(map[string]any)
		if known {
			if err := validator.validate(property, value, path+"."+field); err != nil {
				return err
			}
			continue
		}
		switch additional := schema["additionalProperties"].(type) {
		case bool:
			if !additional {
				return fmt.Errorf("%s.%s: additional property is not allowed", path, field)
			}
		case map[string]any:
			if err := validator.validate(additional, value, path+"."+field); err != nil {
				return err
			}
		}
	}
	if names, ok := schema["propertyNames"].(map[string]any); ok {
		for field := range object {
			if err := validator.validate(names, field, path+"."+field); err != nil {
				return err
			}
		}
	}
	return nil
}

func (validator sourceSchemaValidator) validateArray(schema map[string]any, array []any, path string) error {
	if minimum, ok := schema["minItems"].(float64); ok && len(array) < int(minimum) {
		return fmt.Errorf("%s: array is too short", path)
	}
	if maximum, ok := schema["maxItems"].(float64); ok && len(array) > int(maximum) {
		return fmt.Errorf("%s: array is too long", path)
	}
	items, _ := schema["items"].(map[string]any)
	for i, value := range array {
		if err := validator.validate(items, value, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func validateSchemaString(schema map[string]any, value, path string) error {
	length := utf8.RuneCountInString(value)
	if minimum, ok := schema["minLength"].(float64); ok && length < int(minimum) {
		return fmt.Errorf("%s: string is too short", path)
	}
	if maximum, ok := schema["maxLength"].(float64); ok && length > int(maximum) {
		return fmt.Errorf("%s: string is too long", path)
	}
	if pattern, ok := schema["pattern"].(string); ok {
		pattern = strings.ReplaceAll(pattern, "?:", "")
		matched, err := regexp.MatchString(pattern, value)
		if err != nil || !matched {
			return fmt.Errorf("%s: string does not match pattern", path)
		}
	}
	switch schema["format"] {
	case "ipv4":
		if address := net.ParseIP(value); address == nil || address.To4() == nil {
			return fmt.Errorf("%s: string is not IPv4", path)
		}
	case "uuid":
		matched, _ := regexp.MatchString(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`, value)
		if !matched {
			return fmt.Errorf("%s: string is not a UUID", path)
		}
	}
	return nil
}

func validateSchemaNumber(schema map[string]any, value float64, path string) error {
	if minimum, ok := schema["minimum"].(float64); ok && value < minimum {
		return fmt.Errorf("%s: number is below minimum", path)
	}
	if maximum, ok := schema["maximum"].(float64); ok && value > maximum {
		return fmt.Errorf("%s: number is above maximum", path)
	}
	return nil
}

func stringsFromJSON(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, item.(string))
	}
	return result
}

func equalJSON(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}
