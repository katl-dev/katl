package kubeadmconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const kubeletNodeIPArgument = "node-ip"

// WithNodeAddress returns a node-specific kubeadm plan. Kubeadm persists
// nodeRegistration.kubeletExtraArgs into kubeadm-flags.env, so one source of
// node identity configures both kubelet and the local API endpoint.
func WithNodeAddress(plan Plan, address string) (Plan, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return clonePlan(plan), nil
	}
	documents, err := decodeDocuments(plan.Config.Content)
	if err != nil {
		return Plan{}, fmt.Errorf("decode KubeadmConfig %q for node address: %w", plan.Name, err)
	}
	for _, document := range documents {
		kind, _ := document["kind"].(string)
		switch kind {
		case "InitConfiguration", "JoinConfiguration":
			if err := setKubeletNodeIP(document, address); err != nil {
				return Plan{}, fmt.Errorf("KubeadmConfig %q %s: %w", plan.Name, kind, err)
			}
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
	content, err := encodeDocuments(documents)
	if err != nil {
		return Plan{}, fmt.Errorf("encode KubeadmConfig %q for node address: %w", plan.Name, err)
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
		return Plan{}, fmt.Errorf("validate KubeadmConfig %q with node address: %w", plan.Name, err)
	}
	return rendered, nil
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
