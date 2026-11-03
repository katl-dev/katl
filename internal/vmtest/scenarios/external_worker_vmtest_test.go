package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/vmtest"
	"gopkg.in/yaml.v3"
)

func TestExternalWorkerJoin(t *testing.T) {
	if strings.TrimSpace(os.Getenv(vmtest.WorldManifestEnv)) == "" {
		t.Skip("run through scripts/vmtest-run to exercise external worker enrollment")
	}
	const name = "external-worker-join"
	world := vmtest.RequireWorld(t)
	// Keep external fixture producers separate from the managed two-node journey.
	world.ScenarioDir = filepath.Join(world.ScenarioDir, "external-worker")
	world.CacheDir = filepath.Join(world.RunDir, "external-worker-fixtures")
	repo := katlRepoRoot(t)
	kvm := vmtest.DefaultOptions().KVM
	version := operationBackedKubernetesVersion(t, repo)

	if err := ensurePublishedRuntimeFixturesForWorld(world, repo, []vmtest.NodeSpec{{Name: "cp-1", Role: vmtest.ControlPlane}}, kvm); err != nil {
		failWorldFixtureSetup(t, world, name, err)
	}
	scenario, err := world.PlanScenario(name)
	if err != nil {
		t.Fatal(err)
	}
	options := vmtest.Options{Enabled: true, StateRoot: filepath.Join(scenario.Dir, "vm-runs"), Keep: vmtest.KeepFailed, KVM: kvm, Missing: vmtest.MissingFails}
	runner := vmtest.NewRunner(options)
	vmScenario := vmtest.Scenario{Name: name}
	result, err := runner.Plan(vmScenario)
	if err != nil {
		t.Fatal(err)
	}
	result.Started = time.Now().UTC()
	smoke := operationBackedSmokeRun{WorldScenario: scenario, Options: options, Runner: runner, Scenario: vmScenario, Result: result, LibvirtURI: world.Libvirt.URI, Network: world.Libvirt.Network, Inputs: operationBackedSmokeInputs{KubernetesVersion: version}}
	runExternalWorkerJoin(t, smoke)
}

func installExternalWorkerFixture(world vmtest.World, repo, cpAddress, version string, kvm vmtest.KVMPolicy) error {
	input := vmtest.DefaultFirstInstallWorldInputFromEnv(vmtest.FirstInstallWorldPreseed, katlctlEnvBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP"))
	_, key, err := ensureWorldSSHKey(world)
	if err != nil {
		return err
	}
	input.SSHAuthorizedKey, input.KubernetesVersion = key, version
	spec := vmtest.NodeSpec{Name: "worker-1", Role: vmtest.Worker}
	contract, err := vmtest.FirstInstallRuntimeFixtureContractForWorld(world, repo, spec, input, kvm)
	if err != nil {
		return err
	}
	sourcePath := filepath.Join(contract.WorldScenario.Dir, "inputs/config-bundle-source/worker-1/cluster.yaml")
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		return err
	}
	// Fixture planning recreates producer inputs. Keep the desired external
	// source outside that directory for installation and the later public join.
	sourcePath = filepath.Join(world.RunDir, "external-worker-inputs", "cluster.yaml")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		return err
	}
	source.Spec.ControlPlaneEndpoint = nil
	source.Spec.Kubernetes.External = &configbundle.SourceExternalKubernetes{Endpoint: "https://" + net.JoinHostPort(cpAddress, "6443")}
	source.Spec.Kubernetes.Kubeadm = nil
	source.Spec.Defaults.Kubernetes.Kubelet = &configbundle.SourceKubeletConfig{ConfigFile: "kubelet.yaml"}
	source.Spec.Nodes = slices.DeleteFunc(source.Spec.Nodes, func(node configbundle.SourceNode) bool { return node.Name != spec.Name })
	if len(source.Spec.Nodes) != 1 {
		return fmt.Errorf("external fixture must contain only worker-1")
	}
	data, err = yaml.Marshal(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(sourcePath, data, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(sourcePath), "kubelet.yaml"), []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nclusterDNS: [10.96.0.10]\nclusterDomain: cluster.local\nmaxPods: 87\n"), 0o600); err != nil {
		return err
	}
	data, err = os.ReadFile(contract.ManifestPath)
	if err != nil {
		return err
	}
	installed, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	imagePath, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(contract.ManifestPath), installed.KatlosImage.LocalRef))
	if err != nil {
		return err
	}
	// The staged image is removed with its producer; retain the file itself.
	if err := os.Link(imagePath, filepath.Join(filepath.Dir(sourcePath), installed.KatlosImage.LocalRef)); err != nil {
		return err
	}
	management, err := vmtest.VMTestManagementPlanning(vmtest.VMTestManagementClusterName, []string{spec.Name})
	if err != nil {
		return err
	}
	input.ConfigBundle = filepath.Join(filepath.Dir(sourcePath), "external-worker.katlcfg")
	if _, err := configbundle.WriteArchive(input.ConfigBundle, configbundle.BuildRequest{SourcePath: sourcePath, Planning: configbundle.PlanningInputs{KatlosImage: installed.KatlosImage, ManagementIdentities: management}}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	return vmtest.EnsurePublishedFirstInstallRuntimeFixtures(ctx, world, repo, []vmtest.NodeSpec{spec}, vmtest.FirstInstallRuntimeFixtureOptions{Input: input, KVM: kvm, RequireInstallerProvenance: true})
}

