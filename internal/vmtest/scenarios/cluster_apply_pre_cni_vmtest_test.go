package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/vmtest"
	"gopkg.in/yaml.v3"
)

func runPreCNIClusterApply(t *testing.T, ctx context.Context, smoke operationBackedSmokeRun, addresses map[string]string, kubeconfig string, bundle threeControlPlaneKubernetesPayloadBundle) error {
	t.Helper()
	dir := filepath.Join(smoke.Result.RunDir, "pre-cni-apply")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// Preserve each installed node's intent; omitted host or kernel configuration
	// would turn this kubelet edit into an unrelated host change.
	var source configbundle.SourceConfig
	for _, name := range []string{"cp-1", "worker-1"} {
		producer := filepath.Dir(smoke.Inputs.WorldProvenance.FixtureProducerScenarios[name])
		original, err := os.ReadFile(filepath.Join(producer, "inputs", "config-bundle-source", name, "cluster.yaml"))
		if err != nil {
			return fmt.Errorf("read %s installed ClusterConfig: %w", name, err)
		}
		installed, err := configbundle.DecodeSource(bytes.NewReader(original))
		if err != nil {
			return err
		}
		if name == "cp-1" {
			source = installed
			source.Spec.Nodes = nil
		} else if !reflect.DeepEqual(source.Spec.Defaults, installed.Spec.Defaults) || source.Spec.ManagementAuthentication != installed.Spec.ManagementAuthentication {
			return fmt.Errorf("installed node sources disagree on shared host intent")
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
			return fmt.Errorf("installed source does not describe %s", name)
		}
	}
	if source.Spec.ControlPlaneEndpoint == nil {
		return fmt.Errorf("installed source has no control-plane endpoint")
	}
	source.Metadata.Name = "two-node"
	source.Spec.ControlPlaneEndpoint.Host = "api.unpublished.katl.test"
	source.Spec.Kubernetes.Kubeadm = &configbundle.SourceKubeadmInput{ConfigFile: "kubeadm.yaml"}
	source.Spec.Defaults.Kubernetes.Kubelet = &configbundle.SourceKubeletConfig{ConfigFile: "kubelet.yaml"}
	data, err := yaml.Marshal(source)
	if err != nil {
		return err
	}
	sourcePath := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(sourcePath, data, 0o600); err != nil {
		return err
	}
	live, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "get", "configmap", "kubeadm-config", "-o", "jsonpath={.data.ClusterConfiguration}")
	if err != nil {
		return err
	}
	live, err = disableAPIServerProfiling(live)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "kubeadm.yaml"), live, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "kubelet.yaml"), []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 111\nhealthzPort: 0\n"), 0o600); err != nil {
		return err
	}
	management, err := vmtest.VMTestManagementPlanning(vmtest.VMTestManagementClusterName, []string{"cp-1", "worker-1"})
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "cluster.katlcfg")
	if _, err := configbundle.WriteArchive(configPath, configbundle.BuildRequest{SourcePath: sourcePath, Planning: configbundle.PlanningInputs{KubernetesBundle: bundle.Ref, ManagementIdentities: management}}); err != nil {
		return err
	}

	observeAPIServer := func(label string) (string, bool, error) {
		data, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "get", "pod", "kube-apiserver-cp-1", "-o", "json")
		if err != nil {
			return "", false, err
		}
		if err := os.WriteFile(filepath.Join(dir, label+"-apiserver-pod.json"), data, 0o600); err != nil {
			return "", false, err
		}
		var pod struct {
			Metadata struct{ UID string } `json:"metadata"`
			Spec     struct {
				Containers []struct {
					Name          string
					Command, Args []string
				}
			} `json:"spec"`
		}
		if err := json.Unmarshal(data, &pod); err != nil {
			return "", false, err
		}
		if pod.Metadata.UID == "" {
			return "", false, fmt.Errorf("API server mirror has no identity")
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == "kube-apiserver" {
				return pod.Metadata.UID, slices.Contains(container.Command, "--profiling=false") || slices.Contains(container.Args, "--profiling=false"), nil
			}
		}
		return "", false, fmt.Errorf("API server mirror has no API server container")
	}
	beforeAPIServer, disabled, err := observeAPIServer("before")
	if err != nil {
		return err
	}
	if disabled {
		return fmt.Errorf("fixture already disables API server profiling; apply would not exercise a control-plane change")
	}
	var appliedAPIServer string

	before := map[string]configuredKubelet{}
	for _, name := range []string{"cp-1", "worker-1"} {
		// Node registration can precede CSINode initialization. Establish the
		// sole-CNI baseline through read-only observations before any apply.
		baselineCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		observed, err := waitForConfiguredKubelet(baselineCtx, dir, "before", smoke.Inputs.SSHPrivateKey, addresses[name], name, kubeconfig, 0, false)
		cancel()
		if err != nil {
			return fmt.Errorf("%s did not establish the pre-CNI baseline: %w", name, err)
		}
		if observed.MaxPods == 111 {
			return fmt.Errorf("%s fixture already has the requested maxPods; apply would not exercise a kubelet change", name)
		}
		before[name] = observed
	}
	katlctl := buildKatlctlCommand(t, ctx, katlRepoRoot(t))
	var appliedGenerations map[string]string
	for _, label := range []string{"apply", "repeat"} {
		stdout, stderr, err := runProofKatlctl(ctx, katlctl, dir, label, "cluster", "apply", "--config", configPath, "-o", "json")
		if err != nil {
			return fmt.Errorf("pre-CNI %s: %w: %s", label, err, stderr)
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
		if report.Result != "succeeded" || report.RebootRequired || len(report.NodePlans) != 2 {
			return fmt.Errorf("unexpected pre-CNI %s result: %s", label, stdout)
		}
		if label == "repeat" {
			for _, plan := range report.NodePlans {
				if !plan.NoChanges {
					return fmt.Errorf("unchanged pre-CNI apply planned another host change: %s", stdout)
				}
			}
		}
		apiserver, disabled, err := observeAPIServer(label)
		if err != nil {
			return err
		}
		if !disabled || apiserver == beforeAPIServer {
			return fmt.Errorf("pre-CNI %s did not replace the API server mirror with profiling=false", label)
		}
		if label == "repeat" && apiserver != appliedAPIServer {
			return fmt.Errorf("unchanged pre-CNI apply replaced the API server mirror")
		}
		appliedAPIServer = apiserver
		current, err := readTwoNodeEnrollments(ctx, addresses["cp-1"], addresses["worker-1"])
		if err != nil {
			return err
		}
		generations := map[string]string{}
		for _, name := range []string{"cp-1", "worker-1"} {
			observed, err := observeConfiguredKubelet(ctx, dir, label, smoke.Inputs.SSHPrivateKey, addresses[name], name, kubeconfig, 111, false)
			if err != nil {
				return err
			}
			if observed.BootID != before[name].BootID {
				return fmt.Errorf("%s pre-CNI %s rebooted the node", name, label)
			}
			if label == "repeat" && observed.Start != before[name].Start {
				return fmt.Errorf("%s unchanged pre-CNI apply restarted kubelet", name)
			}
			before[name] = observed
			generations[name] = current[name].GetCurrentGenerationId()
		}
		if label == "repeat" && !reflect.DeepEqual(generations, appliedGenerations) {
			return fmt.Errorf("unchanged pre-CNI apply changed current generations: %v -> %v", appliedGenerations, generations)
		}
		appliedGenerations = generations
	}
	for _, failure := range []struct{ name, input, diagnostic string }{
		{"invalid-policy", "cpuManagerPolicy: katl-invalid\n", "unknown policy"},
		{"checkpoint-policy", "cpuManagerPolicy: static\nreservedSystemCPUs: \"0\"\n", "differs from state checkpoint policy"},
	} {
		// Only the control-plane kubelet is changed, so recovery must pass the
		// unchanged control-plane component before repairing its failed kubelet.
		failedSource := source
		failedSource.Spec.Nodes = append([]configbundle.SourceNode(nil), source.Spec.Nodes...)
		for i := range failedSource.Spec.Nodes {
			if failedSource.Spec.Nodes[i].Name == "cp-1" {
				failedSource.Spec.Nodes[i].Kubernetes.Kubelet = &configbundle.SourceKubeletConfig{ConfigFile: failure.name + ".yaml"}
			}
		}
		badInput := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 111\nhealthzPort: 0\n" + failure.input
		if err := os.WriteFile(filepath.Join(dir, failure.name+".yaml"), []byte(badInput), 0o600); err != nil {
			return err
		}
		badSource, err := yaml.Marshal(failedSource)
		if err != nil {
			return err
		}
		badSourcePath := filepath.Join(dir, failure.name+"-cluster.yaml")
		if err := os.WriteFile(badSourcePath, badSource, 0o600); err != nil {
			return err
		}
		badConfig := filepath.Join(dir, failure.name+".katlcfg")
		if _, err := configbundle.WriteArchive(badConfig, configbundle.BuildRequest{SourcePath: badSourcePath, Planning: configbundle.PlanningInputs{KubernetesBundle: bundle.Ref, ManagementIdentities: management}}); err != nil {
			return err
		}
		if err := runPreCNIKubeletFailure(ctx, katlctl, dir, failure.name, failure.diagnostic, badConfig, configPath, smoke.Inputs.SSHPrivateKey, addresses["cp-1"], kubeconfig); err != nil {
			return err
		}
	}
	apiserver, disabled, err := observeAPIServer("repaired")
	if err != nil {
		return err
	}
	if !disabled || apiserver != appliedAPIServer {
		return fmt.Errorf("kubelet failure/repair changed the desired API server pod")
	}
	return nil
}

