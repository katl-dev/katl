package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/vmtest"
	"gopkg.in/yaml.v3"
)

func twoNodeInstalledSource(smoke operationBackedSmokeRun, addresses map[string]string) (configbundle.SourceConfig, error) {
	// Preserve installed host and kernel intent while changing Kubernetes input.
	var source configbundle.SourceConfig
	for _, name := range []string{"cp-1", "worker-1"} {
		producer := filepath.Dir(smoke.Inputs.WorldProvenance.FixtureProducerScenarios[name])
		original, err := os.ReadFile(filepath.Join(producer, "inputs", "config-bundle-source", name, "cluster.yaml"))
		if err != nil {
			return source, fmt.Errorf("read %s installed ClusterConfig: %w", name, err)
		}
		installed, err := configbundle.DecodeSource(bytes.NewReader(original))
		if err != nil {
			return source, err
		}
		if name == "cp-1" {
			source = installed
			source.Spec.Nodes = nil
		} else if !reflect.DeepEqual(source.Spec.Defaults, installed.Spec.Defaults) || source.Spec.ManagementAuthentication != installed.Spec.ManagementAuthentication {
			return source, fmt.Errorf("installed node sources disagree on shared host intent")
		}
		found := false
		for _, node := range installed.Spec.Nodes {
			if node.Name == name {
				node.Management.Address = addresses[name]
				source.Spec.Nodes = append(source.Spec.Nodes, node)
				found = true
			}
		}
		if !found {
			return source, fmt.Errorf("installed source does not describe %s", name)
		}
	}
	if source.Spec.ControlPlaneEndpoint == nil {
		return source, fmt.Errorf("installed source has no control-plane endpoint")
	}
	source.Metadata.Name = "two-node"
	source.Spec.ControlPlaneEndpoint.Host = "api.unpublished.katl.test"
	return source, nil
}

func stageBootstrapKubeletInput(t *testing.T, ctx context.Context, smoke operationBackedSmokeRun, addresses map[string]string, bundle threeControlPlaneKubernetesPayloadBundle) error {
	t.Helper()
	dir := filepath.Join(smoke.Result.RunDir, "bootstrap-kubelet")
	if err := os.MkdirAll(filepath.Join(dir, "patches"), 0o700); err != nil {
		return err
	}
	source, err := twoNodeInstalledSource(smoke, addresses)
	if err != nil {
		return err
	}
	source.Spec.Kubernetes.Kubeadm = &configbundle.SourceKubeadmInput{ConfigFile: "kubeadm.yaml", PatchesDir: "patches"}
	source.Spec.Defaults.Kubernetes.Kubelet = nil
	for i := range source.Spec.Nodes {
		source.Spec.Nodes[i].Kubernetes.Kubelet = nil
		if source.Spec.Nodes[i].Name == "cp-1" {
			source.Spec.Nodes[i].Kubernetes.Kubelet = &configbundle.SourceKubeletConfig{ConfigFile: "kubelet.yaml"}
		}
	}
	data, err := yaml.Marshal(source)
	if err != nil {
		return err
	}
	for name, content := range map[string][]byte{
		"cluster.yaml": data,
		"kubeadm.yaml": []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nnetworking:\n  podSubnet: 10.244.0.0/16\n  serviceSubnet: 10.96.0.0/12\n---\napiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\nhealthzPort: 10248\n"),
		"kubelet.yaml": []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nhealthzPort: 0\n"),
		// A second application fails the JSON test; init and join must each
		// apply the ordinary patch exactly once to the unpatched shared base.
		"patches/kubeletconfiguration+json.json": []byte(`[{"op":"test","path":"/maxPods","value":110},{"op":"replace","path":"/maxPods","value":112}]`),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			return err
		}
	}
	management, err := vmtest.VMTestManagementPlanning(vmtest.VMTestManagementClusterName, []string{"cp-1", "worker-1"})
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "cluster.katlcfg")
	if _, err := configbundle.WriteArchive(configPath, configbundle.BuildRequest{SourcePath: filepath.Join(dir, "cluster.yaml"), Planning: configbundle.PlanningInputs{KubernetesBundle: bundle.Ref, ManagementIdentities: management}}); err != nil {
		return err
	}
	katlctl := buildKatlctlCommand(t, ctx, katlRepoRoot(t))
	stdout, stderr, err := runProofKatlctl(ctx, katlctl, dir, "stage", "cluster", "apply", "--config", configPath, "-o", "json")
	if err != nil {
		return fmt.Errorf("stage fresh-bootstrap kubelet input: %w: %s", err, stderr)
	}
	var report struct {
		Result         string
		RebootRequired bool
	}
	if err := json.Unmarshal(stdout, &report); err != nil {
		return err
	}
	if report.Result != "succeeded" || report.RebootRequired {
		return fmt.Errorf("fresh-bootstrap staging did not apply live: %s", stdout)
	}
	for _, name := range []string{"cp-1", "worker-1"} {
		observed, err := readKubeletNode(ctx, smoke.Inputs.SSHPrivateKey, addresses[name], name, "test ! -e /var/lib/kubelet/config.yaml && systemctl show kubelet.service -p ExecMainStartTimestampMonotonic --value")
		if err != nil {
			return fmt.Errorf("%s pre-bootstrap staging wrote live kubelet state: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+"-staged-kubelet-start.txt"), observed, 0o600); err != nil {
			return err
		}
		if strings.TrimSpace(string(observed)) != "0" {
			return fmt.Errorf("%s pre-bootstrap staging started kubelet: %s", name, observed)
		}
	}
	return nil
}

