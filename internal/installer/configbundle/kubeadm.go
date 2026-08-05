package configbundle

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"gopkg.in/yaml.v3"
)

const (
	kubeadmCRISocket      = "unix:///run/containerd/containerd.sock"
	kubeletNodeIPArgument = "node-ip"
)

type kubeadmSourceInput struct {
	Name    string
	Content []byte
}

func resolveKubeadmConfigs(sourceRoot string, input *SourceKubeadmInput, kubernetesVersion string) (map[string]kubeadmconfig.Plan, []kubeadmSourceInput, error) {
	if input == nil {
		configs, err := defaultKubeadmConfigs(kubernetesVersion)
		return configs, nil, err
	}
	configFile := strings.TrimSpace(input.ConfigFile)
	patchesDir := strings.TrimSpace(input.PatchesDir)
	if configFile == "" {
		return nil, nil, fmt.Errorf("spec.kubernetes.kubeadm.configFile is required")
	}
	resolved, err := kubeadmconfig.Resolve(kubeadmconfig.ResolveRequest{
		RepoRoot: filepath.Clean(sourceRoot),
		Object: kubeadmconfig.Object{
			APIVersion: kubeadmconfig.APIVersion,
			Kind:       kubeadmconfig.Kind,
			Metadata:   kubeadmconfig.Metadata{Name: "operator-input"},
			Spec: kubeadmconfig.Spec{
				ConfigFile: configFile,
				PatchesDir: patchesDir,
			},
		},
		KubernetesVersion: kubernetesVersion,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("spec.kubernetes.kubeadm: %w", err)
	}
	plans, err := splitKubeadmPlans(resolved, kubernetesVersion)
	if err != nil {
		return nil, nil, err
	}
	inputs := []kubeadmSourceInput{{Name: configFile, Content: resolved.Config.Content}}
	for _, patch := range resolved.Patches {
		inputs = append(inputs, kubeadmSourceInput{
			Name:    filepath.ToSlash(filepath.Join(patchesDir, filepath.Base(patch.SourcePath))),
			Content: patch.Content,
		})
	}
	return plans, inputs, nil
}

func resolveNodeKubeletConfigs(sourceRoot string, source SourceConfig, base map[string]kubeadmconfig.Plan) (map[string]kubeadmconfig.Plan, []kubeadmSourceInput, error) {
	configs := make(map[string]kubeadmconfig.Plan, len(base)+len(source.Spec.Nodes))
	for name, plan := range base {
		configs[name] = plan
	}
	var inputs []kubeadmSourceInput
	for i, node := range source.Spec.Nodes {
		if node.Kubernetes.Kubelet == nil {
			continue
		}
		field := sourceNodePath(node, i) + ".kubernetes.kubelet.configFile"
		configFile := strings.TrimSpace(node.Kubernetes.Kubelet.ConfigFile)
		data, err := readHostConfigurationSource(sourceRoot, configFile)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", field, err)
		}
		patch, err := kubeletConfigurationPatch(data)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", field, err)
		}
		roleRef := defaultKubeadmConfigRef(sourceNodeRole(node))
		rolePlan, ok := base[roleRef]
		if !ok {
			return nil, nil, fmt.Errorf("%s: base %s kubeadm input is unavailable", field, roleRef)
		}
		ref := nodeKubeadmConfigRef(node.Name)
		plan, err := kubeadmPlanWithNodeKubeletConfig(rolePlan, ref, configFile, patch)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", field, err)
		}
		configs[ref] = plan
		inputs = append(inputs, kubeadmSourceInput{Name: "nodes/" + node.Name + "/kubelet/" + configFile, Content: data})
	}
	return configs, inputs, nil
}

func kubeletConfigurationPatch(data []byte) ([]byte, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode KubeletConfiguration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("decode KubeletConfiguration: multiple YAML documents")
	}
	if version, _ := document["apiVersion"].(string); strings.TrimSpace(version) != "kubelet.config.k8s.io/v1beta1" {
		return nil, fmt.Errorf("apiVersion must be kubelet.config.k8s.io/v1beta1")
	}
	if kind, _ := document["kind"].(string); strings.TrimSpace(kind) != "KubeletConfiguration" {
		return nil, fmt.Errorf("kind must be KubeletConfiguration")
	}
	delete(document, "apiVersion")
	delete(document, "kind")
	if len(document) == 0 {
		return nil, fmt.Errorf("KubeletConfiguration must set at least one kubelet field")
	}
	patch, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode kubelet patch: %w", err)
	}
	return patch, nil
}