func runPreCNIKubeletFailure(ctx context.Context, katlctl, dir, name, diagnostic, badConfig, validConfig, privateKey, address, kubeconfig string) error {
	read := func(command string) ([]byte, error) {
		return readKubeletNode(ctx, privateKey, address, "cp-1", command)
	}
	captureControlPlane := func(phase string) error {
		// NodeLifecycle can mark mirror PodReady false on a pre-CNI node while
		// ContainersReady remains true. Preserve both conditions and native
		// events without requiring observation of that transient window.
		for _, probe := range []struct {
			suffix string
			args   []string
		}{
			{"pods", []string{"get", "pods", "etcd-cp-1", "kube-apiserver-cp-1", "kube-controller-manager-cp-1", "kube-scheduler-cp-1", "-o", "json"}},
			{"events", []string{"get", "events", "--field-selector=involvedObject.kind=Pod", "-o", "json"}},
		} {
			probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			data, err := kubectlOutput(probeCtx, kubeconfig, append([]string{"-n", "kube-system"}, probe.args...)...)
			cancel()
			if err != nil {
				return fmt.Errorf("capture %s %s control-plane %s: %w", name, phase, probe.suffix, err)
			}
			if err := os.WriteFile(filepath.Join(dir, name+"-"+phase+"-control-plane-"+probe.suffix+".json"), data, 0o600); err != nil {
				return err
			}
		}
		return nil
	}
	before, err := observeConfiguredKubelet(ctx, dir, name+"-before", privateKey, address, "cp-1", kubeconfig, 111, false)
	if err != nil {
		return err
	}
	if err := captureControlPlane("before"); err != nil {
		return err
	}
	checkpoint, err := read("cat /var/lib/kubelet/cpu_manager_state")
	if err != nil {
		return err
	}
	var cpuState struct {
		PolicyName string `json:"policyName"`
	}
	if err := json.Unmarshal(checkpoint, &cpuState); err != nil || cpuState.PolicyName != "none" {
		return fmt.Errorf("%s requires the native none-policy checkpoint, got %s: %v", name, checkpoint, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+"-checkpoint-before.json"), checkpoint, 0o600); err != nil {
		return err
	}

	stdout, stderr, applyErr := runProofKatlctl(ctx, katlctl, dir, name, "cluster", "apply", "--config", badConfig, "--node", "cp-1", "-o", "json")
	var failed struct{ Result, NextAction string }
	if err := json.Unmarshal(stdout, &failed); err != nil {
		return fmt.Errorf("%s failure report: %w: %s", name, err, stderr)
	}
	if applyErr == nil || failed.Result != "partial" || strings.TrimSpace(failed.NextAction) == "" {
		return fmt.Errorf("%s did not report incomplete configuration with recovery guidance: %s %s", name, stdout, stderr)
	}

	// Preserve the actual failure before repair. A CLI error alone could merely
	// indicate transport trouble and would not exercise the startup contract.
	for _, probe := range []struct{ suffix, command string }{
		{"service.txt", "systemctl show kubelet -p ActiveState -p SubState -p MainPID -p NRestarts -p InvocationID"},
		{"kubelet.yaml", "cat /var/lib/kubelet/config.yaml"},
		{"journal.txt", "journalctl -u kubelet -n 160 --no-pager"},
		{"checkpoint.json", "cat /var/lib/kubelet/cpu_manager_state"},
	} {
		data, err := read(probe.command)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name+"-failed-"+probe.suffix), data, 0o600); err != nil {
			return err
		}
		if probe.suffix == "journal.txt" && !strings.Contains(string(data), diagnostic) {
			return fmt.Errorf("%s did not reach the intended native startup failure: %s", name, data)
		}
		if probe.suffix == "checkpoint.json" && !bytes.Equal(checkpoint, data) {
			return fmt.Errorf("%s changed the existing CPU-manager checkpoint", name)
		}
	}
	data, err := kubectlOutput(ctx, kubeconfig, "get", "node", "cp-1", "-o", "json")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+"-failed-node.json"), data, 0o600); err != nil {
		return err
	}
	if err := captureControlPlane("failed"); err != nil {
		return err
	}

	var repaired configuredKubelet
	var repairedGeneration string
	for _, phase := range []string{"repair", "repeat"} {
		label := name + "-" + phase
		stdout, stderr, err := runProofKatlctl(ctx, katlctl, dir, label, "cluster", "apply", "--config", validConfig, "--node", "cp-1", "-o", "json")
		if err != nil {
			return fmt.Errorf("%s: %w: %s", label, err, stderr)
		}
		var report struct {
			Result         string
			RebootRequired bool
		}
		if err := json.Unmarshal(stdout, &report); err != nil {
			return err
		}
		if report.Result != "succeeded" || report.RebootRequired {
			return fmt.Errorf("%s unexpected result: %s", label, stdout)
		}
		if err := captureControlPlane(phase); err != nil {
			return err
		}
		current, err := observeConfiguredKubelet(ctx, dir, label, privateKey, address, "cp-1", kubeconfig, 111, false)
		if err != nil {
			return err
		}
		if current.BootID != before.BootID {
			return fmt.Errorf("%s rebooted the node", label)
		}
		if !current.Heartbeat.After(before.Heartbeat) {
			return fmt.Errorf("%s retained the pre-failure Node heartbeat", label)
		}
		retained, err := read("cat /var/lib/kubelet/cpu_manager_state")
		if err != nil {
			return err
		}
		if !bytes.Equal(checkpoint, retained) {
			return fmt.Errorf("%s changed the existing CPU-manager checkpoint", label)
		}
		if phase == "repeat" && current.Start != repaired.Start {
			return fmt.Errorf("%s restarted the repaired kubelet", label)
		}
		status, err := readAgentNodeStatus(ctx, "cp-1", address)
		if err != nil {
			return err
		}
		generationID := status.GetCurrentGenerationId()
		if generationID == "" {
			return fmt.Errorf("%s returned no current generation", label)
		}
		if phase == "repeat" && generationID != repairedGeneration {
			return fmt.Errorf("%s changed the repaired generation", label)
		}
		repaired, repairedGeneration = current, generationID
	}
	return nil
}

