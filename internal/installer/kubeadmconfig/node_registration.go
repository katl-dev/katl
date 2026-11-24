package kubeadmconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/manifest"
	"gopkg.in/yaml.v3"
)

const (
	kubeletNodeIPArgument     = "node-ip"
	kubeletNodeLabelsArgument = "node-labels"
)

// NodeRegistration is the node identity and Kubernetes metadata that kubeadm
// registers a node with.
type NodeRegistration struct {
	Address string
	Labels  map[string]string
	Taints  []manifest.NodeTaint
}

// RegistrationFor returns the registration declared by a node's desired
// configuration.
func RegistrationFor(node manifest.NodeConfig) NodeRegistration {
	registration := NodeRegistration{Address: node.Kubernetes.Address}
	if node.Bootstrap != nil {
		registration.Labels = node.Bootstrap.Labels
		registration.Taints = node.Bootstrap.Taints
	}
	return registration
}

// WithNodeRegistration returns a node-specific kubeadm plan. Kubeadm persists
// nodeRegistration.kubeletExtraArgs into kubeadm-flags.env, so one source of
// node identity configures both kubelet and the local API endpoint. Labels and
// taints take effect only when the Node is first registered: kubelet creates
// it with its labels, kubeadm join registers it with its taints, and kubeadm
// init marks the first control plane with them.
func WithNodeRegistration(plan Plan, registration NodeRegistration) (Plan, error) {
	address := strings.TrimSpace(registration.Address)
	if address == "" && len(registration.Labels) == 0 && len(registration.Taints) == 0 {
		return clonePlan(plan), nil
	}
	documents, err := decodeDocuments(plan.Config.Content)
	if err != nil {
		return Plan{}, fmt.Errorf("decode KubeadmConfig %q for node registration: %w", plan.Name, err)
	}
	for _, document := range documents {
		kind, _ := document["kind"].(string)
		if kind != "InitConfiguration" && kind != "JoinConfiguration" {
			continue
		}
		if address != "" {
			if err := setKubeletNodeIP(document, address); err != nil {
				return Plan{}, fmt.Errorf("KubeadmConfig %q %s: %w", plan.Name, kind, err)
			}
		}
		if len(registration.Labels) > 0 {
			if err := replaceKubeletArgument(document, kubeletNodeLabelsArgument, nodeLabelsArgument(registration.Labels)); err != nil {
				return Plan{}, fmt.Errorf("KubeadmConfig %q %s: %w", plan.Name, kind, err)
			}
		}
		if len(registration.Taints) > 0 {
			childMapping(document, "nodeRegistration")["taints"] = kubeadmTaints(registration.Taints)
		}
		if address == "" {
			continue
		}
		switch kind {
		case "InitConfiguration":
			if err := setAdvertiseAddress(childMapping(document, "localAPIEndpoint"), address); err != nil {
				return Plan{}, fmt.Errorf("KubeadmConfig %q InitConfiguration: %w", plan.Name, err)
			}
		case "JoinConfiguration":
			controlPlane, ok := document["controlPlane"].(map[string]any)
			if !ok || controlPlane == nil {
				continue
			}
			if err := setAdvertiseAddress(childMapping(controlPlane, "localAPIEndpoint"), address); err != nil {
				return Plan{}, fmt.Errorf("KubeadmConfig %q JoinConfiguration.controlPlane: %w", plan.Name, err)
			}
		}
	}
	return withDocuments(plan, documents, "for node registration")
}

// WithoutNodeRegistration returns plan without the fields WithNodeRegistration
// owns: node-ip, node-labels, nodeRegistration.taints and advertiseAddress.
// ClusterConfig validation reserves them for Katl. Kubeadm reads them only when
// it registers the node, so after registration they describe node identity and
// metadata, which their own domains plan, rather than kubeadm configuration.
func WithoutNodeRegistration(plan Plan) (Plan, error) {
	documents, err := decodeDocuments(plan.Config.Content)
	if err != nil {
		return Plan{}, fmt.Errorf("decode KubeadmConfig %q without node registration: %w", plan.Name, err)
	}
	for _, document := range documents {
		kind, _ := document["kind"].(string)
		if kind != "InitConfiguration" && kind != "JoinConfiguration" {
			continue
		}
		if nodeRegistration, ok := document["nodeRegistration"].(map[string]any); ok {
			delete(nodeRegistration, "taints")
			if extraArgs, ok := nodeRegistration["kubeletExtraArgs"].([]any); ok {
				nodeRegistration["kubeletExtraArgs"] = slices.DeleteFunc(slices.Clone(extraArgs), func(raw any) bool {
					argument, _ := raw.(map[string]any)
					name, _ := argument["name"].(string)
					name = strings.TrimSpace(name)
					return name == kubeletNodeIPArgument || name == kubeletNodeLabelsArgument
				})
				if len(nodeRegistration["kubeletExtraArgs"].([]any)) == 0 {
					delete(nodeRegistration, "kubeletExtraArgs")
				}
			}
			if len(nodeRegistration) == 0 {
				delete(document, "nodeRegistration")
			}
		}
		endpoint := document
		if kind == "JoinConfiguration" {
			endpoint, _ = document["controlPlane"].(map[string]any)
		}
		if local, ok := endpoint["localAPIEndpoint"].(map[string]any); ok {
			delete(local, "advertiseAddress")
			if len(local) == 0 {
				delete(endpoint, "localAPIEndpoint")
			}
		}
	}
	return withDocuments(plan, documents, "without node registration")
}

