package configbundle

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const SourceSchemaID = "https://katl.dev/schemas/config.katl.dev/v1alpha1/cluster-config.json"

type sourceSchema struct {
	Schema      string                  `json:"$schema"`
	ID          string                  `json:"$id"`
	Title       string                  `json:"title"`
	Description string                  `json:"description"`
	Ref         string                  `json:"$ref"`
	Defs        map[string]schemaObject `json:"$defs"`
}

type schemaObject struct {
	Ref                  string                  `json:"$ref,omitempty"`
	Type                 string                  `json:"type,omitempty"`
	Const                any                     `json:"const,omitempty"`
	Enum                 []any                   `json:"enum,omitempty"`
	Description          string                  `json:"description,omitempty"`
	Default              any                     `json:"default,omitempty"`
	Properties           map[string]schemaObject `json:"properties,omitempty"`
	Required             []string                `json:"required,omitempty"`
	AdditionalProperties any                     `json:"additionalProperties,omitempty"`
	PropertyNames        *schemaObject           `json:"propertyNames,omitempty"`
	Items                *schemaObject           `json:"items,omitempty"`
	MinItems             *int                    `json:"minItems,omitempty"`
	MaxItems             *int                    `json:"maxItems,omitempty"`
	MinLength            *int                    `json:"minLength,omitempty"`
	MaxLength            *int                    `json:"maxLength,omitempty"`
	Pattern              string                  `json:"pattern,omitempty"`
	Format               string                  `json:"format,omitempty"`
	Minimum              *int64                  `json:"minimum,omitempty"`
	Maximum              *int64                  `json:"maximum,omitempty"`
	OneOf                []schemaObject          `json:"oneOf,omitempty"`
	AnyOf                []schemaObject          `json:"anyOf,omitempty"`
	Not                  *schemaObject           `json:"not,omitempty"`
}

// SourceSchema returns the JSON Schema for the ClusterConfig accepted by this
// version of the compiler. The schema is derived from the decoder's Go types so
// newly accepted fields cannot be added without changing the published schema.
func SourceSchema() ([]byte, error) {
	builder := schemaBuilder{defs: map[string]schemaObject{}}
	root := reflect.TypeOf(SourceConfig{})
	builder.schemaFor(root)
	document := sourceSchema{
		Schema:      "https://json-schema.org/draft/2020-12/schema",
		ID:          SourceSchemaID,
		Title:       APIVersion + " " + Kind,
		Description: "Katl cluster intent compiled into per-node install and runtime configuration.",
		Ref:         "#/$defs/" + schemaTypeName(root),
		Defs:        builder.defs,
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal ClusterConfig schema: %w", err)
	}
	return append(data, '\n'), nil
}

type schemaBuilder struct {
	defs map[string]schemaObject
}

func (builder *schemaBuilder) schemaFor(t reflect.Type) schemaObject {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if valueType, ok := optionalValueType(t); ok {
		return builder.schemaFor(valueType)
	}
	switch t.Kind() {
	case reflect.Struct:
		name := schemaTypeName(t)
		ref := schemaObject{Ref: "#/$defs/" + name}
		if _, exists := builder.defs[name]; exists {
			return ref
		}
		// Reserve the definition before descending so recursive types are safe.
		builder.defs[name] = schemaObject{}
		properties := map[string]schemaObject{}
		var required []string
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			name, omitEmpty, visible := yamlField(field)
			if !visible {
				continue
			}
			property := builder.schemaFor(field.Type)
			if t == reflect.TypeOf(SourceConfig{}) {
				switch name {
				case "apiVersion":
					property = schemaObject{Type: "string", Const: APIVersion}
				case "kind":
					property = schemaObject{Type: "string", Const: Kind}
				}
			}
			rule := sourceSchemaFieldRule(t, name)
			property = applySchemaFieldRule(property, rule)
			if !omitEmpty || rule.Required {
				required = append(required, name)
			}
			properties[name] = property
		}
		sort.Strings(required)
		object := schemaObject{
			Type:                 "object",
			Properties:           properties,
			Required:             required,
			AdditionalProperties: false,
		}
		builder.defs[name] = applySchemaTypeRule(object, sourceSchemaTypeRule(t))
		return ref
	case reflect.Slice, reflect.Array:
		items := builder.schemaFor(t.Elem())
		return schemaObject{Type: "array", Items: &items}
	case reflect.Map:
		value := builder.schemaFor(t.Elem())
		return schemaObject{Type: "object", AdditionalProperties: value}
	case reflect.Bool:
		return schemaObject{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return schemaObject{Type: "integer"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		zero := int64(0)
		return schemaObject{Type: "integer", Minimum: &zero}
	case reflect.Float32, reflect.Float64:
		return schemaObject{Type: "number"}
	default:
		return schemaObject{Type: "string"}
	}
}

func schemaTypeName(t reflect.Type) string {
	path := t.PkgPath()
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		path = path[index+1:]
	}
	return path + "." + t.Name()
}