func kubeadmPlanWithNodeKubeletConfig(base kubeadmconfig.Plan, name, sourcePath string, patch []byte) (kubeadmconfig.Plan, error) {
	documents, err := decodeKubeadmDocuments(base.Config.Content)
	if err != nil {
		return kubeadmconfig.Plan{}, fmt.Errorf("decode base kubeadm input: %w", err)
	}
	var patchFields map[string]any
	if err := yaml.Unmarshal(patch, &patchFields); err != nil {
		return kubeadmconfig.Plan{}, fmt.Errorf("decode kubelet patch: %w", err)
	}
	foundKubelet := false
	for _, document := range documents {
		if document["kind"] != "KubeletConfiguration" {
			continue
		}
		mergeKubeletFields(document, patchFields)
		foundKubelet = true
		break
	}
	if !foundKubelet {
		return kubeadmconfig.Plan{}, fmt.Errorf("base kubeadm input has no KubeletConfiguration")
	}
	content, err := encodeKubeadmDocuments(documents, name, true)
	if err != nil {
		return kubeadmconfig.Plan{}, err
	}
	files := []kubeadmconfig.File{{
		SourcePath: base.Config.SourcePath,
		RenderPath: "/etc/katl/kubeadm/" + name + "/config.yaml",
		Content:    content,
		Mode:       0o644,
	}}
	basePrefix := "/etc/katl/kubeadm/" + base.Name + "/patches/"
	for _, existing := range base.Patches {
		rel := strings.TrimPrefix(filepath.ToSlash(existing.RenderPath), basePrefix)
		copy := existing
		copy.RenderPath = "/etc/katl/kubeadm/" + name + "/patches/" + rel
		files = append(files, copy)
	}
	const patchName = "kubeletconfiguration999+merge.yaml"
	for _, existing := range files[1:] {
		if filepath.Base(existing.RenderPath) == patchName {
			return kubeadmconfig.Plan{}, fmt.Errorf("cluster kubeadm patches already contain reserved per-node patch name %q", patchName)
		}
	}
	files = append(files, kubeadmconfig.File{
		SourcePath: sourcePath,
		RenderPath: "/etc/katl/kubeadm/" + name + "/patches/" + patchName,
		Content:    patch,
		Mode:       0o644,
	})
	plan, err := kubeadmconfig.PlanFromRenderedFiles(name, files)
	if err != nil {
		return kubeadmconfig.Plan{}, fmt.Errorf("compile per-node kubelet input: %w", err)
	}
	plan.NodeLocalKubelet = true
	return plan, nil
}

func mergeKubeletFields(target, patch map[string]any) {
	for key, value := range patch {
		if value == nil {
			delete(target, key)
			continue
		}
		patchMap, patchIsMap := value.(map[string]any)
		targetMap, targetIsMap := target[key].(map[string]any)
		if patchIsMap && targetIsMap {
			mergeKubeletFields(targetMap, patchMap)
			continue
		}
		target[key] = value
	}
}

func nodeKubeadmConfigRef(nodeName string) string {
	name := "node-" + strings.TrimSpace(nodeName)
	if len(name) <= 63 {
		return name
	}
	digest := strings.TrimPrefix(digestBytes([]byte(nodeName)), "sha256:")[:10]
	return name[:52] + "-" + digest
}