func withDocuments(plan Plan, documents []map[string]any, purpose string) (Plan, error) {
	content, err := encodeDocuments(documents)
	if err != nil {
		return Plan{}, fmt.Errorf("encode KubeadmConfig %q %s: %w", plan.Name, purpose, err)
	}
	files := []File{{
		SourcePath: plan.Config.SourcePath,
		RenderPath: plan.Config.RenderPath,
		Content:    content,
		Mode:       plan.Config.Mode,
	}}
	for _, patch := range plan.Patches {
		copy := patch
		copy.Content = slices.Clone(patch.Content)
		files = append(files, copy)
	}
	rendered, err := PlanFromRenderedFiles(plan.Name, files)
	if err != nil {
		return Plan{}, fmt.Errorf("validate KubeadmConfig %q %s: %w", plan.Name, purpose, err)
	}
	rendered.NodeLocalKubelet = plan.NodeLocalKubelet
	rendered.SharedKubeletBase = plan.SharedKubeletBase
	return rendered, nil
}

func nodeLabelsArgument(labels map[string]string) string {
	pairs := make([]string, 0, len(labels))
	for key, value := range labels {
		pairs = append(pairs, key+"="+value)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func kubeadmTaints(taints []manifest.NodeTaint) []any {
	out := make([]any, 0, len(taints))
	for _, taint := range taints {
		entry := map[string]any{"key": taint.Key, "effect": taint.Effect}
		if taint.Value != "" {
			entry["value"] = taint.Value
		}
		out = append(out, entry)
	}
	return out
}

func clonePlan(plan Plan) Plan {
	out := plan
	out.Config.Content = slices.Clone(plan.Config.Content)
	out.Patches = slices.Clone(plan.Patches)
	for i := range out.Patches {
		out.Patches[i].Content = slices.Clone(plan.Patches[i].Content)
	}
	out.Documents = slices.Clone(plan.Documents)
	return out
}

func decodeDocuments(data []byte) ([]map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var documents []map[string]any
	for {
		document := map[string]any{}
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, err
		}
		if len(document) > 0 {
			documents = append(documents, document)
		}
	}
}

func encodeDocuments(documents []map[string]any) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	for _, document := range documents {
		if err := encoder.Encode(document); err != nil {
			_ = encoder.Close()
			return nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func setKubeletNodeIP(document map[string]any, address string) error {
	nodeRegistration := childMapping(document, "nodeRegistration")
	extraArgs, ok := nodeRegistration["kubeletExtraArgs"].([]any)
	if nodeRegistration["kubeletExtraArgs"] != nil && !ok {
		return fmt.Errorf("nodeRegistration.kubeletExtraArgs must use the kubeadm v1beta4 argument list")
	}
	for _, raw := range extraArgs {
		argument, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := argument["name"].(string)
		if strings.TrimSpace(name) != kubeletNodeIPArgument {
			continue
		}
		value := strings.TrimSpace(fmt.Sprint(argument["value"]))
		if value != address {
			return fmt.Errorf("nodeRegistration.kubeletExtraArgs node-ip %q conflicts with node Kubernetes address %q", value, address)
		}
		return nil
	}
	nodeRegistration["kubeletExtraArgs"] = append(extraArgs, map[string]any{
		"name":  kubeletNodeIPArgument,
		"value": address,
	})
	document["nodeRegistration"] = nodeRegistration
	return nil
}

// replaceKubeletArgument makes value the only kubelet argument called name.
// Source validation reserves the argument for Katl, so replacing keeps
// already node-specific input, such as a stored plan, stable.
func replaceKubeletArgument(document map[string]any, name, value string) error {
	nodeRegistration := childMapping(document, "nodeRegistration")
	extraArgs, ok := nodeRegistration["kubeletExtraArgs"].([]any)
	if nodeRegistration["kubeletExtraArgs"] != nil && !ok {
		return fmt.Errorf("nodeRegistration.kubeletExtraArgs must use the kubeadm v1beta4 argument list")
	}
	kept := make([]any, 0, len(extraArgs)+1)
	for _, raw := range extraArgs {
		if argument, ok := raw.(map[string]any); ok {
			if argumentName, _ := argument["name"].(string); strings.TrimSpace(argumentName) == name {
				continue
			}
		}
		kept = append(kept, raw)
	}
	nodeRegistration["kubeletExtraArgs"] = append(kept, map[string]any{"name": name, "value": value})
	return nil
}

func setAdvertiseAddress(localAPIEndpoint map[string]any, address string) error {
	configured := strings.TrimSpace(fmt.Sprint(localAPIEndpoint["advertiseAddress"]))
	if configured != "" && configured != "<nil>" && configured != address {
		return fmt.Errorf("localAPIEndpoint.advertiseAddress %q conflicts with node Kubernetes address %q", configured, address)
	}
	localAPIEndpoint["advertiseAddress"] = address
	return nil
}

func childMapping(parent map[string]any, key string) map[string]any {
	child, _ := parent[key].(map[string]any)
	if child == nil {
		child = map[string]any{}
		parent[key] = child
	}
	return child
}