func readKubeletNode(ctx context.Context, privateKey, address, node, command string) ([]byte, error) {
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probe, "ssh", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5", "-i", privateKey, "root@"+address, command)
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("observe %s %s: %w", node, command, err)
	}
	return data, nil
}

type configuredKubelet struct {
	BootID    string
	Start     string
	MaxPods   int
	Heartbeat time.Time
}

func waitForConfiguredKubelet(ctx context.Context, dir, label, privateKey, address, node, kubeconfig string, maxPods int, ready bool) (configuredKubelet, error) {
	for attempt := 0; ; attempt++ {
		attemptLabel := fmt.Sprintf("%s-%d", label, attempt)
		observed, err := observeConfiguredKubelet(ctx, dir, attemptLabel, privateKey, address, node, kubeconfig, maxPods, ready)
		if err == nil {
			return observed, nil
		}
		if writeErr := os.WriteFile(filepath.Join(dir, node+"-"+attemptLabel+"-observation-error.txt"), []byte(err.Error()+"\n"), 0o600); writeErr != nil {
			return observed, writeErr
		}
		select {
		case <-ctx.Done():
			return observed, fmt.Errorf("%w (last observation: %v)", ctx.Err(), err)
		case <-time.After(time.Second):
		}
	}
}

func observeConfiguredKubelet(ctx context.Context, dir, label, privateKey, address, node, kubeconfig string, maxPods int, ready bool) (configuredKubelet, error) {
	var observed configuredKubelet
	read := func(command string) ([]byte, error) {
		return readKubeletNode(ctx, privateKey, address, node, command)
	}
	data, err := read("systemctl is-active kubelet.service")
	if err != nil {
		return observed, err
	}
	if strings.TrimSpace(string(data)) != "active" {
		return observed, fmt.Errorf("%s kubelet is not active: %s", node, data)
	}
	data, err = read("cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return observed, err
	}
	observed.BootID = strings.TrimSpace(string(data))
	data, err = read("systemctl show kubelet.service -p ExecMainStartTimestampMonotonic --value")
	if err != nil {
		return observed, err
	}
	observed.Start = strings.TrimSpace(string(data))
	if observed.BootID == "" || observed.Start == "" || observed.Start == "0" {
		return observed, fmt.Errorf("%s has incomplete kernel or kubelet process identity", node)
	}
	data, err = read("systemctl show kubelet.service -p ExecMainStartTimestamp --value")
	if err != nil {
		return observed, err
	}
	started, err := time.Parse("Mon 2006-01-02 15:04:05 MST", strings.TrimSpace(string(data)))
	if err != nil {
		return observed, fmt.Errorf("%s native kubelet start timestamp: %w", node, err)
	}
	identity := fmt.Sprintf("bootID=%s\nExecMainStartTimestampMonotonic=%s\nExecMainStartTimestamp=%s\n", observed.BootID, observed.Start, strings.TrimSpace(string(data)))
	if err := os.WriteFile(filepath.Join(dir, node+"-"+label+"-native-identity.txt"), []byte(identity), 0o600); err != nil {
		return observed, err
	}
	data, err = read("cat /var/lib/kubelet/config.yaml")
	if err != nil {
		return observed, err
	}
	if err := os.WriteFile(filepath.Join(dir, node+"-"+label+"-kubelet.yaml"), data, 0o600); err != nil {
		return observed, err
	}
	var persisted struct {
		MaxPods     int  `yaml:"maxPods"`
		HealthzPort *int `yaml:"healthzPort"`
	}
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		return observed, err
	}
	data, err = kubectlOutput(ctx, kubeconfig, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/configz")
	if err != nil {
		return observed, err
	}
	if err := os.WriteFile(filepath.Join(dir, node+"-"+label+"-configz.json"), data, 0o600); err != nil {
		return observed, err
	}
	var running struct {
		Kubelet struct {
			MaxPods     int  `json:"maxPods"`
			HealthzPort *int `json:"healthzPort"`
		} `json:"kubeletconfig"`
	}
	if err := json.Unmarshal(data, &running); err != nil {
		return observed, err
	}
	observed.MaxPods = running.Kubelet.MaxPods
	if maxPods != 0 && (persisted.MaxPods != maxPods || running.Kubelet.MaxPods != maxPods) {
		return observed, fmt.Errorf("%s maxPods persisted=%d running=%d, want %d", node, persisted.MaxPods, running.Kubelet.MaxPods, maxPods)
	}
	if maxPods != 0 && (persisted.HealthzPort == nil || *persisted.HealthzPort != 0 || running.Kubelet.HealthzPort == nil || *running.Kubelet.HealthzPort != 0) {
		return observed, fmt.Errorf("%s disabled native kubelet healthz was not preserved", node)
	}
	data, err = kubectlOutput(ctx, kubeconfig, "get", "node", node, "-o", "json")
	if err != nil {
		return observed, err
	}
	if err := os.WriteFile(filepath.Join(dir, node+"-"+label+"-node.json"), data, 0o600); err != nil {
		return observed, err
	}
	var reported struct {
		Status struct {
			NodeInfo struct {
				BootID string `json:"bootID"`
			} `json:"nodeInfo"`
			Conditions []struct {
				Type              string
				LastHeartbeatTime time.Time
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &reported); err != nil {
		return observed, err
	}
	if reported.Status.NodeInfo.BootID != observed.BootID {
		return observed, fmt.Errorf("%s Node report belongs to an earlier boot", node)
	}
	for _, condition := range reported.Status.Conditions {
		if condition.Type == "Ready" {
			observed.Heartbeat = condition.LastHeartbeatTime
		}
		if condition.Type == "Ready" && (condition.LastHeartbeatTime.Before(started) || condition.LastHeartbeatTime.After(time.Now())) {
			return observed, fmt.Errorf("%s Node Ready heartbeat %s predates native kubelet start %s or is in the future", node, condition.LastHeartbeatTime, started)
		}
	}
	if !ready {
		cri, err := read("crictl info -o json")
		if err != nil {
			return observed, err
		}
		if err := os.WriteFile(filepath.Join(dir, node+"-"+label+"-cri.json"), cri, 0o600); err != nil {
			return observed, err
		}
		return observed, verifyPreCNIHealth(data, cri)
	}
	var status struct {
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return observed, err
	}
	for _, condition := range status.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			return observed, nil
		}
	}
	return observed, fmt.Errorf("%s Ready condition is not True: %s", node, data)
}

