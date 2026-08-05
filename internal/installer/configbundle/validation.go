package configbundle

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type validationIssue struct {
	Path    string
	Message string
	Line    int
}

type validationErrors struct {
	issues []validationIssue
}

func (e *validationErrors) Error() string {
	if e == nil || len(e.issues) == 0 {
		return ""
	}
	var out strings.Builder
	noun := "errors"
	if len(e.issues) == 1 {
		noun = "error"
	}
	fmt.Fprintf(&out, "cluster config has %d validation %s:", len(e.issues), noun)
	for _, issue := range e.issues {
		out.WriteString("\n- ")
		out.WriteString(issue.Message)
		if issue.Line > 0 && !strings.Contains(issue.Message, "(line ") {
			fmt.Fprintf(&out, " (line %d)", issue.Line)
		}
	}
	return out.String()
}

func newValidationErrors(issues []validationIssue) error {
	if len(issues) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	filtered := make([]validationIssue, 0, len(issues))
	for _, issue := range issues {
		issue.Path = strings.TrimSpace(issue.Path)
		issue.Message = strings.TrimSpace(issue.Message)
		if issue.Message == "" {
			continue
		}
		key := issue.Path + "\x00" + issue.Message
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, issue)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Path != filtered[j].Path {
			return filtered[i].Path < filtered[j].Path
		}
		if filtered[i].Line != filtered[j].Line {
			return filtered[i].Line < filtered[j].Line
		}
		return filtered[i].Message < filtered[j].Message
	})
	if len(filtered) == 0 {
		return nil
	}
	return &validationErrors{issues: filtered}
}

func validationIssuesFromErrors(errs []error, lines map[string]int) []validationIssue {
	var issues []validationIssue
	for _, err := range errs {
		if err == nil {
			continue
		}
		var aggregate *validationErrors
		if errors.As(err, &aggregate) {
			for _, issue := range aggregate.issues {
				if issue.Line == 0 {
					issue.Line = nearestValidationLine(issue.Path, lines)
				}
				issues = append(issues, issue)
			}
			continue
		}
		message := strings.TrimSpace(err.Error())
		path := nearestValidationPath(message, lines)
		issues = append(issues, validationIssue{Path: path, Message: message, Line: nearestValidationLine(path, lines)})
	}
	return issues
}

func nearestValidationPath(message string, lines map[string]int) string {
	if token := validationPathToken(message); token != "" {
		return token
	}
	best := ""
	for path := range lines {
		if len(path) <= len(best) || !strings.HasPrefix(message, path) {
			continue
		}
		if len(message) > len(path) {
			switch message[len(path)] {
			case '.', '[', ':', ' ':
			default:
				continue
			}
		}
		best = path
	}
	if best != "" {
		return best
	}
	for _, prefix := range []string{"apiVersion", "kind", "metadata", "spec"} {
		if strings.HasPrefix(message, prefix) {
			return prefix
		}
	}
	return "document"
}

func validationPathToken(message string) string {
	end := len(message)
	for i, char := range message {
		if char == ':' || char == ' ' {
			end = i
			break
		}
	}
	token := strings.TrimRight(message[:end], ".,;")
	for _, prefix := range []string{"apiVersion", "kind", "metadata", "spec"} {
		if token == prefix || strings.HasPrefix(token, prefix+".") || strings.HasPrefix(token, prefix+"[") {
			return token
		}
	}
	return ""
}

func nearestValidationLine(path string, lines map[string]int) int {
	for candidate := path; candidate != ""; candidate = parentValidationPath(candidate) {
		if line := lines[candidate]; line > 0 {
			return line
		}
	}
	return 0
}

func parentValidationPath(path string) string {
	if index := strings.LastIndex(path, "."); index >= 0 {
		return path[:index]
	}
	if index := strings.LastIndex(path, "["); index >= 0 {
		return path[:index]
	}
	return ""
}

// ValidateSourceFile reports every independent source and semantic error that
// can be determined without resolving external artifacts.
func ValidateSourceFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read source config: %w", err)
	}
	return validateSourceData(data)
}

