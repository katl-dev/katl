package config

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"gopkg.in/yaml.v3"
)

type Options struct {
	CheckKubeadmRefs   bool
	KubeadmConfigNames map[string]struct{}
}

type Result struct {
	Diagnostics []Diagnostic
}

func (r Result) Accepted() bool {
	return len(r.Diagnostics) == 0
}

func (r Result) Strings() []string {
	out := make([]string, 0, len(r.Diagnostics))
	for _, diagnostic := range r.Diagnostics {
		out = append(out, diagnostic.String())
	}
	sort.Strings(out)
	return out
}

type Diagnostic struct {
	Code    string
	Field   string
	Message string
}

func (d Diagnostic) String() string {
	if d.Field == "" {
		return d.Code + ": " + d.Message
	}
	return d.Code + ": " + d.Field + ": " + d.Message
}

func ValidateNodeConfigurationChange(input string, options Options) Result {
	var result Result
	decoder := yaml.NewDecoder(strings.NewReader(input))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return Result{Diagnostics: []Diagnostic{{
			Code:    "decode",
			Field:   "document",
			Message: fmt.Sprintf("decode NodeConfigurationChange: %v", err),
		}}}
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return Result{Diagnostics: []Diagnostic{{
			Code:    "decode",
			Field:   "document",
			Message: "multiple YAML documents are not supported",
		}}}
	}
	root := documentRoot(&document)
	if root == nil || root.Kind != yaml.MappingNode {
		return Result{Diagnostics: []Diagnostic{{
			Code:    "decode",
			Field:   "document",
			Message: "NodeConfigurationChange must be a YAML mapping",
		}}}
	}
	result.addDuplicateKeyDiagnostics(root, "")
	validateDocument(root, options, &result)
	sort.SliceStable(result.Diagnostics, func(i, j int) bool {
		left, right := result.Diagnostics[i], result.Diagnostics[j]
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		return left.Message < right.Message
	})
	return result
}

func validateDocument(root *yaml.Node, options Options, result *Result) {
	for _, pair := range mappingPairs(root) {
		switch pair.key {
		case "apiVersion", "kind", "metadata", "apply", "spec":
		default:
			result.add("unsupported-field", pair.path, "top-level field is not supported")
		}
	}
	if got := scalarAt(root, "apiVersion"); got != configapply.NodeConfigurationChangeAPIVersion {
		result.add("invalid-envelope", "apiVersion", fmt.Sprintf("must be %s", configapply.NodeConfigurationChangeAPIVersion))
	}
	if got := scalarAt(root, "kind"); got != configapply.NodeConfigurationChangeKind {
		result.add("invalid-envelope", "kind", fmt.Sprintf("must be %s", configapply.NodeConfigurationChangeKind))
	}
	spec := mappingValue(root, "spec")
	if spec == nil {
		result.add("missing-field", "spec", "spec is required")
		return
	}
	if spec.Kind != yaml.MappingNode {
		result.add("invalid-field", "spec", "spec must be a mapping")
		return
	}
	options.KubeadmConfigNames = mergedKubeadmConfigNames(options.KubeadmConfigNames, mappingValue(spec, "kubeadmConfigs"))
	for _, pair := range mappingPairsWithPath(spec, "spec") {
		switch pair.key {
		case "clusterDefaults":
			validateOverlay(pair.value, pair.path, options, result)
		case "systemRoleOverrides":
			validateOverlayMap(pair.value, pair.path, options, result)
		case "nodeOverrides":
			validateOverlayMap(pair.value, pair.path, options, result)
		case "kubeadmConfigs":
			validateInlineKubeadmConfigs(pair.value, pair.path, result)
		default:
			result.add(unsupportedCode(pair.key), pair.path, "configuration domain is not supported")
		}
	}
}

func mergedKubeadmConfigNames(installed map[string]struct{}, inline *yaml.Node) map[string]struct{} {
	names := make(map[string]struct{}, len(installed))
	for name := range installed {
		names[name] = struct{}{}
	}
	if inline != nil && inline.Kind == yaml.MappingNode {
		for _, pair := range mappingPairs(inline) {
			names[pair.key] = struct{}{}
		}
	}
	return names
}

