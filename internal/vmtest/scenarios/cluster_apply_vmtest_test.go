package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	source.Spec.Defaults.Kubernetes.Kubelet = &configbundle.SourceKubeletConfig{ConfigFile: "kubelet.yaml"}
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
	if err := os.WriteFile(filepath.Join(dir, "kubeadm.yaml"), live, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "kubelet.yaml"), []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 111\n"), 0o600); err != nil {
		return err
	}
	// Model an existing cluster whose shared config predates Katl's defaults.
	shared, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "get", "configmap", "kubelet-config", "-o", "jsonpath={.data.kubelet}")
	if err != nil {
		return err
	}
	var sharedConfig map[string]any
	if err := yaml.Unmarshal(shared, &sharedConfig); err != nil {
		return err
	}
	delete(sharedConfig, "systemReserved")
	shared, err = yaml.Marshal(sharedConfig)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{"kubelet": string(shared)}})
	if err != nil {
		return err
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "patch", "configmap", "kubelet-config", "--type=merge", "--patch", string(patch)); err != nil {
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
		stdout, stderr, err := runProofKatlctl(ctx, katlctl, dir, name, "cluster", "apply", "--config", configPath, "-o", "json")
		if err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, stderr)
		}
		var report struct {
			Result         string `json:"result"`
			RebootRequired bool   `json:"rebootRequired"`
			NodePlans      []struct {
				NoChanges bool `json:"noChanges"`
			} `json:"nodePlans"`
		}
		if err := json.Unmarshal(stdout, &report); err != nil {
			return err
		}
		if report.Result != "succeeded" {
			return fmt.Errorf("%s result: %s", name, stdout)
		}
		if unchanged {
			for _, plan := range report.NodePlans {
				if !plan.NoChanges {
					return fmt.Errorf("%s created another host generation: %s", name, stdout)
				}
			}
		}
		if report.RebootRequired {
			return fmt.Errorf("%s unexpectedly requires reboot", name)
		}
		return nil
	}
	// Observe kernel identity through operator SSH, independently of apply status.
	observe := func(node, commandText string) (string, error) {
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		command := exec.CommandContext(probe, "ssh", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5", "-i", smoke.Inputs.SSHPrivateKey, "root@"+addresses[node], commandText)
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
	bootID := func(node string) (string, error) { return observe(node, "cat /proc/sys/kernel/random/boot_id") }
	generationIDs := func(node, label string) ([]string, error) {
		data, stderr, err := runProofKatlctl(ctx, katlctl, dir, node+"-generations-"+label, "node", "generations", "list", node, "--config", configPath, "-o", "json")
		if err != nil {
			return nil, fmt.Errorf("list generations: %w: %s", err, stderr)
		}
		var report struct {
			Generations []struct {
				ID string `json:"generationId"`
			} `json:"generations"`
		}
		if err := json.Unmarshal(data, &report); err != nil {
			return nil, err
		}
		var ids []string
		for _, item := range report.Generations {
			ids = append(ids, item.ID)
		}
		slices.Sort(ids)
		return ids, nil
	}
	before := map[string]string{}
	for _, node := range nodes {
		id, err := bootID(node.Name)
		if err != nil {
			return err
		}
		before[node.Name] = id
	}
	// Remove only the shared input, leaving running nodes healthy. Restoring it
	// must make the same desired configuration retryable without another generation.
	restorePath := filepath.Join(dir, "restore-kubelet-config.json")
	restore, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]string{"name": "kubelet-config", "namespace": "kube-system"}, "data": map[string]string{"kubelet": string(shared)}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(restorePath, restore, 0o600); err != nil {
		return err
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "delete", "configmap", "kubelet-config"); err != nil {
		return err
	}
	failedOutput, failedStderr, applyErr := runProofKatlctl(ctx, katlctl, dir, "missing-shared-config", "cluster", "apply", "--config", configPath, "-o", "json")
	if _, err := kubectlOutput(ctx, kubeconfig, "create", "-f", restorePath); err != nil {
		return err
	}
	var failed struct {
		Result     string `json:"result"`
		NextAction string `json:"nextAction"`
	}
	if err := json.Unmarshal(failedOutput, &failed); err != nil {
		return fmt.Errorf("partial report: %w: %s", err, failedStderr)
	}
	if applyErr == nil || failed.Result != "partial" || !strings.Contains(failed.NextAction, "rerun") {
		return fmt.Errorf("missing shared configuration did not report recoverable partial success: %s %s", failedOutput, failedStderr)
	}
	retained := map[string][]string{}
	for _, node := range nodes {
		ids, err := generationIDs(node.Name, "failed")
		if err != nil {
			return err
		}
		retained[node.Name] = ids
	}
	if err := apply("live", true); err != nil {
		return err
	}
	verify := func(node vmtest.RunningInstalledRuntimeNode, unchangedBoot bool) error {
		data, err := kubectlOutput(ctx, kubeconfig, "get", "--raw", "/api/v1/nodes/"+node.Name+"/proxy/configz")
		if err != nil {
			return err
		}
		var config struct {
			Kubelet struct {
				MaxPods        int               `json:"maxPods"`
				SystemReserved map[string]string `json:"systemReserved"`
			} `json:"kubeletconfig"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			return err
		}
		if config.Kubelet.MaxPods != 111 {
			return fmt.Errorf("%s kubelet maxPods=%d, want 111", node.Name, config.Kubelet.MaxPods)
		}
		if config.Kubelet.SystemReserved["memory"] != "1Gi" {
			return fmt.Errorf("%s kubelet systemReserved.memory=%q, want 1Gi", node.Name, config.Kubelet.SystemReserved["memory"])
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
	starts := map[string]string{}
	for _, node := range nodes {
		start, err := observe(node.Name, "systemctl show kubelet.service -p ExecMainStartTimestampMonotonic --value")
		if err != nil {
			return err
		}
		starts[node.Name] = start
	}
	if err := apply("repeat", true); err != nil {
		return err
	}
	for _, node := range nodes {
		ids, err := generationIDs(node.Name, "repeat")
		if err != nil {
			return err
		}
		if !slices.Equal(ids, retained[node.Name]) {
			return fmt.Errorf("%s retry created generations: %v -> %v", node.Name, retained[node.Name], ids)
		}
		start, err := observe(node.Name, "systemctl show kubelet.service -p ExecMainStartTimestampMonotonic --value")
		if err != nil {
			return err
		}
		if start != starts[node.Name] {
			return fmt.Errorf("%s unchanged apply restarted kubelet", node.Name)
		}
	}
	sharedAfter, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "get", "configmap", "kubelet-config", "-o", "jsonpath={.data.kubelet}")
	if err != nil {
		return err
	}
	if !bytes.Equal(sharedAfter, shared) {
		return fmt.Errorf("node-local kubelet apply changed the shared configuration")
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