func yamlField(field reflect.StructField) (name string, omitEmpty, visible bool) {
	if field.PkgPath != "" {
		return "", false, false
	}
	tag := field.Tag.Get("yaml")
	parts := strings.Split(tag, ",")
	if parts[0] == "-" {
		return "", false, false
	}
	name = parts[0]
	if name == "" {
		name = strings.ToLower(field.Name[:1]) + field.Name[1:]
	}
	for _, option := range parts[1:] {
		if option == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, true
}

func validateSourceFields(node *yaml.Node) error {
	return newValidationErrors(validateSourceFieldIssues(node))
}

func validateSourceFieldIssues(node *yaml.Node) []validationIssue {
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	var issues []validationIssue
	validateYAMLFields(node, reflect.TypeOf(SourceConfig{}), "", &issues)
	return issues
}

func validateYAMLFields(node *yaml.Node, t reflect.Type, path string, issues *[]validationIssue) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if valueType, ok := optionalValueType(t); ok {
		validateYAMLFields(node, valueType, path, issues)
		return
	}
	if node.Tag == "!!null" {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			addYAMLShapeIssue(issues, path, "an object", node.Line)
			return
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			name, _, visible := yamlField(field)
			if visible {
				fields[name] = field.Type
			}
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			fieldType, ok := fields[key.Value]
			fieldPath := joinFieldPath(path, key.Value)
			if !ok {
				*issues = append(*issues, validationIssue{Path: fieldPath, Message: fmt.Sprintf("%s: field is not supported", fieldPath), Line: key.Line})
				continue
			}
			validateYAMLFields(value, fieldType, fieldPath, issues)
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			addYAMLShapeIssue(issues, path, "a map", node.Line)
			return
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			validateYAMLFields(node.Content[i+1], t.Elem(), joinFieldPath(path, node.Content[i].Value), issues)
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			addYAMLShapeIssue(issues, path, "a list", node.Line)
			return
		}
		for i, item := range node.Content {
			itemPath := sequenceItemPath(path, item, i)
			validateYAMLFields(item, t.Elem(), itemPath, issues)
		}
	case reflect.String:
		validateYAMLScalar(node, path, "a string", "!!str", issues)
	case reflect.Bool:
		validateYAMLScalar(node, path, "a boolean", "!!bool", issues)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		validateYAMLScalar(node, path, "an integer", "!!int", issues)
	case reflect.Float32, reflect.Float64:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!float" && node.Tag != "!!int" {
			addYAMLShapeIssue(issues, path, "a number", node.Line)
		}
	}
}

func validateYAMLScalar(node *yaml.Node, path, expected, tag string, issues *[]validationIssue) {
	if node.Kind != yaml.ScalarNode || node.Tag != tag {
		addYAMLShapeIssue(issues, path, expected, node.Line)
	}
}

func addYAMLShapeIssue(issues *[]validationIssue, path, expected string, line int) {
	if path == "" {
		path = "document"
	}
	*issues = append(*issues, validationIssue{Path: path, Message: fmt.Sprintf("%s must be %s", path, expected), Line: line})
}

func sequenceItemPath(path string, item *yaml.Node, index int) string {
	if path == "spec.nodes" && item.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(item.Content); i += 2 {
			if item.Content[i].Value == "name" && strings.TrimSpace(item.Content[i+1].Value) != "" {
				return fmt.Sprintf("%s[%q]", path, strings.TrimSpace(item.Content[i+1].Value))
			}
		}
	}
	return fmt.Sprintf("%s[%d]", path, index)
}

func joinFieldPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}