func validateInlineKubeadmConfigs(node *yaml.Node, path string, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "kubeadmConfigs must be a mapping")
		return
	}
	for _, config := range mappingPairsWithPath(node, path) {
		if !validName(config.key) {
			result.add("invalid-kubeadm-name", config.path, fmt.Sprintf("%q must be a DNS label", config.key))
		}
		if config.value.Kind != yaml.MappingNode {
			result.add("invalid-field", config.path, "inline kubeadm config must be a mapping")
			continue
		}
		for _, field := range mappingPairsWithPath(config.value, config.path) {
			switch field.key {
			case "config":
				if strings.TrimSpace(scalarValue(field.value)) == "" {
					result.add("missing-kubeadm-config", field.path, "config must contain kubeadm YAML")
				}
			case "patches":
				validateInlineKubeadmPatches(field.value, field.path, result)
			default:
				result.add("unsupported-field", field.path, "inline kubeadm config field is not supported")
			}
		}
		if mappingValue(config.value, "config") == nil {
			result.add("missing-field", config.path+".config", "config is required")
		}
	}
}

func validateInlineKubeadmPatches(node *yaml.Node, path string, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "patches must be a mapping of file names to YAML content")
		return
	}
	for _, patch := range mappingPairsWithPath(node, path) {
		if err := validateNetworkdName(patch.key); err != nil {
			result.add("unsafe-render-path", patch.path, err.Error())
		}
		if strings.TrimSpace(scalarValue(patch.value)) == "" {
			result.add("missing-kubeadm-patch", patch.path, "patch must contain kubeadm patch YAML")
		}
	}
}

func validateOverlayMap(node *yaml.Node, path string, options Options, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "must be a mapping")
		return
	}
	for _, pair := range mappingPairsWithPath(node, path) {
		if !validName(pair.key) {
			result.add("invalid-node-name", pair.path, fmt.Sprintf("%q must be a DNS label", pair.key))
		}
		validateOverlay(pair.value, pair.path, options, result)
	}
}

func validateOverlay(node *yaml.Node, path string, options Options, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "overlay must be a mapping")
		return
	}
	for _, pair := range mappingPairsWithPath(node, path) {
		switch pair.key {
		case "identity":
			validateIdentity(pair.value, pair.path, result)
		case "systemRole":
			validateSystemRole(pair.value, pair.path, result)
		case "networkd":
			validateNetworkd(pair.value, pair.path, result)
		case "sysctl":
			validateSysctl(pair.value, pair.path, result)
		case "kubernetes":
			validateKubernetes(pair.value, pair.path, options, result)
		case "controlPlaneEndpoint":
			validateControlPlaneEndpoint(pair.value, pair.path, result)
		case "livePreflight":
			// The apply matrix validates the domain names and requested mode.
		default:
			result.add(unsupportedCode(pair.key), pair.path, "configuration domain is not supported")
		}
	}
}

func validateControlPlaneEndpoint(node *yaml.Node, path string, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "controlPlaneEndpoint must be a mapping")
		return
	}
	for _, pair := range mappingPairsWithPath(node, path) {
		switch pair.key {
		case "managed", "config":
		default:
			result.add("unsupported-field", pair.path, "controlPlaneEndpoint field is not supported")
		}
	}
	managed := mappingValue(node, "managed")
	if managed == nil {
		result.add("missing-field", path+".managed", "managed is required")
		return
	}
	if managed.Kind != yaml.ScalarNode || managed.Tag != "!!bool" {
		result.add("invalid-field", path+".managed", "managed must be true or false")
		return
	}
	config := mappingValue(node, "config")
	if managed.Value == "true" && config == nil {
		result.add("missing-field", path+".config", "config is required when managed is true")
	}
	if managed.Value == "false" && config != nil {
		result.add("invalid-field", path+".config", "config must be omitted when managed is false")
	}
}

