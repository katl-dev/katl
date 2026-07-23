package kubeadmplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

func CanonicalClusterConfigurationSHA256(data []byte) (string, error) {
	return CanonicalDocumentSHA256(data, "kubeadm.k8s.io/v1beta4", "ClusterConfiguration")
}

func CanonicalKubeletConfigurationSHA256(data []byte) (string, error) {
	config, err := configurationDocument(data, "kubelet.config.k8s.io/v1beta1", "KubeletConfiguration")
	if err != nil {
		return "", err
	}
	normalizeKubeletDurationValues(config)
	canonical, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func CanonicalKubeProxyConfigurationSHA256(data []byte) (string, error) {
	config, err := configurationDocumentByKind(data, "KubeProxyConfiguration")
	if err != nil {
		return "", err
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func CanonicalDocumentSHA256(data []byte, apiVersion, kind string) (string, error) {
	config, err := configurationDocument(data, apiVersion, kind)
	if err != nil {
		return "", err
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func KubeletConfigurationContains(actual, desired []byte) error {
	actualConfig, err := configurationDocument(actual, "kubelet.config.k8s.io/v1beta1", "KubeletConfiguration")
	if err != nil {
		return fmt.Errorf("actual: %w", err)
	}
	desiredConfig, err := configurationDocument(desired, "kubelet.config.k8s.io/v1beta1", "KubeletConfiguration")
	if err != nil {
		return fmt.Errorf("desired: %w", err)
	}
	if endpoint, ok := desiredConfig["containerRuntimeEndpoint"].(string); ok && endpoint == "" {
		delete(actualConfig, "containerRuntimeEndpoint")
		delete(desiredConfig, "containerRuntimeEndpoint")
	}
	normalizeKubeletDurationValues(actualConfig)
	normalizeKubeletDurationValues(desiredConfig)
	if !containsValue(actualConfig, desiredConfig) {
		return fmt.Errorf("live KubeletConfiguration does not contain the desired fields")
	}
	return nil
}

func normalizeKubeletDurationValues(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			typed[key] = normalizeKubeletDurationValues(child)
		}
	case []any:
		for i, child := range typed {
			typed[i] = normalizeKubeletDurationValues(child)
		}
	case string:
		if duration, err := time.ParseDuration(typed); err == nil {
			return duration.String()
		}
	}
	return value
}

func KubeProxyConfigurationContains(actual, desired []byte) error {
	actualConfig, err := configurationDocumentByKind(actual, "KubeProxyConfiguration")
	if err != nil {
		return fmt.Errorf("actual: %w", err)
	}
	desiredConfig, err := configurationDocumentByKind(desired, "KubeProxyConfiguration")
	if err != nil {
		return fmt.Errorf("desired: %w", err)
	}
	if !containsValue(actualConfig, desiredConfig) {
		return fmt.Errorf("live KubeProxyConfiguration does not contain the desired fields")
	}
	return nil
}

func containsValue(actual, desired any) bool {
	switch desiredValue := desired.(type) {
	case map[string]any:
		actualValue, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for key, desiredChild := range desiredValue {
			actualChild, exists := actualValue[key]
			if !exists || !containsValue(actualChild, desiredChild) {
				return false
			}
		}
		return true
	case []any:
		actualValue, ok := actual.([]any)
		return ok && reflect.DeepEqual(actualValue, desiredValue)
	default:
		return reflect.DeepEqual(actual, desired)
	}
}

var profilingComponents = []string{"apiServer", "controllerManager", "scheduler"}

var controlPlaneManifestFields = []string{"extraArgs", "extraEnvs", "extraVolumes"}

func SupportedControlPlaneComponentDelta(desired, live []byte) ([]string, error) {
	desiredConfig, err := clusterConfiguration(desired)
	if err != nil {
		return nil, fmt.Errorf("desired: %w", err)
	}
	liveConfig, err := clusterConfiguration(live)
	if err != nil {
		return nil, fmt.Errorf("live: %w", err)
	}
	var delta []string
	for _, component := range profilingComponents {
		desiredSection, err := configurationSection(desiredConfig, component)
		if err != nil {
			return nil, err
		}
		liveSection, err := configurationSection(liveConfig, component)
		if err != nil {
			return nil, err
		}
		for _, field := range controlPlaneManifestFields {
			desiredValue, desiredSet := desiredSection[field]
			liveValue, liveSet := liveSection[field]
			if desiredSet != liveSet || !reflect.DeepEqual(desiredValue, liveValue) {
				delta = append(delta, "ClusterConfiguration."+component+"."+field)
			}
			delete(desiredSection, field)
			delete(liveSection, field)
		}
		removeEmptySection(desiredConfig, component, desiredSection)
		removeEmptySection(liveConfig, component, liveSection)
	}
	if !containsValue(liveConfig, desiredConfig) {
		return nil, fmt.Errorf("unsupported ClusterConfiguration difference outside control-plane manifest fields")
	}
	sort.Strings(delta)
	return delta, nil
}

func EffectiveControlPlaneConfiguration(desired, live []byte) ([]byte, []string, error) {
	delta, err := SupportedControlPlaneComponentDelta(desired, live)
	if err != nil {
		return nil, nil, err
	}
	desiredConfig, err := clusterConfiguration(desired)
	if err != nil {
		return nil, nil, fmt.Errorf("desired: %w", err)
	}
	effectiveConfig, err := clusterConfiguration(live)
	if err != nil {
		return nil, nil, fmt.Errorf("live: %w", err)
	}
	for _, component := range profilingComponents {
		desiredSection, err := configurationSection(desiredConfig, component)
		if err != nil {
			return nil, nil, err
		}
		effectiveSection, err := configurationSection(effectiveConfig, component)
		if err != nil {
			return nil, nil, err
		}
		for _, field := range controlPlaneManifestFields {
			value, set := desiredSection[field]
			if set {
				effectiveSection[field] = value
			} else {
				delete(effectiveSection, field)
			}
		}
		if len(effectiveSection) == 0 {
			delete(effectiveConfig, component)
		} else {
			effectiveConfig[component] = effectiveSection
		}
	}

	decoder := yaml.NewDecoder(bytes.NewReader(desired))
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	found := false
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = encoder.Close()
			return nil, nil, err
		}
		if document["apiVersion"] == "kubeadm.k8s.io/v1beta4" && document["kind"] == "ClusterConfiguration" {
			document = effectiveConfig
			found = true
		}
		if err := encoder.Encode(document); err != nil {
			_ = encoder.Close()
			return nil, nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, fmt.Errorf("kubeadm.k8s.io/v1beta4 ClusterConfiguration is required")
	}
	return out.Bytes(), delta, nil
}

func configurationSection(config map[string]any, name string) (map[string]any, error) {
	value, ok := config[name]
	if !ok {
		return map[string]any{}, nil
	}
	section, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a mapping", name)
	}
	return section, nil
}

func removeEmptySection(config map[string]any, name string, section map[string]any) {
	if len(section) == 0 {
		delete(config, name)
	}
}

func SupportedControlPlaneProfilingDelta(desired, live []byte) ([]string, error) {
	desiredConfig, err := clusterConfiguration(desired)
	if err != nil {
		return nil, fmt.Errorf("desired: %w", err)
	}
	liveConfig, err := clusterConfiguration(live)
	if err != nil {
		return nil, fmt.Errorf("live: %w", err)
	}
	var delta []string
	for _, component := range profilingComponents {
		desiredValue, desiredSet, err := removeProfiling(desiredConfig, component)
		if err != nil {
			return nil, err
		}
		liveValue, liveSet, err := removeProfiling(liveConfig, component)
		if err != nil {
			return nil, err
		}
		if liveSet || !desiredSet {
			if liveSet != desiredSet || liveValue != desiredValue {
				return nil, fmt.Errorf("unsupported profiling transition for %s", component)
			}
			continue
		}
		if desiredValue != "false" {
			return nil, fmt.Errorf("%s profiling value must be false", component)
		}
		delta = append(delta, "ClusterConfiguration."+component+".extraArgs.profiling=false")
	}
	if !reflect.DeepEqual(desiredConfig, liveConfig) {
		return nil, fmt.Errorf("unsupported ClusterConfiguration difference outside profiling=false")
	}
	sort.Strings(delta)
	if len(delta) == 0 {
		return nil, fmt.Errorf("no supported profiling=false additions")
	}
	return delta, nil
}

func clusterConfiguration(data []byte) (map[string]any, error) {
	return configurationDocument(data, "kubeadm.k8s.io/v1beta4", "ClusterConfiguration")
}

func configurationDocument(data []byte, apiVersion, kind string) (map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if doc["apiVersion"] == apiVersion && doc["kind"] == kind {
			return doc, nil
		}
	}
	return nil, fmt.Errorf("%s %s is required", apiVersion, kind)
}