func splitKubeadmPlans(input kubeadmconfig.Plan, kubernetesVersion string) (map[string]kubeadmconfig.Plan, error) {
	documents, err := decodeKubeadmDocuments(input.Config.Content)
	if err != nil {
		return nil, fmt.Errorf("spec.kubernetes.kubeadm.configFile: %w", err)
	}
	byKind := make(map[string]map[string]any, len(documents))
	for index, document := range documents {
		kind, _ := document["kind"].(string)
		if _, exists := byKind[kind]; exists {
			return nil, fmt.Errorf("spec.kubernetes.kubeadm.configFile contains duplicate %s documents", kind)
		}
		if err := rejectManagedNodeAddress(document); err != nil {
			return nil, fmt.Errorf("spec.kubernetes.kubeadm.configFile document %d: %w", index+1, err)
		}
		if kind == "JoinConfiguration" {
			if _, controlPlane := document["controlPlane"]; controlPlane {
				return nil, fmt.Errorf("spec.kubernetes.kubeadm.configFile document %d must not set JoinConfiguration.controlPlane; Katl derives control-plane join credentials and state", index+1)
			}
		}
		if kind == "InitConfiguration" || kind == "JoinConfiguration" {
			if patches, ok := document["patches"].(map[string]any); ok {
				if directory, _ := patches["directory"].(string); strings.TrimSpace(directory) != "" {
					return nil, fmt.Errorf("spec.kubernetes.kubeadm.configFile document %d must omit patches.directory; Katl sets the bundled role-specific path", index+1)
				}
			}
		}
		byKind[kind] = document
	}

	defaults, err := defaultKubeadmDocuments()
	if err != nil {
		return nil, err
	}
	for kind, document := range defaults {
		if _, ok := byKind[kind]; !ok {
			byKind[kind] = document
		}
	}
	if err := provideKubeadmDefaults(byKind, kubernetesVersion); err != nil {
		return nil, err
	}

	commonKinds := []string{"KubeletConfiguration", "KubeProxyConfiguration"}
	controlPlaneDocs := []map[string]any{byKind["InitConfiguration"], byKind["ClusterConfiguration"]}
	workerDocs := []map[string]any{byKind["JoinConfiguration"]}
	for _, kind := range commonKinds {
		if document, ok := byKind[kind]; ok {
			controlPlaneDocs = append(controlPlaneDocs, document)
			workerDocs = append(workerDocs, document)
		}
	}

	plans := make(map[string]kubeadmconfig.Plan, 2)
	for _, role := range []struct {
		name      string
		documents []map[string]any
	}{
		{name: "control-plane", documents: controlPlaneDocs},
		{name: "worker", documents: workerDocs},
	} {
		patches := rolePatches(input, role.name)
		content, err := encodeKubeadmDocuments(role.documents, role.name, len(patches) > 0)
		if err != nil {
			return nil, err
		}
		files := []kubeadmconfig.File{{
			SourcePath: input.Config.SourcePath,
			RenderPath: "/etc/katl/kubeadm/" + role.name + "/config.yaml",
			Content:    content,
			Mode:       0o644,
		}}
		files = append(files, patches...)
		plan, err := kubeadmconfig.PlanFromRenderedFiles(role.name, files)
		if err != nil {
			return nil, fmt.Errorf("compile %s kubeadm input: %w", role.name, err)
		}
		plans[role.name] = plan
	}
	return plans, nil
}

func rejectManagedNodeAddress(document map[string]any) error {
	kind, _ := document["kind"].(string)
	if kind != "InitConfiguration" && kind != "JoinConfiguration" {
		return nil
	}
	if nodeRegistration, ok := document["nodeRegistration"].(map[string]any); ok {
		switch extraArgs := nodeRegistration["kubeletExtraArgs"].(type) {
		case map[string]any:
			if _, exists := extraArgs[kubeletNodeIPArgument]; exists {
				return fmt.Errorf("%s nodeRegistration.kubeletExtraArgs node-ip is supplied from nodes[].kubernetes.address", kind)
			}
		case []any:
			for _, raw := range extraArgs {
				argument, _ := raw.(map[string]any)
				name, _ := argument["name"].(string)
				if strings.TrimSpace(name) == kubeletNodeIPArgument {
					return fmt.Errorf("%s nodeRegistration.kubeletExtraArgs node-ip is supplied from nodes[].kubernetes.address", kind)
				}
			}
		}
	}
	if kind == "InitConfiguration" {
		if endpoint, ok := document["localAPIEndpoint"].(map[string]any); ok {
			if value, exists := endpoint["advertiseAddress"]; exists && strings.TrimSpace(fmt.Sprint(value)) != "" {
				return fmt.Errorf("InitConfiguration localAPIEndpoint.advertiseAddress is supplied from nodes[].kubernetes.address")
			}
		}
	}
	if controlPlane, ok := document["controlPlane"].(map[string]any); ok {
		if endpoint, ok := controlPlane["localAPIEndpoint"].(map[string]any); ok {
			if value, exists := endpoint["advertiseAddress"]; exists && strings.TrimSpace(fmt.Sprint(value)) != "" {
				return fmt.Errorf("JoinConfiguration controlPlane.localAPIEndpoint.advertiseAddress is supplied from nodes[].kubernetes.address")
			}
		}
	}
	return nil
}

