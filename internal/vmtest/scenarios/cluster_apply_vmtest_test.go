package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/vmtest"
	"gopkg.in/yaml.v3"
)

// Exercise a Kubernetes edit on the already configured host. Success must
// include the dependent kubelet rollout through the ordinary ClusterConfig CLI.
func runPublicClusterApply(t *testing.T, ctx context.Context, smoke threeControlPlaneSmokeRun, result vmtest.Result, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string, kubeconfig string, bundle threeControlPlaneKubernetesPayloadBundle) error {
	t.Helper()
	dir := filepath.Join(result.RunDir, "public-cluster-apply")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sourcePath := filepath.Join(dir, "cluster.yaml")
	configPath := filepath.Join(dir, "cluster.katlcfg")
	// Edit the installed source so omission cannot reset host or kernel intent.
	producer := filepath.Dir(smoke.Inputs.WorldProvenance.FixtureProducerScenarios["cp-1"])
	original, err := os.ReadFile(filepath.Join(producer, "inputs", "config-bundle-source", "cp-1", "cluster.yaml"))
	if err != nil {
		return fmt.Errorf("read installed ClusterConfig: %w", err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(original))
	if err != nil {
		return err
	}
	if len(source.Spec.Nodes) != 1 || source.Spec.Nodes[0].Name != "cp-1" || source.Spec.ControlPlaneEndpoint == nil {
		return fmt.Errorf("installed source must describe cp-1 and its API endpoint")
	}
	source.Metadata.Name = "three-control-plane"
	source.Spec.ControlPlaneEndpoint.Host = "api.unpublished.katl.test"
	source.Spec.Kubernetes.Kubeadm = &configbundle.SourceKubeadmInput{ConfigFile: "kubeadm.yaml"}
	node := source.Spec.Nodes[0]
	source.Spec.Nodes = nil
	for _, name := range []string{"cp-1", "cp-2", "cp-3"} {
		node.Name = name
		node.Management.Address = addresses[name]
		source.Spec.Nodes = append(source.Spec.Nodes, node)
	}
	data, err := yaml.Marshal(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(sourcePath, data, 0o600); err != nil {
		return err
	}
	live, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "get", "configmap", "kubeadm-config", "-o", "jsonpath={.data.ClusterConfiguration}")
	if err != nil {
		return err
	}
	enrollments, err := readThreeControlPlaneEnrollments(ctx, addresses)
	if err != nil {
		return err
	}
	contextPath := filepath.Join(dir, "katlctl.yaml")
	if err := writeThreeControlPlaneWorkstationContext(contextPath, "three-control-plane", addresses, enrollments); err != nil {
		return err
	}
	t.Setenv("KATLCTL_CONFIG", contextPath)
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	katlctl := buildKatlctlCommand(t, ctx, katlRepoRoot(t))
	desired := fmt.Sprintf("%s\n---\napiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 111\n", strings.TrimSpace(string(live)))
	if err := os.WriteFile(filepath.Join(dir, "kubeadm.yaml"), []byte(desired), 0o600); err != nil {
		return err
	}
	management, err := vmtest.VMTestManagementPlanning(vmtest.VMTestManagementClusterName, []string{"cp-1", "cp-2", "cp-3"})
	if err != nil {
		return err
	}
	if _, err := configbundle.WriteArchive(configPath, configbundle.BuildRequest{SourcePath: sourcePath, Planning: configbundle.PlanningInputs{KubernetesBundle: bundle.Ref, ManagementIdentities: management}}); err != nil {
		return err
	}
	apply := func(name string, unchanged bool) error {
		stdout, stderr, err := runProofKatlctl(ctx, katlctl, dir, name, "cluster", "apply", "--config", configPath)
		if err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, stderr)
		}
		var report struct {
			Result         string                     `json:"result"`
			RebootRequired bool                       `json:"rebootRequired"`
			Kubernetes     map[string]json.RawMessage `json:"kubernetes"`
		}
		if err := json.Unmarshal(stdout, &report); err != nil {
			return err
		}
		if report.Result != "succeeded" {
			return fmt.Errorf("%s result: %s", name, stdout)
		}
		if unchanged && len(report.Kubernetes) != 0 {
			return fmt.Errorf("%s repeated Kubernetes operations: %s", name, stdout)
		}
		if report.RebootRequired {
			return fmt.Errorf("%s unexpectedly requires reboot", name)
		}
		return nil
	}
	// Observe kernel identity through operator SSH, independently of apply status.
	bootID := func(node string) (string, error) {
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		command := exec.CommandContext(probe, "ssh", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5", "-i", smoke.Inputs.SSHPrivateKey, "root@"+addresses[node], "cat /proc/sys/kernel/random/boot_id")
		output, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("read %s kernel boot identity: %w", node, err)
		}
		id := strings.TrimSpace(string(output))
		if id == "" {
			return "", fmt.Errorf("%s kernel boot identity is empty", node)
		}
		return id, nil
	}
	before := map[string]string{}
	for _, node := range nodes {
		id, err := bootID(node.Name)
		if err != nil {
			return err
		}
		before[node.Name] = id
	}
	if err := apply("live", false); err != nil {
		return err
	}
	verify := func(node vmtest.RunningInstalledRuntimeNode, unchangedBoot bool) error {
		data, err := kubectlOutput(ctx, kubeconfig, "get", "--raw", "/api/v1/nodes/"+node.Name+"/proxy/configz")
		if err != nil {
			return err
		}
		var config struct {
			Kubelet struct {
				MaxPods int `json:"maxPods"`
			} `json:"kubeletconfig"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			return err
		}
		if config.Kubelet.MaxPods != 111 {
			return fmt.Errorf("%s kubelet maxPods=%d, want 111", node.Name, config.Kubelet.MaxPods)
		}
		id, err := bootID(node.Name)
		if err != nil {
			return err
		}
		if (id == before[node.Name]) != unchangedBoot {
			return fmt.Errorf("unexpected boot identity on %s", node.Name)
		}
		return os.WriteFile(filepath.Join(dir, node.Name+"-configz.json"), data, 0o600)
	}
	for _, node := range nodes {
		if err := verify(node, true); err != nil {
			return err
		}
	}
	if err := apply("repeat", true); err != nil {
		return err
	}
	for _, node := range nodes {
		if err := verify(node, true); err != nil {
			return err
		}
	}
	if _, stderr, err := runProofKatlctl(ctx, katlctl, dir, "reboot-cp-1", "node", "reboot", "cp-1", "--config", configPath); err != nil {
		return fmt.Errorf("reboot cp-1: %w: %s", err, stderr)
	}
	if err := verify(nodeByName(nodes, "cp-1"), false); err != nil {
		return err
	}
	_, err = stageThreeControlPlaneCNIFixtures(ctx, katlRepoRoot(t), nodes, addresses)
	return err
}