func assertBootstrapKubeletConfig(ctx context.Context, smoke operationBackedSmokeRun, addresses map[string]string, kubeconfig string) error {
	dir := filepath.Join(smoke.Result.RunDir, "bootstrap-kubelet")
	for _, name := range []string{"cp-1", "worker-1"} {
		persisted, err := readKubeletNode(ctx, smoke.Inputs.SSHPrivateKey, addresses[name], name, "cat /var/lib/kubelet/config.yaml")
		if err != nil {
			return err
		}
		configz, err := kubectlOutput(ctx, kubeconfig, "get", "--raw", "/api/v1/nodes/"+name+"/proxy/configz")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name+"-kubelet.yaml"), persisted, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name+"-configz.json"), configz, 0o600); err != nil {
			return err
		}
		var local struct {
			MaxPods     int  `yaml:"maxPods"`
			HealthzPort *int `yaml:"healthzPort"`
		}
		var running struct {
			Kubelet struct {
				MaxPods     int  `json:"maxPods"`
				HealthzPort *int `json:"healthzPort"`
			} `json:"kubeletconfig"`
		}
		if err := yaml.Unmarshal(persisted, &local); err != nil {
			return err
		}
		if err := json.Unmarshal(configz, &running); err != nil {
			return err
		}
		port := 10248
		if name == "cp-1" {
			port = 0
		}
		if local.MaxPods != 112 || running.Kubelet.MaxPods != 112 || local.HealthzPort == nil || *local.HealthzPort != port || running.Kubelet.HealthzPort == nil || *running.Kubelet.HealthzPort != port {
			return fmt.Errorf("%s fresh-bootstrap config differs from native patch plus node overlay: persisted=%s configz=%s", name, persisted, configz)
		}
	}
	shared, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "get", "configmap", "kubelet-config", "-o", "jsonpath={.data.kubelet}")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "shared-kubelet.yaml"), shared, 0o600); err != nil {
		return err
	}
	var base struct {
		MaxPods     int `yaml:"maxPods"`
		HealthzPort int `yaml:"healthzPort"`
	}
	if err := yaml.Unmarshal(shared, &base); err != nil {
		return err
	}
	if base.MaxPods != 110 || base.HealthzPort != 10248 {
		return fmt.Errorf("fresh-bootstrap shared base contains node patches: %s", shared)
	}
	return nil
}