func defaultKubeadmDocuments() (map[string]map[string]any, error) {
	controlPlane, err := decodeKubeadmDocuments([]byte(defaultKubeadmInitConfig()))
	if err != nil {
		return nil, err
	}
	worker, err := decodeKubeadmDocuments([]byte(defaultKubeadmJoinConfig()))
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]any, len(controlPlane)+len(worker))
	for _, document := range append(controlPlane, worker...) {
		kind, _ := document["kind"].(string)
		out[kind] = document
	}
	return out, nil
}

func provideKubeadmDefaults(documents map[string]map[string]any, kubernetesVersion string) error {
	cluster := documents["ClusterConfiguration"]
	version, _ := cluster["kubernetesVersion"].(string)
	version = strings.TrimSpace(version)
	if version != "" && version != kubernetesVersion {
		return fmt.Errorf("spec.kubernetes.kubeadm.configFile Kubernetes version %q does not match spec.kubernetes.version %q", version, kubernetesVersion)
	}
	cluster["kubernetesVersion"] = kubernetesVersion
	for _, kind := range []string{"InitConfiguration", "JoinConfiguration"} {
		document := documents[kind]
		nodeRegistration := childMapping(document, "nodeRegistration")
		if value, _ := nodeRegistration["criSocket"].(string); strings.TrimSpace(value) != "" && value != kubeadmCRISocket {
			return fmt.Errorf("spec.kubernetes.kubeadm.configFile %s nodeRegistration.criSocket must be %q on KatlOS", kind, kubeadmCRISocket)
		}
		nodeRegistration["criSocket"] = kubeadmCRISocket
		if kind == "InitConfiguration" {
			if taints, exists := nodeRegistration["taints"]; exists && !emptyKubeadmSequence(taints) {
				return fmt.Errorf("spec.kubernetes.kubeadm.configFile InitConfiguration nodeRegistration.taints must be empty; configure node taints in ClusterConfig")
			}
			nodeRegistration["taints"] = []any{}
		}
		document["nodeRegistration"] = nodeRegistration
	}
	kubelet := documents["KubeletConfiguration"]
	if value, _ := kubelet["volumePluginDir"].(string); strings.TrimSpace(value) != "" && value != kubeadmconfig.KubeletVolumePluginDir {
		return fmt.Errorf("spec.kubernetes.kubeadm.configFile KubeletConfiguration volumePluginDir must be %q on KatlOS", kubeadmconfig.KubeletVolumePluginDir)
	}
	kubelet["volumePluginDir"] = kubeadmconfig.KubeletVolumePluginDir
	return nil
}

func emptyKubeadmSequence(value any) bool {
	if value == nil {
		return true
	}
	sequence, ok := value.([]any)
	return ok && len(sequence) == 0
}

func encodeKubeadmDocuments(documents []map[string]any, role string, hasPatches bool) ([]byte, error) {
	if hasPatches {
		for _, document := range documents {
			kind, _ := document["kind"].(string)
			if kind != "InitConfiguration" && kind != "JoinConfiguration" {
				continue
			}
			patches := childMapping(document, "patches")
			patches["directory"] = "/etc/katl/kubeadm/" + role + "/patches"
			document["patches"] = patches
		}
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	for _, document := range documents {
		if err := encoder.Encode(document); err != nil {
			_ = encoder.Close()
			return nil, fmt.Errorf("encode %s kubeadm input: %w", role, err)
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode %s kubeadm input: %w", role, err)
	}
	return out.Bytes(), nil
}

func decodeKubeadmDocuments(data []byte) ([]map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var documents []map[string]any
	for {
		document := map[string]any{}
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(document) != 0 {
			documents = append(documents, document)
		}
	}
	return documents, nil
}

func childMapping(parent map[string]any, key string) map[string]any {
	child, _ := parent[key].(map[string]any)
	if child == nil {
		child = map[string]any{}
	}
	return child
}

func rolePatches(input kubeadmconfig.Plan, role string) []kubeadmconfig.File {
	base := "/etc/katl/kubeadm/" + input.Name + "/patches/"
	out := make([]kubeadmconfig.File, 0, len(input.Patches))
	for _, patch := range input.Patches {
		rel := strings.TrimPrefix(filepath.ToSlash(patch.RenderPath), base)
		if role == "worker" && !strings.HasPrefix(strings.ToLower(filepath.Base(rel)), "kubeletconfiguration") {
			continue
		}
		copy := patch
		copy.RenderPath = "/etc/katl/kubeadm/" + role + "/patches/" + rel
		out = append(out, copy)
	}
	return out
}
