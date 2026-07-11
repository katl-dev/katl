package configbundle

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"gopkg.in/yaml.v3"
)

const SourceSchemaID = "https://katl.dev/schemas/config.katl.dev/v1alpha1/cluster-config.json"

type sourceSchema struct {
	Schema string                  `json:"$schema"`
	ID     string                  `json:"$id"`
	Title  string                  `json:"title"`
	Ref    string                  `json:"$ref"`
	Defs   map[string]schemaObject `json:"$defs"`
}

type schemaObject struct {
	Ref                  string                  `json:"$ref,omitempty"`
	Type                 string                  `json:"type,omitempty"`
	Const                string                  `json:"const,omitempty"`
	Enum                 []string                `json:"enum,omitempty"`
	Properties           map[string]schemaObject `json:"properties,omitempty"`
	Required             []string                `json:"required,omitempty"`
	AdditionalProperties any                     `json:"additionalProperties,omitempty"`
	Items                *schemaObject           `json:"items,omitempty"`
	Minimum              *int                    `json:"minimum,omitempty"`
}

// SourceSchema returns the JSON Schema for the ClusterConfig accepted by this
// version of the compiler. The schema is derived from the decoder's Go types so
// newly accepted fields cannot be added without changing the published schema.
func SourceSchema() ([]byte, error) {
	builder := schemaBuilder{defs: map[string]schemaObject{}}
	root := reflect.TypeOf(SourceConfig{})
	builder.schemaFor(root)
	document := sourceSchema{
		Schema: "https://json-schema.org/draft/2020-12/schema",
		ID:     SourceSchemaID,
		Title:  APIVersion + " " + Kind,
		Ref:    "#/$defs/" + schemaTypeName(root),
		Defs:   builder.defs,
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
	if t == reflect.TypeOf(inventory.SystemRole("")) {
		return schemaObject{Type: "string", Enum: []string{string(inventory.RoleControlPlane), string(inventory.RoleWorker)}}
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
			name, _, visible := yamlField(field)
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
				required = append(required, name)
			}
			properties[name] = property
		}
		sort.Strings(required)
		builder.defs[name] = schemaObject{
			Type:                 "object",
			Properties:           properties,
			Required:             required,
			AdditionalProperties: false,
		}
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
		zero := 0
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
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	return validateYAMLFields(node, reflect.TypeOf(SourceConfig{}), "")
}

func validateYAMLFields(node *yaml.Node, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return nil
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
				return fmt.Errorf("%s: field is not supported (line %d)", fieldPath, key.Line)
			}
			if err := validateYAMLFields(value, fieldType, fieldPath); err != nil {
				return err
			}
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := validateYAMLFields(node.Content[i+1], t.Elem(), joinFieldPath(path, node.Content[i].Value)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return nil
		}
		for i, item := range node.Content {
			itemPath := fmt.Sprintf("%s[%d]", path, i)
			if err := validateYAMLFields(item, t.Elem(), itemPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func joinFieldPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}