func runExternalWorkerJoin(t *testing.T, smoke operationBackedSmokeRun) {
	t.Helper()
	requireVMHost(t, smoke.Runner, smoke.Scenario, smoke.Result, vmtest.HostRequirements{Libvirt: true, OVMF: true, KVM: smoke.Options.KVM})
	requireSmokePrereqs(t, smoke.Runner, smoke.Scenario, smoke.Result, "external worker prerequisites missing", twoNodeHostToolPrereqs(exec.LookPath))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	repo, result, inputs := katlRepoRoot(t), smoke.Result, smoke.Inputs
	var nodes []vmtest.RunningInstalledRuntimeNode
	kubeconfig := filepath.Join(result.RunDir, "operator-kubeconfig.yaml")
	defer func() {
		status, failure := vmtest.StatusPassed, ""
		if t.Failed() {
			status, failure = vmtest.StatusFailed, "external worker journey failed; inspect command transcripts and node diagnostics"
			collectKubectlDiagnostics(kubeconfig, result.RunDir)
			collectTwoNodeDiagnostics("", nodes...)
		}
		for _, node := range nodes {
			stopNode(t, node)
		}
		finishTwoNodeResult(t, smoke.Runner, smoke.Scenario, result, status, failure)
	}()
	addresses := map[string]string{}
	world := smoke.WorldScenario.World
	for _, spec := range []vmtest.NodeSpec{{Name: "cp-1", Role: vmtest.ControlPlane}, {Name: "worker-1", Role: vmtest.Worker}} {
		if spec.Name == "worker-1" {
			// The external endpoint is the running control plane's observed DHCP
			// address, not the world's logical planning address.
			if err := installExternalWorkerFixture(world, repo, addresses["cp-1"], inputs.KubernetesVersion, smoke.Options.KVM); err != nil {
				t.Fatal(err)
			}
		}
		fixture, err := vmtest.AddPublishedInstalledRuntimeNodeFromBuildRoots(smoke.WorldScenario, publishedRuntimeBuildRoots(world, repo), spec)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Config.VM = operationBackedVMConfigForRun(smoke, fixture.Node.MACAddress, 0)
		running, err := vmtest.StartInstalledRuntimeNode(ctx, result, vmtest.InstalledRuntimeNodeConfig{Name: spec.Name, Runtime: fixture.Config}, vmtest.VMRunner{})
		if err != nil {
			t.Fatalf("start %s: %v", spec.Name, err)
		}
		nodes = append(nodes, running)
		addresses[spec.Name], err = liveNodeIPv4Address(ctx, running, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := waitForKatlcAgentTCP(ctx, spec.Name, addresses[spec.Name], 2*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	cp, worker := nodes[0], nodes[1]
	bundle, server, err := stageOperationBackedKubernetesPayloadBundle(repo, result, smoke.WorldScenario.World.Network.Gateway, inputs.KubernetesVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, node := range nodes {
		if err := installKubernetesBundleCA(ctx, node, bundle); err != nil {
			t.Fatal(err)
		}
		if err := waitForKatlcAgentTCP(ctx, node.Name, addresses[node.Name], 2*time.Minute); err != nil {
			t.Fatal(err)
		}
		if observed, err := liveNodeIPv4Address(ctx, node, addresses[node.Name]); err != nil || observed != addresses[node.Name] {
			t.Fatalf("%s runtime address: got %s, expected %s: %v", node.Name, observed, addresses[node.Name], err)
		}
	}
	enrollments, err := readTwoNodeEnrollments(ctx, addresses["cp-1"], addresses["worker-1"])
	if err != nil {
		t.Fatal(err)
	}
	contextPath := filepath.Join(result.RunDir, "katlctl.yaml")
	if err := writeTwoNodeWorkstationContext(contextPath, addresses, enrollments); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KATLCTL_CONFIG", contextPath)
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	if _, err := stageTwoNodeCNIFixtures(ctx, repo, cp, worker, addresses["cp-1"], addresses["worker-1"]); err != nil {
		t.Fatal(err)
	}
	if _, err := stageKubernetesImageFixtures(ctx, repo, inputs.KubernetesVersion, nodes...); err != nil {
		t.Fatal(err)
	}
	if _, err := stageTwoNodeImageFixtures(ctx, repo, result.RunDir, nodes...); err != nil {
		t.Fatal(err)
	}
	configPaths := map[string]string{}
	for _, node := range nodes {
		producer := filepath.Join(world.ScenarioDir, vmtest.FirstInstallRuntimeFixtureScenarioName(vmtest.NodeSpec{Name: node.Name, Role: vmtest.ControlPlane}))
		sourcePath := filepath.Join(producer, "inputs/config-bundle-source", node.Name, "cluster.yaml")
		if node.Name == "worker-1" {
			sourcePath = filepath.Join(smoke.WorldScenario.World.RunDir, "external-worker-inputs", "cluster.yaml")
		}
		data, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		source, err := configbundle.DecodeSource(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		source.Metadata.Name = "two-node"
		source.Spec.Nodes[0].Management.Address = addresses[node.Name]
		if node.Name == "cp-1" {
			source.Spec.ControlPlaneEndpoint.Host = addresses[node.Name]
			source.Spec.Kubernetes.Kubeadm = &configbundle.SourceKubeadmInput{ConfigFile: "kubeadm.yaml"}
			config := fmt.Sprintf("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nnodeRegistration:\n  taints: []\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: %s\nnetworking:\n  podSubnet: 10.244.0.0/16\n  serviceSubnet: 10.96.0.0/12\n", inputs.KubernetesVersion)
			if err := os.WriteFile(filepath.Join(filepath.Dir(sourcePath), "kubeadm.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		data, err = yaml.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sourcePath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		management, err := vmtest.VMTestManagementPlanning(vmtest.VMTestManagementClusterName, []string{node.Name})
		if err != nil {
			t.Fatal(err)
		}
		configPaths[node.Name] = filepath.Join(result.RunDir, node.Name+".katlcfg")
		if _, err := configbundle.WriteArchive(configPaths[node.Name], configbundle.BuildRequest{SourcePath: sourcePath, Planning: configbundle.PlanningInputs{KubernetesBundle: bundle.Ref, ManagementIdentities: management}}); err != nil {
			t.Fatal(err)
		}
	}
	katlctl := buildKatlctlCommand(t, ctx, repo)
	command := func(name string, args ...string) []byte {
		t.Helper()
		output, stderr, err := runProofKatlctl(ctx, katlctl, result.RunDir, name, args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s\n%s", name, err, output, stderr)
		}
		return output
	}
	command("configure-control-plane", "cluster", "apply", "--config", configPaths["cp-1"])
	command("bootstrap-control-plane", "cluster", "bootstrap", "--config", configPaths["cp-1"], "--init-node", "cp-1", "--kubeconfig-out", kubeconfig)
	command("configure-external-worker", "cluster", "apply", "--config", configPaths["worker-1"])
	if err := portableExternalWorkerProxy(ctx, kubeconfig, result.RunDir, addresses["cp-1"]); err != nil {
		t.Fatal(err)
	}
	token, err := runNodeCommand(ctx, cp, []string{"/usr/bin/kubeadm", "token", "create", "--ttl=30m"}, 16<<10)
	if err != nil || token.ExitStatus != 0 {
		t.Fatalf("create restricted bootstrap identity: %v", err)
	}
	ca, err := kubectlOutput(ctx, kubeconfig, "config", "view", "--raw", "--minify", "-o", "jsonpath={.clusters[0].cluster.certificate-authority-data}")
	if err != nil {
		t.Fatal(err)
	}
	if len(ca) == 0 {
		t.Fatal("exported operator kubeconfig has no embedded cluster CA")
	}
	bootstrapPath := filepath.Join(result.RunDir, "worker-bootstrap.conf")
	bootstrap := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: external\n  cluster:\n    server: https://%s\n    certificate-authority-data: %s\nusers:\n- name: bootstrap\n  user:\n    token: %s\ncontexts:\n- name: bootstrap\n  context:\n    cluster: external\n    user: bootstrap\ncurrent-context: bootstrap\n", net.JoinHostPort(addresses["cp-1"], "6443"), ca, strings.TrimSpace(string(token.Stdout)))
	if err := os.WriteFile(bootstrapPath, []byte(bootstrap), 0o600); err != nil {
		t.Fatal(err)
	}
	// Withhold both kubeadm metadata and Katl management access: the worker must
	// depend only on the external Kubernetes API and its bootstrap identity.
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "delete", "configmap", "kubeadm-config", "kubelet-config"); err != nil {
		t.Fatal(err)
	}
	stopped, err := runNodeCommand(ctx, cp, []string{"systemctl", "stop", "katlc-agent.service"}, 16<<10)
	if err != nil || stopped.ExitStatus != 0 {
		t.Fatalf("withhold control-plane management: %v", err)
	}
	command("join-external-worker", "node", "join", "worker-1", "--config", configPaths["worker-1"], "--bootstrap-kubeconfig", bootstrapPath)
	before, err := readAgentNodeStatus(ctx, "worker-1", addresses["worker-1"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitForKubectlNodes(ctx, kubeconfig, filepath.Join(result.RunDir, "nodes.txt"), 3*time.Minute, "node/cp-1", "node/worker-1"); err != nil {
		t.Fatal(err)
	}
	capacity, err := kubectlOutput(ctx, kubeconfig, "get", "node", "worker-1", "-o", "jsonpath={.status.capacity.pods}")
	if err != nil || string(capacity) != "87" {
		t.Fatalf("native worker kubelet setting not observed: %s: %v", capacity, err)
	}
	if err := os.Remove(bootstrapPath); err != nil {
		t.Fatal(err)
	}
	tokenID, _, _ := strings.Cut(strings.TrimSpace(string(token.Stdout)), ".")
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "delete", "secret", "bootstrap-token-"+tokenID); err != nil {
		t.Fatal(err)
	}
	command("repeat-join", "node", "join", "worker-1", "--config", configPaths["worker-1"])
	command("apply-external-worker", "cluster", "apply", "--config", configPaths["worker-1"])
	after, err := readAgentNodeStatus(ctx, "worker-1", addresses["worker-1"])
	if err != nil || before.GetCurrentGenerationId() != after.GetCurrentGenerationId() {
		t.Fatalf("repeat operations replaced the worker generation: %v", err)
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "apply", "-f", filepath.Join(repo, "internal/vmtest/scenarios/testdata/bootstrap/cross-node-workload.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "katl-vmtest", "wait", "--for=condition=Complete", "job/net-client", "--timeout=3m"); err != nil {
		t.Fatal(err)
	}
	logs, err := kubectlOutput(ctx, kubeconfig, "-n", "katl-vmtest", "logs", "job/net-client")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(result.RunDir, "cross-node-workload.log"), logs, 0o600); err != nil {
		t.Fatal(err)
	}
}

func portableExternalWorkerProxy(ctx context.Context, kubeconfig, dir, address string) error {
	data, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "get", "configmap", "kube-proxy", "-o", "json")
	if err != nil {
		return err
	}
	var configMap struct{ Data map[string]string }
	if err := json.Unmarshal(data, &configMap); err != nil {
		return err
	}
	var config map[string]any
	if err := yaml.Unmarshal([]byte(configMap.Data["kubeconfig.conf"]), &config); err != nil {
		return err
	}
	clusters, ok := config["clusters"].([]any)
	if !ok || len(clusters) != 1 {
		return fmt.Errorf("expected one kube-proxy API cluster")
	}
	entry, ok := clusters[0].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid kube-proxy cluster entry")
	}
	cluster, ok := entry["cluster"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid kube-proxy cluster configuration")
	}
	cluster["server"] = "https://" + net.JoinHostPort(address, "6443")
	data, err = yaml.Marshal(config)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{"kubeconfig.conf": string(data)}})
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "kube-proxy-patch.json")
	if err := os.WriteFile(path, patch, 0o600); err != nil {
		return err
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "patch", "configmap", "kube-proxy", "--type=merge", "--patch-file", path); err != nil {
		return err
	}
	_, err = kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "rollout", "restart", "daemonset/kube-proxy")
	return err
}