func verifyPreCNIHealth(node, cri []byte) error {
	// These are the native diagnostics independently observed on the installed
	// containerd/Kubernetes fixture, not the agent's configuration-health result.
	const missingCNI = "Network plugin returns error: cni plugin not initialized"
	var status struct {
		Status struct {
			Conditions []struct {
				Type, Status, Reason, Message string
			}
		}
	}
	if err := json.Unmarshal(node, &status); err != nil {
		return err
	}
	readyConditions := 0
	for _, condition := range status.Status.Conditions {
		if condition.Type != "Ready" {
			continue
		}
		readyConditions++
		if condition.Status != "False" || condition.Reason != "KubeletNotReady" || condition.Message != "container runtime network not ready: NetworkReady=false reason:NetworkPluginNotReady message:"+missingCNI {
			return fmt.Errorf("node is not waiting solely for CNI: %s", node)
		}
	}
	if readyConditions != 1 {
		return fmt.Errorf("expected one Node Ready condition, got %d", readyConditions)
	}
	var runtime struct {
		Status struct {
			Conditions []struct {
				Type            string
				Status          *bool
				Reason, Message string
			}
		}
	}
	if err := json.Unmarshal(cri, &runtime); err != nil {
		return err
	}
	runtimeConditions, networkConditions := 0, 0
	for _, condition := range runtime.Status.Conditions {
		switch condition.Type {
		case "RuntimeReady":
			runtimeConditions++
			if condition.Status == nil || !*condition.Status {
				return fmt.Errorf("container runtime is not ready: %s", cri)
			}
		case "NetworkReady":
			networkConditions++
			if condition.Status == nil || *condition.Status || condition.Reason != "NetworkPluginNotReady" || condition.Message != missingCNI {
				return fmt.Errorf("runtime network is not waiting solely for CNI: %s", cri)
			}
		}
	}
	if runtimeConditions != 1 || networkConditions != 1 {
		return fmt.Errorf("expected one RuntimeReady and NetworkReady condition, got %d and %d", runtimeConditions, networkConditions)
	}
	return nil
}