func validateIdentity(node *yaml.Node, path string, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "identity must be a mapping")
		return
	}
	for _, pair := range mappingPairsWithPath(node, path) {
		switch pair.key {
		case "hostname":
			hostname := strings.TrimSpace(scalarValue(pair.value))
			if !validHostname(hostname) {
				result.add("invalid-hostname", pair.path, fmt.Sprintf("%q must be a DNS hostname label", hostname))
			}
		case "authorizedKeys":
			validateAuthorizedKeys(pair.value, pair.path, result)
		default:
			result.add("unsupported-field", pair.path, "identity field is not supported")
		}
	}
}

func validateAuthorizedKeys(node *yaml.Node, path string, result *Result) {
	if node.Kind != yaml.SequenceNode {
		result.add("invalid-ssh-key", path, "authorizedKeys must be a list")
		return
	}
	if len(node.Content) == 0 {
		result.add("missing-ssh-key", path, "authorizedKeys must contain at least one SSH public key")
		return
	}
	for i, child := range node.Content {
		key := strings.TrimSpace(scalarValue(child))
		if !manifest.ValidAuthorizedKey(key) {
			result.add("invalid-ssh-key", fmt.Sprintf("%s[%d]", path, i), "must be an SSH public key")
		}
	}
}

func validateSystemRole(node *yaml.Node, path string, result *Result) {
	switch strings.TrimSpace(scalarValue(node)) {
	case "control-plane", "worker":
	default:
		result.add("invalid-system-role", path, "systemRole must be control-plane or worker")
	}
}

func validateNetworkd(node *yaml.Node, path string, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "networkd must be a mapping")
		return
	}
	for _, pair := range mappingPairsWithPath(node, path) {
		switch pair.key {
		case "files":
			validateNetworkdFiles(pair.value, pair.path, result)
		default:
			result.add("unsupported-field", pair.path, "networkd field is not supported")
		}
	}
}

func validateNetworkdFiles(node *yaml.Node, path string, result *Result) {
	if node.Kind != yaml.SequenceNode {
		result.add("invalid-field", path, "networkd files must be a list")
		return
	}
	seen := map[string]struct{}{}
	for i, child := range node.Content {
		filePath := fmt.Sprintf("%s[%d]", path, i)
		if child.Kind != yaml.MappingNode {
			result.add("invalid-field", filePath, "networkd file must be a mapping")
			continue
		}
		for _, pair := range mappingPairsWithPath(child, filePath) {
			switch pair.key {
			case "name", "content":
			default:
				result.add("unsupported-field", pair.path, "networkd file field is not supported")
			}
		}
		name := strings.TrimSpace(scalarAt(child, "name"))
		if name == "" {
			result.add("unsafe-render-path", filePath+".name", "networkd file name is required")
			continue
		}
		if err := validateNetworkdName(name); err != nil {
			result.add("unsafe-render-path", filePath+".name", err.Error())
		}
		if _, ok := seen[name]; ok {
			result.add("duplicate-render-path", filePath+".name", fmt.Sprintf("%q duplicates another networkd file", name))
		}
		seen[name] = struct{}{}
	}
}

func validateSysctl(node *yaml.Node, path string, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "sysctl must be a mapping")
		return
	}
	for _, pair := range mappingPairsWithPath(node, path) {
		switch pair.key {
		case "settings":
			validateSysctlSettings(pair.value, pair.path, result)
		default:
			result.add("unsupported-field", pair.path, "sysctl field is not supported")
		}
	}
}

func validateSysctlSettings(node *yaml.Node, path string, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "sysctl settings must be a mapping")
		return
	}
	for _, pair := range mappingPairsWithPath(node, path) {
		key := pair.key
		if key == "" {
			result.add("unsupported-sysctl-key", pair.path, "sysctl key is required")
			continue
		}
		if key != strings.TrimSpace(key) {
			result.add("unsupported-sysctl-key", pair.path, "sysctl key must not contain leading or trailing whitespace")
		}
		if !manifest.ValidSysctlKey(key) {
			result.add("unsupported-sysctl-key", pair.path, "sysctl key is not supported")
		}
		value := scalarValue(pair.value)
		if value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\n\r") {
			result.add("unsafe-sysctl-value", pair.path, "sysctl value is unsafe")
			continue
		}
		if manifest.ValidSysctlKey(key) && !manifest.ValidSysctlValue(key, value) {
			result.add("invalid-sysctl-value", pair.path, manifest.SysctlValueHint(key))
		}
	}
}