func configurationDocumentByKind(data []byte, kind string) (map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if doc["kind"] == kind {
			return doc, nil
		}
	}
	return nil, fmt.Errorf("%s is required", kind)
}

func removeProfiling(config map[string]any, component string) (string, bool, error) {
	value, ok := config[component]
	if !ok {
		return "", false, nil
	}
	section, ok := value.(map[string]any)
	if !ok {
		return "", false, fmt.Errorf("%s must be a mapping", component)
	}
	raw, ok := section["extraArgs"]
	if !ok {
		if len(section) == 0 {
			delete(config, component)
		}
		return "", false, nil
	}
	args, ok := raw.([]any)
	if !ok {
		return "", false, fmt.Errorf("%s.extraArgs must be a list", component)
	}
	filtered := make([]any, 0, len(args))
	found := ""
	for _, rawArg := range args {
		arg, ok := rawArg.(map[string]any)
		if !ok {
			return "", false, fmt.Errorf("%s.extraArgs entry must be a mapping", component)
		}
		if arg["name"] == "profiling" {
			if found != "" {
				return "", false, fmt.Errorf("%s profiling argument is repeated", component)
			}
			text, ok := arg["value"].(string)
			if !ok {
				return "", false, fmt.Errorf("%s profiling value must be a string", component)
			}
			found = text
			continue
		}
		filtered = append(filtered, arg)
	}
	if len(filtered) == 0 {
		delete(section, "extraArgs")
	} else {
		section["extraArgs"] = filtered
	}
	if len(section) == 0 {
		delete(config, component)
	}
	return found, found != "", nil
}