func TestPreCNIHealth(t *testing.T) {
	const node = `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"KubeletNotReady","message":"container runtime network not ready: NetworkReady=false reason:NetworkPluginNotReady message:Network plugin returns error: cni plugin not initialized"}]}}`
	const cri = `{"status":{"conditions":[{"type":"RuntimeReady","status":true},{"type":"NetworkReady","status":false,"reason":"NetworkPluginNotReady","message":"Network plugin returns error: cni plugin not initialized"}]}}`
	for _, tc := range []struct {
		name, node, cri string
		wantError       bool
	}{
		{"sole missing CNI", node, cri, false},
		{"unknown node", strings.Replace(node, `"False"`, `"Unknown"`, 1), cri, true},
		{"additional node failure", strings.Replace(node, `cni plugin not initialized`, `cni plugin not initialized; PLEG is not healthy`, 1), cri, true},
		{"runtime failed", node, strings.Replace(cri, `"status":true`, `"status":false`, 1), true},
		{"different network error", node, strings.Replace(cri, `cni plugin not initialized`, `CNI plugin executable missing`, 1), true},
		{"missing runtime status", node, strings.Replace(cri, `,"status":true`, ``, 1), true},
		{"missing node condition", `{"status":{"conditions":[]}}`, cri, true},
		{"missing CRI conditions", node, `{"status":{"conditions":[]}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyPreCNIHealth([]byte(tc.node), []byte(tc.cri)); (err != nil) != tc.wantError {
				t.Fatalf("health error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}

func disableAPIServerProfiling(data []byte) ([]byte, error) {
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, fmt.Errorf("native ClusterConfiguration is empty")
	}
	api, ok := config["apiServer"].(map[string]any)
	if !ok {
		if config["apiServer"] != nil {
			return nil, fmt.Errorf("native apiServer configuration is not a mapping")
		}
		api = map[string]any{}
		config["apiServer"] = api
	}
	args, ok := api["extraArgs"].([]any)
	if !ok && api["extraArgs"] != nil {
		return nil, fmt.Errorf("native API server extraArgs is not a list")
	}
	found := false
	for _, item := range args {
		arg, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("native API server argument is not a mapping")
		}
		if arg["name"] != "profiling" {
			continue
		}
		if found || arg["value"] == "false" {
			return nil, fmt.Errorf("fixture profiling argument is repeated or already false")
		}
		arg["value"] = "false"
		found = true
	}
	if !found {
		args = append(args, map[string]any{"name": "profiling", "value": "false"})
	}
	api["extraArgs"] = args
	return yaml.Marshal(config)
}

func TestPreCNIControlPlaneInput(t *testing.T) {
	for _, profiling := range []string{"", "  - name: profiling\n    value: \"true\"\n"} {
		t.Run(fmt.Sprintf("profilingPresent=%t", profiling != ""), func(t *testing.T) {
			input := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nnetworking:\n  podSubnet: 10.200.0.0/16\napiServer:\n  extraArgs:\n  - name: audit-log-maxage\n    value: \"30\"\n" + profiling + "  certSANs: [api.example.test]\n"
			data, err := disableAPIServerProfiling([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				APIVersion string `yaml:"apiVersion"`
				Kind       string `yaml:"kind"`
				Networking struct {
					PodSubnet string `yaml:"podSubnet"`
				}
				APIServer struct {
					ExtraArgs []struct{ Name, Value string } `yaml:"extraArgs"`
					CertSANs  []string                       `yaml:"certSANs"`
				} `yaml:"apiServer"`
			}
			if err := yaml.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.APIVersion != "kubeadm.k8s.io/v1beta4" || got.Kind != "ClusterConfiguration" || got.Networking.PodSubnet != "10.200.0.0/16" || !reflect.DeepEqual(got.APIServer.CertSANs, []string{"api.example.test"}) {
				t.Fatalf("native configuration lost: %s", data)
			}
			if len(got.APIServer.ExtraArgs) != 2 || got.APIServer.ExtraArgs[0].Name != "audit-log-maxage" || got.APIServer.ExtraArgs[0].Value != "30" || got.APIServer.ExtraArgs[1].Name != "profiling" || got.APIServer.ExtraArgs[1].Value != "false" {
				t.Fatalf("native arguments lost or not updated: %s", data)
			}
		})
	}
}