func validateKubernetes(node *yaml.Node, path string, options Options, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "kubernetes must be a mapping")
		return
	}
	for _, pair := range mappingPairsWithPath(node, path) {
		switch pair.key {
		case "kubeadm":
			validateKubeadm(pair.value, pair.path, options, result)
		default:
			result.add(unsupportedCode(pair.key), pair.path, "direct Kubernetes sysext/confext activation input is not supported")
		}
	}
}

func validateKubeadm(node *yaml.Node, path string, options Options, result *Result) {
	if node.Kind != yaml.MappingNode {
		result.add("invalid-field", path, "kubeadm must be a mapping")
		return
	}
	for _, pair := range mappingPairsWithPath(node, path) {
		switch pair.key {
		case "configRef":
			ref := strings.TrimSpace(scalarValue(pair.value))
			if !validName(ref) {
				result.add("invalid-kubeadm-ref", pair.path, fmt.Sprintf("%q must be a single DNS label", ref))
				continue
			}
			if options.CheckKubeadmRefs {
				if _, ok := options.KubeadmConfigNames[ref]; !ok {
					result.add("invalid-kubeadm-ref", pair.path, fmt.Sprintf("KubeadmConfig %q was not resolved", ref))
				}
			}
		default:
			result.add("unsupported-field", pair.path, "kubeadm field is not supported")
		}
	}
}

func (r *Result) addDuplicateKeyDiagnostics(node *yaml.Node, path string) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			r.addDuplicateKeyDiagnostics(child, path)
		}
	case yaml.MappingNode:
		seen := map[string]struct{}{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]
			key := keyNode.Value
			field := joinPath(path, key)
			if _, ok := seen[key]; ok {
				code := "duplicate-key"
				message := fmt.Sprintf("mapping key %q is duplicated", key)
				if path == "spec.nodeOverrides" {
					code = "duplicate-node-name"
					message = fmt.Sprintf("node name %q is duplicated", key)
				}
				r.add(code, field, message)
			}
			seen[key] = struct{}{}
			r.addDuplicateKeyDiagnostics(valueNode, field)
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			r.addDuplicateKeyDiagnostics(child, path)
		}
	}
}

func (r *Result) add(code, field, message string) {
	r.Diagnostics = append(r.Diagnostics, Diagnostic{Code: code, Field: field, Message: message})
}

type pair struct {
	key   string
	path  string
	value *yaml.Node
}

func mappingPairs(node *yaml.Node) []pair {
	return mappingPairsWithPath(node, "")
}

func mappingPairsWithPath(node *yaml.Node, path string) []pair {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]pair, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		out = append(out, pair{key: key, path: joinPath(path, key), value: node.Content[i+1]})
	}
	return out
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func scalarAt(node *yaml.Node, key string) string {
	return scalarValue(mappingValue(node, key))
}

func scalarValue(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func documentRoot(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		return node.Content[0]
	}
	return node
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func unsupportedCode(key string) string {
	key = strings.ToLower(key)
	if strings.Contains(key, "sysext") || strings.Contains(key, "confext") || strings.Contains(key, "activation") {
		return "unsupported-activation-input"
	}
	return "unsupported-domain"
}

func validateNetworkdName(name string) error {
	if filepath.IsAbs(name) || name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("%q must be a single render path segment", name)
	}
	switch filepath.Ext(name) {
	case ".network", ".netdev", ".link":
	default:
		return fmt.Errorf("%q must end with .network, .netdev, or .link", name)
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._@+-", r)
		if !ok {
			return fmt.Errorf("%q contains unsupported character %q", name, r)
		}
	}
	return nil
}

func validHostname(value string) bool {
	return validName(value)
}

func validName(value string) bool {
	return manifest.ValidHostname(value)
}
