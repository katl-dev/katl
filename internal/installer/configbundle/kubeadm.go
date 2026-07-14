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

const kubeadmCRISocket = "unix:///run/containerd/containerd.sock"

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

	defaults, err := defaultKubeadmDocuments(kubernetesVersion)
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

func defaultKubeadmDocuments(kubernetesVersion string) (map[string]map[string]any, error) {
	controlPlane, err := decodeKubeadmDocuments([]byte(defaultKubeadmInitConfig(kubernetesVersion)))
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
		if value, _ := nodeRegistration["criSocket"].(string); strings.TrimSpace(value) == "" {
			nodeRegistration["criSocket"] = kubeadmCRISocket
		}
		document["nodeRegistration"] = nodeRegistration
	}
	return nil
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