func validateSourceData(data []byte) error {
	var document yaml.Node
	nodeDecoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := nodeDecoder.Decode(&document); err != nil {
		return fmt.Errorf("decode cluster config: %w", err)
	}
	var trailing yaml.Node
	if err := nodeDecoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("decode cluster config: multiple YAML documents")
	}

	lines := sourceLineIndex(&document)
	issues := validateSourceFieldIssues(&document)
	structuralPaths := map[string]struct{}{}
	for _, issue := range issues {
		if strings.Contains(issue.Message, " must be ") {
			structuralPaths[issue.Path] = struct{}{}
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var source SourceConfig
	decodeErr := decoder.Decode(&source)
	if decodeErr != nil {
		for _, issue := range yamlDecodeIssues(decodeErr, lines) {
			if _, covered := structuralPaths[issue.Path]; !covered {
				issues = append(issues, issue)
			}
		}
	}
	if source.APIVersion != APIVersion && !validationPathCovered("apiVersion", structuralPaths) {
		issues = append(issues, validationIssue{Path: "apiVersion", Message: fmt.Sprintf("apiVersion must be %s", APIVersion), Line: lines["apiVersion"]})
	}
	if source.Kind != Kind && !validationPathCovered("kind", structuralPaths) {
		issues = append(issues, validationIssue{Path: "kind", Message: fmt.Sprintf("kind must be %s", Kind), Line: lines["kind"]})
	}

	normalized, semanticErrs := normalizeSourceIssues(source)
	for _, issue := range validationIssuesFromErrors(semanticErrs, lines) {
		if !validationPathCovered(issue.Path, structuralPaths) {
			issues = append(issues, issue)
		}
	}
	for _, issue := range validationIssuesFromErrors(validateResolvedSourceNodeIssues(normalized), lines) {
		if !validationPathCovered(issue.Path, structuralPaths) {
			issues = append(issues, issue)
		}
	}
	return newValidationErrors(issues)
}

func validationPathCovered(path string, invalid map[string]struct{}) bool {
	for candidate := range invalid {
		if path == candidate || strings.HasPrefix(path, candidate+".") || strings.HasPrefix(path, candidate+"[") ||
			strings.HasPrefix(candidate, path+".") || strings.HasPrefix(candidate, path+"[") {
			return true
		}
	}
	return false
}

var yamlLineError = regexp.MustCompile(`^line ([0-9]+): (.*)$`)

func yamlDecodeIssues(err error, lines map[string]int) []validationIssue {
	messages := []string{err.Error()}
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		messages = typeErr.Errors
	}
	var issues []validationIssue
	for _, message := range messages {
		line := 0
		detail := strings.TrimSpace(message)
		if match := yamlLineError.FindStringSubmatch(detail); len(match) == 3 {
			line, _ = strconv.Atoi(match[1])
			detail = match[2]
		}
		path := pathAtLine(line, lines)
		if path == "" {
			path = "document"
		}
		if strings.Contains(detail, "cannot unmarshal") {
			detail = "value has the wrong type"
		}
		issues = append(issues, validationIssue{Path: path, Message: path + ": " + detail, Line: line})
	}
	return issues
}

func pathAtLine(line int, lines map[string]int) string {
	best := ""
	for path, candidateLine := range lines {
		if candidateLine == line && len(path) > len(best) {
			best = path
		}
	}
	return best
}

func sourceLineIndex(document *yaml.Node) map[string]int {
	lines := map[string]int{}
	if document.Kind == yaml.DocumentNode && len(document.Content) == 1 {
		document = document.Content[0]
	}
	indexYAMLLines(document, reflect.TypeOf(SourceConfig{}), "", lines)
	return lines
}

func indexYAMLLines(node *yaml.Node, t reflect.Type, path string, lines map[string]int) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if valueType, ok := optionalValueType(t); ok {
		t = valueType
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
	}
	if path != "" && node.Line > 0 {
		lines[path] = node.Line
	}
	switch t.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
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
			fieldPath := joinFieldPath(path, key.Value)
			lines[fieldPath] = key.Line
			if fieldType, ok := fields[key.Value]; ok {
				indexYAMLLines(value, fieldType, fieldPath, lines)
			}
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			fieldPath := joinFieldPath(path, key.Value)
			quotedPath := fmt.Sprintf("%s[%q]", path, key.Value)
			lines[fieldPath] = key.Line
			lines[quotedPath] = key.Line
			indexYAMLLines(value, t.Elem(), fieldPath, lines)
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return
		}
		for i, item := range node.Content {
			itemPath := sequenceItemPath(path, item, i)
			lines[itemPath] = item.Line
			indexYAMLLines(item, t.Elem(), itemPath, lines)
		}
	}
}
