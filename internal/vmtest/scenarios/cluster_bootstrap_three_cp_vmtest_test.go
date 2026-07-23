package scenarios

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/bootstrap/readiness"
	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/kubeadmplan"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/vmtest"
	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

func TestInstalledRuntimeThreeControlPlaneStackedEtcdSmoke(t *testing.T) {
	if run, ok := threeControlPlaneWorldSmokeRun(t, "installed-runtime-three-control-plane-stacked-etcd", false); ok {
		runThreeControlPlaneStackedEtcdSmoke(t, run)
		return
	}

	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run three-control-plane stacked-etcd smoke")
	}
	_ = vmtest.RequireWorld(t)
}

func TestInstalledRuntimeThreeControlPlaneV01WorkloadProof(t *testing.T) {
	if run, ok := threeControlPlaneWorldSmokeRun(t, "installed-runtime-three-control-plane-v01-workload-proof", true); ok {
		runThreeControlPlaneStackedEtcdSmoke(t, run)
		return
	}

	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run three-control-plane v0.1 workload proof")
	}
	_ = vmtest.RequireWorld(t)
}

type threeControlPlaneSmokeRun struct {
	WorldScenario *vmtest.WorldScenario
	Options       vmtest.Options
	Runner        vmtest.Runner
	Scenario      vmtest.Scenario
	Result        vmtest.Result
	Inputs        threeControlPlaneSmokeInputs
	LibvirtURI    string
	Network       string
	WorkloadProof bool
}

func threeControlPlaneWorldSmokeRun(t *testing.T, scenarioName string, workloadProof bool) (threeControlPlaneSmokeRun, bool) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(vmtest.WorldManifestEnv)) == "" {
		return threeControlPlaneSmokeRun{}, false
	}
	world := vmtest.RequireWorld(t)
	repo := katlRepoRoot(t)
	kvm := vmtest.DefaultOptions().KVM
	if err := ensurePublishedRuntimeFixturesForWorld(world, repo, threeControlPlaneWorldRuntimeSpecs(), kvm); err != nil {
		failWorldFixtureSetup(t, world, scenarioName, err)
	}
	run, err := planThreeControlPlaneWorldSmokeRun(world, repo, scenarioName, operationBackedKubernetesVersion(t, repo), kvm, workloadProof)
	if err != nil {
		failTwoNodeWorldSetup(t, run.WorldScenario, err)
	}
	missing := twoNodeHostToolPrereqs(exec.LookPath)
	requireSmokePrereqs(t, run.Runner, run.Scenario, run.Result, "three-control-plane stacked-etcd smoke prerequisites missing", missing)
	return run, true
}

func threeControlPlaneWorldRuntimeSpecs() []vmtest.NodeSpec {
	return []vmtest.NodeSpec{
		{Name: "cp-1", Role: vmtest.ControlPlane},
		{Name: "cp-2", Role: vmtest.ControlPlane},
		{Name: "cp-3", Role: vmtest.ControlPlane},
	}
}

func planThreeControlPlaneWorldSmokeRun(world vmtest.World, repo, scenarioName, kubernetesVersion string, kvm vmtest.KVMPolicy, workloadProof bool) (threeControlPlaneSmokeRun, error) {
	scenario, err := world.PlanScenario(scenarioName)
	if err != nil {
		return threeControlPlaneSmokeRun{}, err
	}
	run := threeControlPlaneSmokeRun{WorldScenario: scenario}
	nodes := make(map[string]vmtest.InstalledRuntimeWorldNode, 3)
	buildRoots := publishedRuntimeBuildRoots(world, repo)
	for _, name := range []string{"cp-1", "cp-2", "cp-3"} {
		node, err := vmtest.AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, buildRoots, vmtest.NodeSpec{Name: name, Role: vmtest.ControlPlane})
		if err != nil {
			_ = scenario.WriteSetupFailure(err)
			return run, err
		}
		nodes[name] = node
	}
	options := vmtest.Options{
		Enabled:   true,
		StateRoot: filepath.Join(scenario.Dir, "vm-runs"),
		Keep:      vmtest.KeepFailed,
		KVM:       kvm,
		Missing:   vmtest.MissingFails,
	}
	runner := vmtest.NewRunner(options)
	vmScenario := vmtest.Scenario{Name: scenarioName}
	result, err := runner.Plan(vmScenario)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	result.Started = time.Now().UTC()
	return threeControlPlaneSmokeRun{
		WorldScenario: scenario,
		Options:       options,
		Runner:        runner,
		Scenario:      vmScenario,
		Result:        result,
		LibvirtURI:    world.Libvirt.URI,
		Network:       world.Libvirt.Network,
		WorkloadProof: workloadProof,
		Inputs: threeControlPlaneSmokeInputs{
			CP1Disk:           nodes["cp-1"].Config.Disk,
			CP1DiskFormat:     string(nodes["cp-1"].Config.DiskFormat),
			CP1ESP:            nodes["cp-1"].Config.ESPArtifacts,
			CP1Fixture:        nodes["cp-1"].Config.FixtureManifest,
			CP1Metadata:       nodes["cp-1"].Config.NodeMetadata,
			CP1Address:        nodes["cp-1"].Node.Address,
			CP1MAC:            nodes["cp-1"].Node.MACAddress,
			CP2Disk:           nodes["cp-2"].Config.Disk,
			CP2DiskFormat:     string(nodes["cp-2"].Config.DiskFormat),
			CP2ESP:            nodes["cp-2"].Config.ESPArtifacts,
			CP2Fixture:        nodes["cp-2"].Config.FixtureManifest,
			CP2Metadata:       nodes["cp-2"].Config.NodeMetadata,
			CP2Address:        nodes["cp-2"].Node.Address,
			CP2MAC:            nodes["cp-2"].Node.MACAddress,
			CP3Disk:           nodes["cp-3"].Config.Disk,
			CP3DiskFormat:     string(nodes["cp-3"].Config.DiskFormat),
			CP3ESP:            nodes["cp-3"].Config.ESPArtifacts,
			CP3Fixture:        nodes["cp-3"].Config.FixtureManifest,
			CP3Metadata:       nodes["cp-3"].Config.NodeMetadata,
			CP3Address:        nodes["cp-3"].Node.Address,
			CP3MAC:            nodes["cp-3"].Node.MACAddress,
			KubernetesVersion: firstString(kubernetesVersion, "v1.36.1"),
			WorldProvenance:   multiNodeWorldProvenanceForSpecs(world, repo, threeControlPlaneWorldRuntimeSpecs()),
		},
	}, nil
}

func runThreeControlPlaneStackedEtcdSmoke(t *testing.T, smoke threeControlPlaneSmokeRun) {
	t.Helper()
	options := smoke.Options
	runner := smoke.Runner
	scenario := smoke.Scenario
	result := smoke.Result
	inputs := smoke.Inputs
	requireVMHost(t, runner, scenario, result, vmtest.HostRequirements{
		Libvirt: true,
		OVMF:    true,
		KVM:     options.KVM,
	})
	etcdTranscriptDir := filepath.Join(result.RunDir, "etcd-transcripts")
	inventoryPath := filepath.Join(result.ManifestDir, "bootstrap-inventory.yaml")
	kubeconfigPath := filepath.Join(result.RunDir, "operator-kubeconfig.yaml")
	kubeconfigMetadataPath := filepath.Join(result.RunDir, "operator-kubeconfig-metadata.json")
	stdoutPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stdout")
	stderrPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stderr")
	kubectlOut := filepath.Join(result.RunDir, "kubectl-get-nodes.txt")
	etcdReportPath := filepath.Join(result.RunDir, "etcd-report.json")
	evidenceDir := filepath.Join(result.RunDir, "operation-evidence")
	versionEvidenceDir := filepath.Join(result.RunDir, "kubernetes-version-evidence")
	bootstrapFixture := bootstrapFixtureInputsFromEnv()
	kubernetesBundle, bundleServer, err := stageOperationBackedKubernetesPayloadBundle(katlRepoRoot(t), result, smoke.WorldScenario.World.Network.Gateway, inputs.KubernetesVersion)
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("stage Kubernetes payload bundles: %v", err)
	}
	defer bundleServer.Close()
	plannedNodes := make([]vmtest.RunningInstalledRuntimeNode, 0, 3)
	for _, name := range []string{"cp-1", "cp-2", "cp-3"} {
		nodeResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, name)
		if err != nil {
			t.Fatal(err)
		}
		plannedNodes = append(plannedNodes, vmtest.RunningInstalledRuntimeNode{Name: name, Result: nodeResult})
	}
	if err := writeThreeControlPlaneSmokeArtifactManifest(result, inputs, "", etcdTranscriptDir, plannedNodes, bootstrapFixture, kubernetesBundle, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	cp1Node, err := vmtest.StartInstalledRuntimeNode(ctx, result, threeControlPlaneNodeConfigForRun(smoke, "cp-1", inputs.CP1Disk, inputs.CP1ESP, inputs.CP1Fixture, inputs.CP1Metadata, vmtest.DiskFormat(inputs.CP1DiskFormat), inputs.CP1MAC, 0), vmtest.VMRunner{})
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start cp-1 VM: %v", err)
	}
	defer stopNode(t, cp1Node)

	cp2Node, err := vmtest.StartInstalledRuntimeNode(ctx, result, threeControlPlaneNodeConfigForRun(smoke, "cp-2", inputs.CP2Disk, inputs.CP2ESP, inputs.CP2Fixture, inputs.CP2Metadata, vmtest.DiskFormat(inputs.CP2DiskFormat), inputs.CP2MAC, 0), vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics("", cp1Node)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start cp-2 VM: %v", err)
	}
	defer stopNode(t, cp2Node)

	cp3Node, err := vmtest.StartInstalledRuntimeNode(ctx, result, threeControlPlaneNodeConfigForRun(smoke, "cp-3", inputs.CP3Disk, inputs.CP3ESP, inputs.CP3Fixture, inputs.CP3Metadata, vmtest.DiskFormat(inputs.CP3DiskFormat), inputs.CP3MAC, 0), vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics("", cp1Node, cp2Node)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start cp-3 VM: %v", err)
	}
	defer stopNode(t, cp3Node)

	nodes := []vmtest.RunningInstalledRuntimeNode{cp1Node, cp2Node, cp3Node}
	for _, node := range nodes {
		if err := assertCNISysctls(ctx, node); err != nil {
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("check CNI sysctls on %s: %v", node.Name, err)
		}
		if err := installKubernetesBundleCA(ctx, node, kubernetesBundle); err != nil {
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("install Kubernetes bundle CA on %s: %v", node.Name, err)
		}
	}
	cp1Address := firstString(cp1Node.Result.IPAddress, inputs.CP1Address)
	cp2Address := firstString(cp2Node.Result.IPAddress, inputs.CP2Address)
	cp3Address := firstString(cp3Node.Result.IPAddress, inputs.CP3Address)
	addresses := map[string]string{"cp-1": cp1Address, "cp-2": cp2Address, "cp-3": cp3Address}
	cniFixtures, err := stageThreeControlPlaneCNIFixtures(ctx, katlRepoRoot(t), nodes, addresses)
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("stage test CNI fixtures: %v", err)
	}
	imageFixtures, err := stageKubernetesImageFixtures(ctx, katlRepoRoot(t), inputs.KubernetesVersion, nodes...)
	if err == nil && smoke.WorkloadProof {
		var workloadFixtures map[string][]nodeImageFixture
		workloadFixtures, err = stageTwoNodeImageFixtures(ctx, katlRepoRoot(t), result.RunDir, nodes...)
		mergeNodeImageFixtures(imageFixtures, workloadFixtures)
	}
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("stage bootstrap images: %v", err)
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeThreeControlPlaneOperationBackedInventory(inventoryPath, inputs.KubernetesVersion, kubernetesBundle, nodes, addresses); err != nil {
		t.Fatal(err)
	}
	if err := writeThreeControlPlaneSmokeArtifactManifest(result, inputs, "", etcdTranscriptDir, nodes, bootstrapFixture, kubernetesBundle, cniFixtures, imageFixtures, nil); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err = runKatlctlCommand(t, ctx, katlRepoRoot(t), appendBootstrapFixtureArgs([]string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--init-node", "cp-1",
		"--control-plane-endpoint", cp1Address + ":6443",
		"--kubernetes-bundle", kubernetesBundle.Ref,
		"--node-address", "cp-1=" + cp1Address,
		"--node-address", "cp-2=" + cp2Address,
		"--node-address", "cp-3=" + cp3Address,
		"--kubeconfig-out", kubeconfigPath,
		"--overwrite-kubeconfig",
	}, bootstrapFixture), &stdout, &stderr)
	_ = os.WriteFile(stdoutPath, stdout.Bytes(), 0o644)
	_ = os.WriteFile(stderrPath, stderr.Bytes(), 0o644)
	_ = writeKubeconfigMetadata(kubeconfigPath, kubeconfigMetadataPath)
	err = bootstrapCommandError(err, stdout.String())
	if err != nil {
		collectOperationBackedFailureEvidence(ctx, cp1Node, filepath.Join(evidenceDir, "cp-1"), "bootstrap-init")
		collectOperationBackedFailureEvidence(ctx, cp2Node, filepath.Join(evidenceDir, "cp-2"), "bootstrap-join-control-plane")
		collectOperationBackedFailureEvidence(ctx, cp3Node, filepath.Join(evidenceDir, "cp-3"), "bootstrap-join-control-plane")
		collectKubectlDiagnosticsIfKubeconfigExists(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("katlctl cluster bootstrap failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	assertThreeControlPlaneBootstrapPhases(t, stdout.String())
	_, cp1Record, err := collectOperationEvidence(ctx, cp1Node, filepath.Join(evidenceDir, "cp-1"), "bootstrap-init")
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("collect cp-1 operation evidence: %v", err)
	}
	assertOperationKubernetesBundle(t, cp1Record, kubernetesBundle)
	assertGenerationKubernetesBundle(t, ctx, cp1Node, filepath.Join(evidenceDir, "cp-1"), cp1Record, kubernetesBundle)
	for _, node := range []vmtest.RunningInstalledRuntimeNode{cp2Node, cp3Node} {
		_, record, err := collectOperationEvidence(ctx, node, filepath.Join(evidenceDir, node.Name), "bootstrap-join-control-plane")
		if err != nil {
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("collect %s operation evidence: %v", node.Name, err)
		}
		if record.ExecutorPlan == nil || record.ExecutorPlan.Phase != "kubeadm-join-control-plane" || record.OperationKind != "bootstrap-join-control-plane" {
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, "unexpected control-plane join operation record")
			t.Fatalf("%s operation record = %#v", node.Name, record)
		}
		assertOperationKubernetesBundle(t, record, kubernetesBundle)
		assertGenerationKubernetesBundle(t, ctx, node, filepath.Join(evidenceDir, node.Name), record, kubernetesBundle)
	}

	output, err := waitForKubectlNodes(ctx, kubeconfigPath, kubectlOut, 5*time.Minute, "node/cp-1", "node/cp-2", "node/cp-3")
	if err != nil {
		collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("wait for kubectl nodes failed: %v\n%s", err, output)
	}
	if err := waitForAgentKubernetesReady(ctx, nodes, addresses, 3*time.Minute); err != nil {
		collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("wait for agent Kubernetes readiness failed: %v", err)
	}
	collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
	for _, node := range nodes {
		if _, err := collectKubernetesVersionEvidence(ctx, node, filepath.Join(versionEvidenceDir, node.Name), kubernetesBundle.PayloadVersion); err != nil {
			collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("collect %s Kubernetes version evidence: %v", node.Name, err)
		}
	}
	var workloadStack *releaseWorkloadStackEvidence
	if smoke.WorkloadProof {
		workloadStack, err = proveReleaseWorkloadStack(ctx, katlRepoRoot(t), result.RunDir, result.ManifestDir, kubeconfigPath, cp1Address)
		if err != nil {
			collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("prove release workload stack: %v", err)
		}
		if err := writeThreeControlPlaneSmokeArtifactManifest(result, inputs, "", etcdTranscriptDir, nodes, bootstrapFixture, kubernetesBundle, cniFixtures, imageFixtures, workloadStack); err != nil {
			t.Fatal(err)
		}
	}
	etcdReport, err := verifyThreeControlPlaneEtcd(ctx, etcdTranscriptDir, nodes)
	if err != nil {
		etcdReport.FailureSummary = err.Error()
		if writeErr := writeThreeControlPlaneEtcdReport(etcdReportPath, etcdReport); writeErr != nil {
			t.Fatalf("write failed etcd report: %v; original error: %v", writeErr, err)
		}
		collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("verify stacked etcd: %v", err)
	}
	if err := writeThreeControlPlaneEtcdReport(etcdReportPath, etcdReport); err != nil {
		t.Fatalf("write etcd report: %v", err)
	}
	if smoke.WorkloadProof {
		if err := runThreeControlPlaneConfigOperationProof(t, ctx, result, nodes, addresses, inventoryPath, kubeconfigPath, kubernetesBundle, etcdReport); err != nil {
			collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("kubeadm control-plane config operation proof failed: %v", err)
		}
		postOperationReport, err := verifyThreeControlPlaneEtcdAt(ctx, filepath.Join(result.RunDir, "etcd-post-config-transcripts"), nodes, "/var/lib/etcd/katl-snapshots/three-control-plane-post-config.db")
		if err != nil {
			postOperationReport.FailureSummary = err.Error()
			_ = writeThreeControlPlaneEtcdReport(filepath.Join(result.RunDir, "etcd-post-config-report.json"), postOperationReport)
			collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("verify stacked etcd after control-plane config operation: %v", err)
		}
		if err := writeThreeControlPlaneEtcdReport(filepath.Join(result.RunDir, "etcd-post-config-report.json"), postOperationReport); err != nil {
			t.Fatal(err)
		}
	}
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusPassed, "")
}

func waitForAgentKubernetesReady(ctx context.Context, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string, timeout time.Duration) error {
	for _, node := range nodes {
		deadline := time.Now().Add(timeout)
		last := "Kubernetes status was not reported"
		for {
			connector := cluster.TCPAgentConnector{DialTimeout: 5 * time.Second}
			connection, err := connector.Connect(ctx, inventory.PlannedNode{
				Name: node.Name, Address: addresses[node.Name], Access: inventory.Access{Method: "agent"},
			})
			if err == nil {
				status, statusErr := connection.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
				_ = connection.Close()
				if statusErr == nil {
					kubernetes := status.GetKubernetes()
					if kubernetes != nil && kubernetes.GetState() == "ready" && kubernetes.GetKubeletActive() && kubernetes.GetNodeReady() && kubernetes.GetControlPlaneComponentsReady() {
						break
					}
					if kubernetes != nil {
						last = firstString(kubernetes.GetFailureReason(), kubernetes.GetState())
					}
				} else {
					last = statusErr.Error()
				}
			} else {
				last = err.Error()
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not report ready Kubernetes state: %s", node.Name, last)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil
}

func assertCNISysctls(ctx context.Context, node vmtest.RunningInstalledRuntimeNode) error {
	const testInterface = "lxc_katltest"
	created, err := runNodeCommandWithRetry(ctx, node, []string{
		"ip", "link", "add", testInterface, "type", "dummy",
	}, 16<<10)
	if err != nil {
		return err
	}
	if created.ExitStatus != 0 {
		return commandErrorDetail(created)
	}
	defer func() {
		_, _ = runNodeCommandWithRetry(ctx, node, []string{"ip", "link", "delete", testInterface}, 16<<10)
	}()
	settled, err := runNodeCommandWithRetry(ctx, node, []string{"udevadm", "settle"}, 16<<10)
	if err != nil {
		return err
	}
	if settled.ExitStatus != 0 {
		return commandErrorDetail(settled)
	}
	result, err := runNodeCommandWithRetry(ctx, node, []string{
		"sysctl", "-n",
		"net.ipv4.conf.all.rp_filter",
		"net.ipv4.conf.default.rp_filter",
		"net.ipv4.conf." + testInterface + ".rp_filter",
	}, 16<<10)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		return commandErrorDetail(result)
	}
	if got, want := strings.Fields(string(result.Stdout)), []string{"0", "0", "0"}; !reflect.DeepEqual(got, want) {
		return fmt.Errorf("reverse-path filtering values = %v, want %v", got, want)
	}
	return nil
}

func runThreeControlPlaneConfigOperationProof(t *testing.T, ctx context.Context, result vmtest.Result, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string, inventoryPath, kubeconfigPath string, bundle threeControlPlaneKubernetesPayloadBundle, snapshot threeControlPlaneEtcdReport) error {
	t.Helper()
	const generationID = "2026.07.11-vmtest-control-plane-config"
	const configName = "control-plane-profiled"
	proofDir := filepath.Join(result.RunDir, "kubeadm-control-plane-config")
	if err := os.MkdirAll(proofDir, 0o755); err != nil {
		return err
	}
	liveConfig, err := kubectlOutput(ctx, kubeconfigPath, "-n", "kube-system", "get", "configmap", "kubeadm-config", "-o", "jsonpath={.data.ClusterConfiguration}")
	if err != nil {
		return fmt.Errorf("collect live kubeadm config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(proofDir, "live-cluster-configuration.yaml"), liveConfig, 0o600); err != nil {
		return err
	}
	desiredConfig, _, err := controlPlaneProfilingConfig(liveConfig)
	if err != nil {
		return err
	}
	desiredDigest, err := kubeadmplan.CanonicalClusterConfigurationSHA256(desiredConfig)
	if err != nil {
		return err
	}
	liveKubeletConfig, err := kubectlOutput(ctx, kubeconfigPath, "-n", "kube-system", "get", "configmap", "kubelet-config", "-o", "jsonpath={.data.kubelet}")
	if err != nil {
		return fmt.Errorf("collect live kubelet config: %w", err)
	}
	desiredKubeletConfig, err := kubeletOperationConfig(liveKubeletConfig, 120)
	if err != nil {
		return err
	}
	desiredKubeletDigest, err := kubeadmplan.CanonicalKubeletConfigurationSHA256(desiredKubeletConfig)
	if err != nil {
		return err
	}
	desiredConfig = append(bytes.TrimSpace(desiredConfig), []byte("\n---\n")...)
	desiredConfig = append(desiredConfig, desiredKubeletConfig...)
	if err := os.WriteFile(filepath.Join(proofDir, "desired-config.yaml"), desiredConfig, 0o600); err != nil {
		return err
	}
	requestPath := filepath.Join(proofDir, "config-apply.yaml")
	inlineConfig := strings.ReplaceAll(strings.TrimSuffix(string(desiredConfig), "\n"), "\n", "\n        ")
	request := fmt.Appendf(nil, "apiVersion: katl.dev/v1alpha1\nkind: NodeConfigurationChange\nmetadata:\n  sourceID: vmtest\n  desiredVersion: \"20260711\"\napply:\n  mode: live\nspec:\n  kubeadmConfigs:\n    %s:\n      config: |\n        %s\n  clusterDefaults:\n    kubernetes:\n      kubeadm:\n        configRef: %s\n", configName, inlineConfig, configName)
	if err := os.WriteFile(requestPath, request, 0o600); err != nil {
		return err
	}
	katlctl := buildKatlctlCommand(t, ctx, katlRepoRoot(t))
	for _, node := range nodes {
		stdout, stderr, err := runProofKatlctl(ctx, katlctl, proofDir, "stage-"+node.Name,
			"node", "apply", "--endpoint", net.JoinHostPort(addresses[node.Name], "9443"), "--config", requestPath, "--mode", "live", "--candidate-generation", generationID, "--client-request-id", "vmtest-control-plane-config-stage-"+node.Name, "--actor", "three-control-plane release proof", "--output", "json")
		if err != nil {
			return fmt.Errorf("activate desired generation on %s: %w: %s", node.Name, err, stderr)
		}
		if err := decodeGenerationApplyResult(stdout, generationID); err != nil {
			return fmt.Errorf("decode %s activated generation result: %w", node.Name, err)
		}
		if err := waitForConfigGeneration(ctx, katlctl, proofDir, node, addresses[node.Name], generationID, configName, true); err != nil {
			return err
		}
	}
	if _, err := waitForKubectlNodes(ctx, kubeconfigPath, filepath.Join(proofDir, "kubectl-before-operation.txt"), 5*time.Minute, "node/cp-1", "node/cp-2", "node/cp-3"); err != nil {
		return err
	}
	const rolloutID = "2026.07.11-vmtest-cluster-config"
	args := []string{"cluster", "apply", "--inventory", inventoryPath, "--coordinator", "cp-3", "--generation", generationID, "--config-name", configName, "--rollout-id", rolloutID}
	stdout, stderr, err := runProofKatlctl(ctx, katlctl, proofDir, "rollout", args...)
	if err != nil {
		return fmt.Errorf("run serial control-plane config rollout: %w: %s", err, stderr)
	}
	if !bytes.Contains(stdout, []byte(`"automaticRollback":false`)) || !bytes.Contains(stdout, []byte(`"result":"succeeded"`)) {
		return fmt.Errorf("rollout summary did not record automaticRollback=false: %s", stdout)
	}
	for position, node := range nodes {
		_, record, err := collectOperationEvidenceForRollout(ctx, node, filepath.Join(proofDir, node.Name), "kubeadm-control-plane-config", rolloutID+"-control-plane")
		if err != nil {
			return fmt.Errorf("collect %s config operation evidence: %w", node.Name, err)
		}
		body := record.KubeadmControlPlaneConfig
		if body == nil || body.Component != "control-plane" || body.NodePosition != uint32(position+1) || body.CoordinatorUpload != (node.Name == "cp-3") || record.Result != operation.ResultSucceeded || body.DesiredConfigSHA256 != desiredDigest || len(body.BeforeManifestSHA256) != 3 || len(body.AfterManifestSHA256) != 3 {
			return fmt.Errorf("%s control-plane config operation evidence is incomplete: %#v", node.Name, record)
		}
	}
	uploaded, err := kubectlOutput(ctx, kubeconfigPath, "-n", "kube-system", "get", "configmap", "kubeadm-config", "-o", "jsonpath={.data.ClusterConfiguration}")
	if err != nil {
		return err
	}
	uploadedDigest, err := kubeadmplan.CanonicalClusterConfigurationSHA256(uploaded)
	if err != nil || uploadedDigest != desiredDigest {
		return fmt.Errorf("uploaded kubeadm config digest = %s, want %s: %w", uploadedDigest, desiredDigest, err)
	}
	for _, node := range nodes {
		for _, component := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
			command, err := waitForKubectlOutputContains(ctx, kubeconfigPath, 2*time.Minute, []byte("--profiling=false"), "-n", "kube-system", "get", "pod", component+"-"+node.Name, "-o", "jsonpath={.spec.containers[0].command}")
			if err != nil {
				return fmt.Errorf("%s on %s does not run with profiling disabled: %w: %s", component, node.Name, err, command)
			}
		}
	}
	expectedPosition := map[string]uint32{"cp-3": 1, "cp-1": 2, "cp-2": 3}
	for _, node := range nodes {
		_, record, err := collectOperationEvidenceForRollout(ctx, node, filepath.Join(proofDir, node.Name+"-kubelet"), "kubeadm-control-plane-config", rolloutID+"-kubelet")
		if err != nil {
			return fmt.Errorf("collect %s kubelet config operation evidence: %w", node.Name, err)
		}
		body := record.KubeadmControlPlaneConfig
		if body == nil || body.Component != "kubelet" || body.NodePosition != expectedPosition[node.Name] || body.CoordinatorUpload != (node.Name == "cp-3") || body.ConfigUploadRan != (node.Name == "cp-3") || record.Result != operation.ResultSucceeded || body.DesiredConfigSHA256 != desiredKubeletDigest {
			return fmt.Errorf("%s kubelet config operation evidence is incomplete: %#v", node.Name, record)
		}
		actual, err := readNodeFileWithRetry(ctx, node, "/var/lib/kubelet/config.yaml", 2<<20, 30*time.Second)
		if err != nil {
			return fmt.Errorf("read %s kubelet config: %w", node.Name, err)
		}
		if err := kubeadmplan.KubeletConfigurationContains(actual, desiredKubeletConfig); err != nil {
			return fmt.Errorf("verify %s kubelet config: %w", node.Name, err)
		}
	}
	if _, err := waitForKubectlNodes(ctx, kubeconfigPath, filepath.Join(proofDir, "kubectl-after-cluster-apply.txt"), 5*time.Minute, "node/cp-1", "node/cp-2", "node/cp-3"); err != nil {
		return err
	}
	return nil
}

func kubeletOperationConfig(live []byte, maxPods int) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(live, &document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("live KubeletConfiguration must be one YAML mapping")
	}
	root := document.Content[0]
	for _, field := range []struct {
		name, tag, value string
	}{
		{name: "maxPods", tag: "!!int", value: strconv.Itoa(maxPods)},
		{name: "shutdownGracePeriod", tag: "!!str", value: "60s"},
		{name: "shutdownGracePeriodCriticalPods", tag: "!!str", value: "20s"},
	} {
		value := yamlMappingValue(root, field.name)
		if value == nil {
			value = &yaml.Node{}
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: field.name}, value)
		}
		value.Kind = yaml.ScalarNode
		value.Tag = field.tag
		value.Value = field.value
	}
	return yaml.Marshal(&document)
}

func waitForKubectlOutputContains(ctx context.Context, kubeconfigPath string, timeout time.Duration, needle []byte, args ...string) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	var last []byte
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = kubectlOutput(ctx, kubeconfigPath, args...)
		if lastErr == nil && bytes.Contains(last, needle) {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if lastErr != nil {
		return last, lastErr
	}
	return last, fmt.Errorf("output did not contain %q before timeout", needle)
}

func controlPlaneProfilingConfig(live []byte) ([]byte, []string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(live, &document); err != nil {
		return nil, nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("live ClusterConfiguration must be one YAML mapping")
	}
	root := document.Content[0]
	for _, component := range []string{"apiServer", "controllerManager", "scheduler"} {
		section := yamlMappingValue(root, component)
		if section == nil {
			section = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: component}, section)
		}
		if section.Kind != yaml.MappingNode {
			return nil, nil, fmt.Errorf("live %s must be a YAML mapping", component)
		}
		args := yamlMappingValue(section, "extraArgs")
		if args == nil {
			args = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			section.Content = append(section.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "extraArgs"}, args)
		}
		if args.Kind != yaml.SequenceNode {
			return nil, nil, fmt.Errorf("live %s.extraArgs must be a YAML sequence", component)
		}
		args.Content = append(args.Content, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "profiling"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "value"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "false", Style: yaml.DoubleQuotedStyle},
		}})
	}
	clusterYAML, err := yaml.Marshal(&document)
	if err != nil {
		return nil, nil, err
	}
	desired := append([]byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\n"), clusterYAML...)
	deltas, err := kubeadmplan.SupportedControlPlaneProfilingDelta(desired, live)
	return desired, deltas, err
}

func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func runProofKatlctl(ctx context.Context, binary, dir, name string, args ...string) ([]byte, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = os.Environ()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	_ = os.WriteFile(filepath.Join(dir, name+".stdout"), stdout.Bytes(), 0o644)
	_ = os.WriteFile(filepath.Join(dir, name+".stderr"), stderr.Bytes(), 0o644)
	return stdout.Bytes(), stderr.String(), err
}

func decodeGenerationApplyResult(data []byte, generationID string) error {
	var status agentapi.OperationStatus
	if err := protojson.Unmarshal(data, &status); err != nil {
		return err
	}
	if status.OperationKind != "generation-apply" || !status.Terminal || status.Result != operation.ResultSucceeded || status.CandidateGenerationId != generationID || status.ActivationState != operation.ActivationStateActiveLive {
		return fmt.Errorf("unexpected live generation apply status: %s", status.String())
	}
	return nil
}

func waitForConfigGeneration(ctx context.Context, katlctl, dir string, node vmtest.RunningInstalledRuntimeNode, address, generationID, configName string, healthy bool) error {
	deadline := time.Now().Add(5 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		stdout, stderr, err := runProofKatlctl(ctx, katlctl, dir, "status-"+node.Name, "node", "apply", "status", "--endpoint", net.JoinHostPort(address, "9443"), "--generation", generationID)
		last = stderr
		if err == nil {
			var generation agentapi.Generation
			if decodeErr := protojson.Unmarshal(stdout, &generation); decodeErr == nil {
				ready := generation.CommitState == "committed"
				if healthy {
					ready = ready && generation.HealthState == "healthy"
				}
				if ready && generation.GetConfigApply().GetKubeadmActionRequired() && generation.GetConfigApply().GetSelectedKubeadmConfigName() == configName {
					return nil
				}
				last = generation.String()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("generation %s on %s did not become ready: %s", generationID, node.Name, last)
}

func kubectlOutput(ctx context.Context, kubeconfig string, args ...string) ([]byte, error) {
	argv := append([]string{"--kubeconfig", kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", argv...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, output)
	}
	return output, nil
}

func nodeFileSHA256(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, path string) (string, error) {
	result, err := runNodeCommandWithRetry(ctx, node, []string{"sha256sum", path}, 16<<10)
	if err != nil {
		return "", fmt.Errorf("sha256 %s on %s: %w", path, node.Name, err)
	}
	if result.ExitStatus != 0 {
		return "", fmt.Errorf("sha256 %s on %s: %w", path, node.Name, commandErrorDetail(result))
	}
	fields := strings.Fields(string(result.Stdout))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", fmt.Errorf("invalid sha256 output for %s: %q", path, result.Stdout)
	}
	return fields[0], nil
}

type threeControlPlaneSmokeInputs struct {
	CP1Disk           string
	CP1DiskFormat     string
	CP1ESP            string
	CP1Fixture        string
	CP1Metadata       string
	CP1Address        string
	CP1MAC            string
	CP2Disk           string
	CP2DiskFormat     string
	CP2ESP            string
	CP2Fixture        string
	CP2Metadata       string
	CP2Address        string
	CP2MAC            string
	CP3Disk           string
	CP3DiskFormat     string
	CP3ESP            string
	CP3Fixture        string
	CP3Metadata       string
	CP3Address        string
	CP3MAC            string
	KubernetesVersion string
	WorldProvenance   multiNodeWorldProvenancePaths
}

type threeControlPlaneKubernetesPayloadBundle struct {
	Source               string            `json:"source,omitempty"`
	Ref                  string            `json:"ref,omitempty"`
	PayloadVersion       string            `json:"payloadVersion,omitempty"`
	BundleManifestDigest string            `json:"bundleManifestDigest,omitempty"`
	SysextPayloadDigest  string            `json:"sysextPayloadDigest,omitempty"`
	Root                 string            `json:"root,omitempty"`
	IndexPath            string            `json:"indexPath,omitempty"`
	CatalogPath          string            `json:"catalogPath,omitempty"`
	BundlePaths          map[string]string `json:"bundlePaths,omitempty"`
	CACertPath           string            `json:"caCertPath,omitempty"`
	CACertGuestPath      string            `json:"caCertGuestPath,omitempty"`
	LogPath              string            `json:"logPath,omitempty"`
	CACertPEM            []byte            `json:"-"`
}

func threeControlPlaneNodeConfig(name, disk, esp, fixtureManifest, nodeMetadata string, format vmtest.DiskFormat, kvm vmtest.KVMPolicy, cid uint32) vmtest.InstalledRuntimeNodeConfig {
	return vmtest.InstalledRuntimeNodeConfig{
		Name: name,
		Runtime: vmtest.InstalledRuntimeConfig{
			Disk:            disk,
			DiskFormat:      format,
			ESPArtifacts:    esp,
			FixtureManifest: fixtureManifest,
			NodeMetadata:    nodeMetadata,
			VM:              twoNodeVMConfig(kvm, cid),
		},
	}
}

func threeControlPlaneNodeConfigForRun(run threeControlPlaneSmokeRun, name, disk, esp, fixtureManifest, nodeMetadata string, format vmtest.DiskFormat, mac string, cid uint32) vmtest.InstalledRuntimeNodeConfig {
	config := threeControlPlaneNodeConfig(name, disk, esp, fixtureManifest, nodeMetadata, format, run.Options.KVM, cid)
	config.Runtime.VM.LibvirtURI = run.LibvirtURI
	config.Runtime.VM.LibvirtNetwork = run.Network
	config.Runtime.VM.Network.MAC = mac
	return config
}

func stageThreeControlPlaneKubernetesPayloadBundles(repo string, result vmtest.Result, selectedVersion string) (threeControlPlaneKubernetesPayloadBundle, error) {
	selectedVersion = firstString(strings.TrimSpace(selectedVersion), "v1.36.1")
	if selectedVersion != "v1.36.0" && selectedVersion != "v1.36.1" {
		return threeControlPlaneKubernetesPayloadBundle{}, fmt.Errorf("three-control-plane bundle proof supports v1.36.0 or v1.36.1, got %q", selectedVersion)
	}
	artifactPath := filepath.Join(repo, "_build/mkosi/katl-kubernetes.raw")
	baseMetadataPath := filepath.Join(repo, "_build/mkosi/katl-kubernetes.raw.json")
	baseMeta, err := artifact.ReadLocal(baseMetadataPath)
	if err != nil {
		return threeControlPlaneKubernetesPayloadBundle{}, fmt.Errorf("read Kubernetes sysext metadata %s: %w", baseMetadataPath, err)
	}
	if _, err := os.Stat(artifactPath); err != nil {
		return threeControlPlaneKubernetesPayloadBundle{}, fmt.Errorf("inspect Kubernetes sysext artifact %s: %w", artifactPath, err)
	}
	if baseMeta.PayloadVersion != selectedVersion {
		return threeControlPlaneKubernetesPayloadBundle{}, fmt.Errorf("Kubernetes sysext artifact payload version %q does not match selected version %q: rebuild with KATL_KUBERNETES_PAYLOAD_VERSION=%s", baseMeta.PayloadVersion, selectedVersion, selectedVersion)
	}
	root := filepath.Join(result.ManifestDir, "kubernetes-payload-bundles")
	bundlePaths := map[string]string{}
	selected, err := sysextcatalog.StageKubernetesSysext(sysextcatalog.StageRequest{
		MetadataPath: baseMetadataPath,
		ArtifactPath: artifactPath,
		OutputDir:    root,
	})
	if err != nil {
		return threeControlPlaneKubernetesPayloadBundle{}, fmt.Errorf("stage Kubernetes bundle %s: %w", selectedVersion, err)
	}
	bundlePaths[selectedVersion] = selected.BundlePath
	return threeControlPlaneKubernetesPayloadBundle{
		Ref:                  selectedVersion + "@" + selected.BundleManifestDigest,
		PayloadVersion:       selectedVersion,
		BundleManifestDigest: selected.BundleManifestDigest,
		SysextPayloadDigest:  "sha256:" + selected.Entry.SHA256,
		Root:                 root,
		IndexPath:            selected.IndexPath,
		CatalogPath:          selected.BundleCatalogPath,
		BundlePaths:          bundlePaths,
	}, nil
}

type guestReachableBundleServer struct {
	Server               *httptest.Server
	Source               string
	Ref                  string
	BundleManifestDigest string
	CACertPEM            []byte
}

func (s guestReachableBundleServer) Close() {
	if s.Server != nil {
		s.Server.Close()
	}
}

type kubernetesBundleRegistry struct {
	Handler              http.Handler
	Tag                  string
	ManifestDigest       string
	BundleManifestDigest string
}

func newKubernetesBundleRegistry(bundle threeControlPlaneKubernetesPayloadBundle, repository string) (kubernetesBundleRegistry, error) {
	bundlePath := bundle.BundlePaths[bundle.PayloadVersion]
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		return kubernetesBundleRegistry{}, fmt.Errorf("read Kubernetes bundle manifest: %w", err)
	}
	var manifest sysextcatalog.KubernetesPayloadBundle
	if err := json.Unmarshal(bundleBytes, &manifest); err != nil {
		return kubernetesBundleRegistry{}, fmt.Errorf("decode Kubernetes bundle manifest: %w", err)
	}
	artifactVersion := bundle.PayloadVersion + "-katl.0"
	manifest.ArtifactVersion = artifactVersion
	for i := range manifest.Metadata {
		descriptor := &manifest.Metadata[i]
		field := "artifactVersion"
		if descriptor.Role == "sysext-metadata" {
			field = "version"
		}
		path := filepath.Join(filepath.Dir(bundlePath), descriptor.FileName)
		data, err := rewriteBundleVersion(path, field, artifactVersion)
		if err != nil {
			return kubernetesBundleRegistry{}, err
		}
		descriptor.Digest = digest.FromBytes(data).String()
		descriptor.SizeBytes = int64(len(data))
		if err := writeRegistryBlob(bundle.Root, descriptor.Digest, data); err != nil {
			return kubernetesBundleRegistry{}, err
		}
	}
	bundleBytes, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return kubernetesBundleRegistry{}, err
	}
	bundleBytes = append(bundleBytes, '\n')
	if err := os.WriteFile(bundlePath, bundleBytes, 0o644); err != nil {
		return kubernetesBundleRegistry{}, err
	}
	bundleDigest := digest.FromBytes(bundleBytes)
	if err := writeRegistryBlob(bundle.Root, bundleDigest.String(), bundleBytes); err != nil {
		return kubernetesBundleRegistry{}, err
	}

	config := ocispec.Descriptor{MediaType: "application/vnd.katl.kubernetes.payload.bundle.v1+json", Digest: bundleDigest, Size: int64(len(bundleBytes))}
	layers := make([]ocispec.Descriptor, 0, len(manifest.Payloads)+len(manifest.Metadata))
	mediaTypes := map[string]string{bundleDigest.String(): config.MediaType}
	for _, descriptor := range append(append([]sysextcatalog.BundleDescriptor(nil), manifest.Payloads...), manifest.Metadata...) {
		desc := ocispec.Descriptor{MediaType: descriptor.MediaType, Digest: digest.Digest(descriptor.Digest), Size: descriptor.SizeBytes}
		if err := desc.Digest.Validate(); err != nil {
			return kubernetesBundleRegistry{}, fmt.Errorf("invalid %s descriptor digest: %w", descriptor.Role, err)
		}
		layers = append(layers, desc)
		mediaTypes[desc.Digest.String()] = desc.MediaType
	}
	ociManifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.katl.kubernetes.payload.bundle.v1",
		Config:       config,
		Layers:       layers,
	}
	ociBytes, err := json.Marshal(ociManifest)
	if err != nil {
		return kubernetesBundleRegistry{}, err
	}
	ociDigest := digest.FromBytes(ociBytes)
	manifestPath := "/v2/" + repository + "/manifests/"
	blobPath := "/v2/" + repository + "/blobs/"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, manifestPath):
			ref := strings.TrimPrefix(r.URL.Path, manifestPath)
			if ref != artifactVersion && ref != ociDigest.String() {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", ociDigest.String())
			http.ServeContent(w, r, "manifest.json", time.Time{}, bytes.NewReader(ociBytes))
		case strings.HasPrefix(r.URL.Path, blobPath):
			blobDigest := strings.TrimPrefix(r.URL.Path, blobPath)
			mediaType, ok := mediaTypes[blobDigest]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", mediaType)
			w.Header().Set("Docker-Content-Digest", blobDigest)
			http.ServeFile(w, r, filepath.Join(bundle.Root, "blobs", "sha256", strings.TrimPrefix(blobDigest, "sha256:")))
		default:
			http.NotFound(w, r)
		}
	})
	return kubernetesBundleRegistry{Handler: handler, Tag: artifactVersion, ManifestDigest: ociDigest.String(), BundleManifestDigest: bundleDigest.String()}, nil
}

func rewriteBundleVersion(path, field, version string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	document[field] = version
	data, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, err
	}
	return data, nil
}

func writeRegistryBlob(root, value string, data []byte) error {
	path := filepath.Join(root, "blobs", "sha256", strings.TrimPrefix(value, "sha256:"))
	return os.WriteFile(path, data, 0o644)
}

func startGuestReachableKubernetesBundleServer(gateway string, bundle threeControlPlaneKubernetesPayloadBundle) (guestReachableBundleServer, error) {
	gateway = strings.TrimSpace(gateway)
	if net.ParseIP(gateway) == nil {
		return guestReachableBundleServer{}, fmt.Errorf("world network gateway %q is not an IP address", gateway)
	}
	cert, caPEM, err := kubernetesBundleServerCertificate(gateway)
	if err != nil {
		return guestReachableBundleServer{}, err
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return guestReachableBundleServer{}, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	host := net.JoinHostPort(gateway, strconv.Itoa(port))
	repository := "katl-vmtest/kubernetes"
	registry, err := newKubernetesBundleRegistry(bundle, repository)
	if err != nil {
		listener.Close()
		return guestReachableBundleServer{}, err
	}
	server := httptest.NewUnstartedServer(registry.Handler)
	server.Listener = listener
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	return guestReachableBundleServer{
		Server:               server,
		Source:               "https://" + host + "/v2/" + repository,
		Ref:                  host + "/" + repository + ":" + registry.Tag + "@" + registry.ManifestDigest,
		BundleManifestDigest: registry.BundleManifestDigest,
		CACertPEM:            caPEM,
	}, nil
}

func kubernetesBundleServerCertificate(gateway string) (tls.Certificate, []byte, error) {
	now := time.Now().UTC()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caTemplate := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "katl vmtest Kubernetes bundle CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serverTemplate := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: gateway},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP(gateway)},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, &serverTemplate, &caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return cert, caPEM, nil
}

func installKubernetesBundleCA(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, bundle threeControlPlaneKubernetesPayloadBundle) error {
	if len(bundle.CACertPEM) == 0 {
		return nil
	}
	if err := writeNodeFile(ctx, node, bundle.CACertGuestPath, bundle.CACertPEM, 0o644, false); err != nil {
		return err
	}
	commands := []struct {
		name string
		argv []string
	}{
		{name: "set katlc-agent TLS CA", argv: []string{"systemctl", "set-environment", "SSL_CERT_FILE=" + bundle.CACertGuestPath}},
		{name: "restart katlc-agent", argv: []string{"systemctl", "restart", "katlc-agent.service"}},
		{name: "check katlc-agent", argv: []string{"systemctl", "is-active", "--quiet", "katlc-agent.service"}},
	}
	for _, command := range commands {
		result, err := runNodeCommandWithRetry(ctx, node, command.argv, 16<<10)
		if err != nil {
			return fmt.Errorf("%s: %w", command.name, err)
		}
		if result.ExitStatus != 0 {
			return fmt.Errorf("%s: %w", command.name, commandErrorDetail(result))
		}
	}
	return nil
}

func writeKubernetesBundleSourceLog(path string, bundle threeControlPlaneKubernetesPayloadBundle) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("Kubernetes bundle source log path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lines := []string{
		"source=" + bundle.Source,
		"ref=" + bundle.Ref,
		"payloadVersion=" + bundle.PayloadVersion,
		"bundleManifestDigest=" + bundle.BundleManifestDigest,
		"sysextPayloadDigest=" + bundle.SysextPayloadDigest,
		"root=" + bundle.Root,
		"indexPath=" + bundle.IndexPath,
		"catalogPath=" + bundle.CatalogPath,
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func writeThreeControlPlaneInventory(path string, kubernetesVersion string, nodes []vmtest.RunningInstalledRuntimeNode) error {
	if len(nodes) != 3 {
		return fmt.Errorf("three control-plane inventory requires three nodes, got %d", len(nodes))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("controlPlaneEndpoint: \"\"\n")
	b.WriteString("kubernetesVersion: " + kubernetesVersion + "\n")
	b.WriteString("nodes:\n")
	for _, node := range nodes {
		b.WriteString("- name: " + node.Name + "\n")
		b.WriteString("  systemRole: control-plane\n")
		b.WriteString("  access:\n")
		b.WriteString("    method: agent\n")
		b.WriteString("    credentialRef: vsock:" + uint32String(node.VSock.GuestCID) + ":" + uint32String(node.VSock.Port) + "\n")
		b.WriteString("  kubeadmConfig:\n")
		b.WriteString("    ref: control-plane\n")
		b.WriteString("    path: /etc/katl/kubeadm/control-plane/config.yaml\n")
		b.WriteString("    intent: control-plane\n")
		b.WriteString("  kubernetesVersion: " + kubernetesVersion + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writeThreeControlPlaneOperationBackedInventory(path string, kubernetesVersion string, kubernetesBundle threeControlPlaneKubernetesPayloadBundle, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string) error {
	if len(nodes) != 3 {
		return fmt.Errorf("three control-plane inventory requires three nodes, got %d", len(nodes))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("controlPlaneEndpoint: \"\"\n")
	b.WriteString("kubernetesVersion: " + kubernetesVersion + "\n")
	b.WriteString("kubernetesBundle: " + strconv.Quote(kubernetesBundle.Ref) + "\n")
	b.WriteString("nodes:\n")
	for _, node := range nodes {
		b.WriteString("- name: " + node.Name + "\n")
		b.WriteString("  address: " + addresses[node.Name] + "\n")
		b.WriteString("  systemRole: control-plane\n")
		b.WriteString("  access:\n")
		b.WriteString("    method: agent\n")
		b.WriteString("  kubeadmConfig:\n")
		b.WriteString("    ref: control-plane\n")
		b.WriteString("    path: /etc/katl/kubeadm/control-plane/config.yaml\n")
		b.WriteString("    intent: control-plane\n")
		b.WriteString("  kubernetesVersion: " + kubernetesVersion + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

type threeControlPlaneCNISpec struct {
	PodSubnet  string
	PodGateway string
	PeerRoutes []cniPeerRoute
}

type cniPeerRoute struct {
	Subnet  string
	Address string
}

func threeControlPlaneCNISpecs(addresses map[string]string) map[string]threeControlPlaneCNISpec {
	return map[string]threeControlPlaneCNISpec{
		"cp-1": {
			PodSubnet:  "10.244.0.0/24",
			PodGateway: "10.244.0.1",
			PeerRoutes: []cniPeerRoute{
				{Subnet: "10.244.1.0/24", Address: addresses["cp-2"]},
				{Subnet: "10.244.2.0/24", Address: addresses["cp-3"]},
			},
		},
		"cp-2": {
			PodSubnet:  "10.244.1.0/24",
			PodGateway: "10.244.1.1",
			PeerRoutes: []cniPeerRoute{
				{Subnet: "10.244.0.0/24", Address: addresses["cp-1"]},
				{Subnet: "10.244.2.0/24", Address: addresses["cp-3"]},
			},
		},
		"cp-3": {
			PodSubnet:  "10.244.2.0/24",
			PodGateway: "10.244.2.1",
			PeerRoutes: []cniPeerRoute{
				{Subnet: "10.244.0.0/24", Address: addresses["cp-1"]},
				{Subnet: "10.244.1.0/24", Address: addresses["cp-2"]},
			},
		},
	}
}

func stageThreeControlPlaneCNIFixtures(ctx context.Context, repo string, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string) (map[string]nodeCNIFixture, error) {
	byName := map[string]vmtest.RunningInstalledRuntimeNode{}
	for _, node := range nodes {
		byName[node.Name] = node
	}
	source := filepath.Join(repo, "internal", "vmtest", "scenarios", "testdata", "bootstrap", "bridge-cni.conflist")
	fixtures := map[string]nodeCNIFixture{}
	for nodeName, spec := range threeControlPlaneCNISpecs(addresses) {
		node, ok := byName[nodeName]
		if !ok {
			return nil, fmt.Errorf("missing running node %s for CNI fixture", nodeName)
		}
		if len(spec.PeerRoutes) == 0 {
			return nil, fmt.Errorf("missing peer routes for %s CNI fixture", nodeName)
		}
		firstPeer := spec.PeerRoutes[0]
		fixture, err := stageNodeCNIFixture(ctx, node, source, spec.PodSubnet, spec.PodGateway, firstPeer.Subnet, firstPeer.Address)
		if err != nil {
			return nil, fmt.Errorf("stage %s CNI: %w", nodeName, err)
		}
		for _, peer := range spec.PeerRoutes[1:] {
			argv := []string{"ip", "route", "replace", peer.Subnet, "via", peer.Address}
			result, err := runNodeCommand(ctx, node, argv, 32<<10)
			if err != nil {
				return nil, fmt.Errorf("stage %s CNI peer route %s: %w", nodeName, peer.Subnet, err)
			}
			if result.ExitStatus != 0 {
				return nil, fmt.Errorf("stage %s CNI peer route %s: %w", nodeName, peer.Subnet, commandErrorDetail(result))
			}
		}
		fixtures[nodeName] = fixture
	}
	return fixtures, nil
}

type threeControlPlaneArtifactManifest struct {
	VMTestRun                 string                                    `json:"vmtestRun,omitempty"`
	WorldManifest             string                                    `json:"worldManifest,omitempty"`
	HostCapabilities          string                                    `json:"hostCapabilities,omitempty"`
	MkosiArtifactIndex        string                                    `json:"mkosiArtifactIndex,omitempty"`
	NodeRunDirs               map[string]string                         `json:"nodeRunDirs"`
	NodeScenarios             map[string]string                         `json:"nodeScenarios,omitempty"`
	NodeResults               map[string]string                         `json:"nodeResults,omitempty"`
	LaunchCommands            map[string]string                         `json:"launchCommands,omitempty"`
	DomainXMLs                map[string]string                         `json:"domainXMLs,omitempty"`
	InstalledRuntimeInputs    map[string]string                         `json:"installedRuntimeInputs,omitempty"`
	VSockTranscripts          map[string]string                         `json:"vsockTranscripts,omitempty"`
	LibvirtLeases             map[string]string                         `json:"libvirtLeases,omitempty"`
	NodeDomains               map[string]string                         `json:"nodeDomains,omitempty"`
	NodeMACs                  map[string]string                         `json:"nodeMACs,omitempty"`
	NodeIPs                   map[string]string                         `json:"nodeIPs,omitempty"`
	FixtureInputs             map[string]nodeFixtureInput               `json:"fixtureInputs,omitempty"`
	FixtureProducerScenarios  map[string]string                         `json:"fixtureProducerScenarios,omitempty"`
	FixtureProducerResults    map[string]string                         `json:"fixtureProducerResults,omitempty"`
	Inventory                 string                                    `json:"inventory"`
	Kubeconfig                string                                    `json:"kubeconfig"`
	KubeconfigMetadata        string                                    `json:"kubeconfigMetadata,omitempty"`
	BootstrapStdout           string                                    `json:"bootstrapStdout"`
	BootstrapStderr           string                                    `json:"bootstrapStderr"`
	BootstrapFixture          *bootstrapFixtureInputs                   `json:"bootstrapFixture,omitempty"`
	CNIFixtures               map[string]nodeCNIFixture                 `json:"cniFixtures,omitempty"`
	ImageFixtures             map[string][]nodeImageFixture             `json:"imageFixtures,omitempty"`
	KubernetesPayloadBundle   *threeControlPlaneKubernetesPayloadBundle `json:"kubernetesPayloadBundle,omitempty"`
	KubernetesVersionEvidence map[string]string                         `json:"kubernetesVersionEvidence,omitempty"`
	ReleaseWorkloadStack      *releaseWorkloadStackEvidence             `json:"releaseWorkloadStack,omitempty"`
	KubectlOutput             string                                    `json:"kubectlOutput"`
	KubectlDiagnostics        map[string]string                         `json:"kubectlDiagnostics,omitempty"`
	EtcdReport                string                                    `json:"etcdReport"`
	Transcripts               map[string]string                         `json:"transcripts"`
	EtcdTranscripts           map[string]string                         `json:"etcdTranscripts"`
	OperationEvidence         map[string]string                         `json:"operationEvidence,omitempty"`
	SerialLogs                map[string]string                         `json:"serialLogs,omitempty"`
	NetworkLeases             string                                    `json:"networkLeases,omitempty"`
	Diagnostics               map[string]string                         `json:"diagnostics,omitempty"`
}

type releaseWorkloadStackEvidence struct {
	Manifest              string            `json:"manifest"`
	CRDManifest           string            `json:"crdManifest"`
	ApplyOutput           string            `json:"applyOutput"`
	CRDApplyOutput        string            `json:"crdApplyOutput"`
	CiliumPods            string            `json:"ciliumPods"`
	CoreDNSPods           string            `json:"coreDNSPods"`
	EnvoyGatewayPods      string            `json:"envoyGatewayPods"`
	EchoPods              string            `json:"echoPods"`
	GatewayClasses        string            `json:"gatewayClasses"`
	GatewayAPIResources   string            `json:"gatewayAPIResources"`
	GatewayURL            string            `json:"gatewayURL"`
	GatewayResponse       string            `json:"gatewayResponse"`
	AdditionalDiagnostics map[string]string `json:"additionalDiagnostics,omitempty"`
}

func writeThreeControlPlaneSmokeArtifactManifest(result vmtest.Result, inputs threeControlPlaneSmokeInputs, transcriptDir, etcdTranscriptDir string, nodes []vmtest.RunningInstalledRuntimeNode, bootstrapFixture bootstrapFixtureInputs, kubernetesBundle threeControlPlaneKubernetesPayloadBundle, cniFixtures map[string]nodeCNIFixture, imageFixtures map[string][]nodeImageFixture, workloadStack *releaseWorkloadStackEvidence) error {
	var bundle *threeControlPlaneKubernetesPayloadBundle
	if strings.TrimSpace(kubernetesBundle.Ref) != "" {
		copy := kubernetesBundle
		copy.CACertPEM = nil
		bundle = &copy
	}
	return writeThreeControlPlaneArtifactManifest(filepath.Join(result.ManifestDir, "three-control-plane-artifacts.json"), threeControlPlaneArtifactManifest{
		VMTestRun:                 inputs.WorldProvenance.VMTestRun,
		WorldManifest:             inputs.WorldProvenance.WorldManifest,
		HostCapabilities:          inputs.WorldProvenance.HostCapabilities,
		MkosiArtifactIndex:        inputs.WorldProvenance.MkosiArtifactIndex,
		NodeRunDirs:               nodeRunDirs(nodes),
		NodeScenarios:             nodeScenarioPaths(nodes),
		NodeResults:               nodeResultPaths(nodes),
		LaunchCommands:            launchCommandPaths(nodes),
		DomainXMLs:                domainXMLPaths(nodes),
		InstalledRuntimeInputs:    installedRuntimeInputPaths(nodes),
		VSockTranscripts:          vsockTranscriptPaths(nodes),
		LibvirtLeases:             libvirtLeasePaths(nodes),
		NodeDomains:               nodeDomainNames(nodes),
		NodeMACs:                  nodeMACAddresses(nodes),
		NodeIPs:                   nodeIPAddresses(nodes),
		FixtureInputs:             threeControlPlaneFixtureInputs(inputs.CP1Disk, inputs.CP1DiskFormat, inputs.CP2Disk, inputs.CP2DiskFormat, inputs.CP3Disk, inputs.CP3DiskFormat, inputs.CP1ESP, inputs.CP2ESP, inputs.CP3ESP, inputs.CP1Fixture, inputs.CP2Fixture, inputs.CP3Fixture, inputs.CP1Metadata, inputs.CP2Metadata, inputs.CP3Metadata),
		FixtureProducerScenarios:  inputs.WorldProvenance.FixtureProducerScenarios,
		FixtureProducerResults:    inputs.WorldProvenance.FixtureProducerResults,
		Inventory:                 filepath.Join(result.ManifestDir, "bootstrap-inventory.yaml"),
		Kubeconfig:                filepath.Join(result.RunDir, "operator-kubeconfig.yaml"),
		KubeconfigMetadata:        filepath.Join(result.RunDir, "operator-kubeconfig-metadata.json"),
		BootstrapStdout:           filepath.Join(result.RunDir, "katlctl-bootstrap.stdout"),
		BootstrapStderr:           filepath.Join(result.RunDir, "katlctl-bootstrap.stderr"),
		BootstrapFixture:          bootstrapFixture.manifestValue(),
		CNIFixtures:               cniFixtures,
		ImageFixtures:             imageFixtures,
		KubernetesPayloadBundle:   bundle,
		KubernetesVersionEvidence: kubernetesVersionEvidencePaths(filepath.Join(result.RunDir, "kubernetes-version-evidence"), nodes),
		ReleaseWorkloadStack:      workloadStack,
		KubectlOutput:             filepath.Join(result.RunDir, "kubectl-get-nodes.txt"),
		KubectlDiagnostics:        kubectlDiagnosticPaths(result.RunDir),
		EtcdReport:                filepath.Join(result.RunDir, "etcd-report.json"),
		Transcripts:               transcriptPaths(transcriptDir, nodes),
		EtcdTranscripts:           transcriptPaths(etcdTranscriptDir, nodes),
		OperationEvidence:         operationEvidencePaths(filepath.Join(result.RunDir, "operation-evidence"), nodes),
		SerialLogs:                serialLogPaths(nodes),
		NetworkLeases:             inputs.WorldProvenance.NetworkLeaseFile,
		Diagnostics:               diagnosticSummaryPaths(nodes),
	})
}

func kubernetesVersionEvidencePaths(evidenceDir string, nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	paths := map[string]string{}
	for _, node := range nodes {
		paths[node.Name] = filepath.Join(evidenceDir, node.Name, "kubernetes-versions.json")
	}
	return paths
}

func writeThreeControlPlaneArtifactManifest(path string, manifest threeControlPlaneArtifactManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func proveReleaseWorkloadStack(ctx context.Context, repo, runDir, manifestDir, kubeconfigPath, gatewayAddress string) (*releaseWorkloadStackEvidence, error) {
	source := filepath.Join(repo, "internal", "vmtest", "scenarios", "testdata", "bootstrap", "release-workload-stack.yaml")
	data, err := os.ReadFile(source)
	if err != nil {
		return nil, fmt.Errorf("read release workload stack manifest: %w", err)
	}
	target := filepath.Join(manifestDir, "release-workload-stack.yaml")
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return nil, fmt.Errorf("stage release workload stack manifest: %w", err)
	}
	crdSource := filepath.Join(repo, "internal", "vmtest", "scenarios", "testdata", "bootstrap", "release-workload-stack-crds.yaml")
	crdData, err := os.ReadFile(crdSource)
	if err != nil {
		return nil, fmt.Errorf("read release workload stack CRD manifest: %w", err)
	}
	crdTarget := filepath.Join(manifestDir, "release-workload-stack-crds.yaml")
	if err := os.WriteFile(crdTarget, crdData, 0o644); err != nil {
		return nil, fmt.Errorf("stage release workload stack CRD manifest: %w", err)
	}
	evidenceDir := filepath.Join(runDir, "release-workload-stack")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return nil, err
	}
	evidence := &releaseWorkloadStackEvidence{
		Manifest:              target,
		CRDManifest:           crdTarget,
		ApplyOutput:           filepath.Join(evidenceDir, "kubectl-apply.txt"),
		CRDApplyOutput:        filepath.Join(evidenceDir, "kubectl-apply-crds.txt"),
		CiliumPods:            filepath.Join(evidenceDir, "kubectl-get-cilium-pods.txt"),
		CoreDNSPods:           filepath.Join(evidenceDir, "kubectl-get-coredns-pods.txt"),
		EnvoyGatewayPods:      filepath.Join(evidenceDir, "kubectl-get-envoy-gateway-pods.txt"),
		EchoPods:              filepath.Join(evidenceDir, "kubectl-get-echo-pods.txt"),
		GatewayClasses:        filepath.Join(evidenceDir, "kubectl-get-gateway-classes.txt"),
		GatewayAPIResources:   filepath.Join(evidenceDir, "kubectl-get-gateway-api.txt"),
		GatewayURL:            "http://" + net.JoinHostPort(gatewayAddress, "31080") + "/hostname",
		GatewayResponse:       filepath.Join(evidenceDir, "gateway-response.txt"),
		AdditionalDiagnostics: releaseWorkloadDiagnosticPaths(evidenceDir),
	}
	if err := runKubectlCapture(ctx, kubeconfigPath, evidence.CRDApplyOutput, "apply", "-f", crdTarget); err != nil {
		return evidence, err
	}
	for _, crd := range []string{
		"gatewayclasses.gateway.networking.k8s.io",
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
	} {
		if err := runKubectlCapture(ctx, kubeconfigPath, filepath.Join(evidenceDir, "kubectl-wait-crd-"+safeEvidenceName(crd)+".txt"), "wait", "--for=condition=Established", "crd/"+crd, "--timeout=2m"); err != nil {
			return evidence, err
		}
	}
	if err := runKubectlCapture(ctx, kubeconfigPath, evidence.ApplyOutput, "apply", "-f", target); err != nil {
		return evidence, err
	}
	waits := [][]string{
		{"-n", "kube-system", "rollout", "status", "daemonset/cilium", "--timeout=5m"},
		{"-n", "kube-system", "rollout", "status", "deployment/cilium-operator", "--timeout=5m"},
		{"-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=5m"},
		{"-n", "envoy-gateway-system", "rollout", "status", "deployment/envoy-gateway", "--timeout=5m"},
		{"-n", "katl-vmtest", "rollout", "status", "deployment/echo", "--timeout=5m"},
		{"-n", "kube-system", "wait", "--for=condition=Ready", "pod", "-l", "k8s-app=kube-dns", "--timeout=5m"},
		{"-n", "katl-vmtest", "wait", "--for=condition=Ready", "pod", "-l", "app.kubernetes.io/name=echo", "--timeout=5m"},
	}
	for _, args := range waits {
		if err := runKubectlCapture(ctx, kubeconfigPath, filepath.Join(evidenceDir, "kubectl-wait-"+safeEvidenceName(strings.Join(args, "-"))+".txt"), args...); err != nil {
			collectReleaseWorkloadDiagnostics(ctx, kubeconfigPath, evidence)
			return evidence, err
		}
	}
	captures := []struct {
		path string
		args []string
	}{
		{evidence.CiliumPods, []string{"-n", "kube-system", "get", "pods", "-l", "k8s-app=cilium", "-o", "wide"}},
		{evidence.CoreDNSPods, []string{"-n", "kube-system", "get", "pods", "-l", "k8s-app=kube-dns", "-o", "wide"}},
		{evidence.EnvoyGatewayPods, []string{"-n", "envoy-gateway-system", "get", "pods", "-o", "wide"}},
		{evidence.EchoPods, []string{"-n", "katl-vmtest", "get", "pods", "-o", "wide"}},
		{evidence.GatewayClasses, []string{"get", "gatewayclass", "-o", "yaml"}},
		{evidence.GatewayAPIResources, []string{"-n", "katl-vmtest", "get", "gateway,httproute", "-o", "yaml"}},
	}
	for _, capture := range captures {
		if err := runKubectlCapture(ctx, kubeconfigPath, capture.path, capture.args...); err != nil {
			collectReleaseWorkloadDiagnostics(ctx, kubeconfigPath, evidence)
			return evidence, err
		}
	}
	response, err := waitForHTTPText(ctx, evidence.GatewayURL, 5*time.Minute)
	if err != nil {
		collectReleaseWorkloadDiagnostics(ctx, kubeconfigPath, evidence)
		return evidence, err
	}
	if err := os.WriteFile(evidence.GatewayResponse, []byte(response), 0o644); err != nil {
		return evidence, err
	}
	return evidence, nil
}

func runKubectlCapture(ctx context.Context, kubeconfigPath, outputPath string, args ...string) error {
	argv := append([]string{selectedKubectl(), "--kubeconfig", kubeconfigPath}, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var combined bytes.Buffer
	combined.WriteString("$ " + strings.Join(argv, " ") + "\n")
	combined.Write(stdout.Bytes())
	if stderr.Len() > 0 {
		combined.WriteString("\n[stderr]\n")
		combined.Write(stderr.Bytes())
	}
	_ = os.WriteFile(outputPath, combined.Bytes(), 0o644)
	if err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

func waitForHTTPText(ctx context.Context, url string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			data, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				return "", readErr
			}
			body := string(data)
			if got := resp.Header.Get("X-Katl-VMTest-Gateway"); got != "envoy-gateway-fixture" {
				lastErr = fmt.Errorf("gateway header %q", got)
			} else if hostname := strings.TrimSpace(body); !strings.HasPrefix(hostname, "echo-") {
				lastErr = fmt.Errorf("gateway response body %q does not look like an echo pod hostname", hostname)
			} else {
				return "X-Katl-VMTest-Gateway: " + got + "\n\n" + body, nil
			}
		} else if err == nil {
			lastErr = fmt.Errorf("status %s", resp.Status)
			_ = resp.Body.Close()
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("GET %s did not succeed within %s: %w", url, timeout, lastErr)
		case <-time.After(time.Second):
		}
	}
}

func releaseWorkloadDiagnosticPaths(dir string) map[string]string {
	return map[string]string{
		"events":        filepath.Join(dir, "kubectl-get-events.txt"),
		"allPods":       filepath.Join(dir, "kubectl-get-pods-all-namespaces.txt"),
		"services":      filepath.Join(dir, "kubectl-get-services-all-namespaces.txt"),
		"gatewaySystem": filepath.Join(dir, "kubectl-describe-envoy-gateway.txt"),
	}
}

func collectReleaseWorkloadDiagnostics(ctx context.Context, kubeconfigPath string, evidence *releaseWorkloadStackEvidence) {
	if evidence == nil {
		return
	}
	commands := map[string][]string{
		"events":        {"get", "events", "-A", "--sort-by=.lastTimestamp"},
		"allPods":       {"get", "pods", "-A", "-o", "wide"},
		"services":      {"get", "svc", "-A", "-o", "wide"},
		"gatewaySystem": {"-n", "envoy-gateway-system", "describe", "deployment/envoy-gateway"},
	}
	for name, args := range commands {
		if path := evidence.AdditionalDiagnostics[name]; path != "" {
			_ = runKubectlCapture(ctx, kubeconfigPath, path, args...)
		}
	}
}

func safeEvidenceName(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 80 {
		out = out[:80]
	}
	if out == "" {
		return "command"
	}
	return out
}

func collectKubernetesVersionEvidence(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir string, payloadVersion string) (string, error) {
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return "", err
	}
	commands := map[string][]string{
		"kubeadm": {"kubeadm", "version", "-o", "short"},
		"kubelet": {"kubelet", "--version"},
		"kubectl": {"kubectl", "version", "--client=true", "--output=yaml"},
	}
	evidence := nodeLocalStatusEvidence{
		Node:    node.Name,
		Results: make(map[string]nodeCommandEvidence, len(commands)),
	}
	for name, argv := range commands {
		result, err := runNodeCommand(ctx, node, argv, 256<<10)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		stdout := string(result.Stdout)
		stderr := string(result.Stderr)
		if result.ExitStatus != 0 {
			return "", fmt.Errorf("%s exited %d: %s%s", name, result.ExitStatus, stdout, stderr)
		}
		if !strings.Contains(stdout, payloadVersion) && !strings.Contains(stderr, payloadVersion) {
			return "", fmt.Errorf("%s output does not contain selected payload version %s: stdout=%q stderr=%q", name, payloadVersion, stdout, stderr)
		}
		evidence.Results[name] = nodeCommandEvidence{
			Argv:       argv,
			ExitStatus: result.ExitStatus,
			Stdout:     stdout,
			Stderr:     stderr,
		}
	}
	hostPath := filepath.Join(evidenceDir, "kubernetes-versions.json")
	return hostPath, writeTwoNodeDiagnosticJSON(hostPath, evidence)
}

func assertOperationKubernetesBundle(t *testing.T, record operation.OperationRecord, bundle threeControlPlaneKubernetesPayloadBundle) {
	t.Helper()
	if record.BootstrapRequest == nil {
		t.Fatalf("operation %s missing bootstrap request", record.OperationID)
	}
	req := record.BootstrapRequest
	if req.KubernetesPayloadVersion != bundle.PayloadVersion ||
		req.KubernetesBundleSource != bundle.Source ||
		req.KubernetesBundleRef != bundle.Ref ||
		!resolvedBundleDigestMatches(req.KubernetesBundleManifestDigest, bundle.BundleManifestDigest) ||
		!resolvedBundleDigestMatches(req.KubernetesSysextPayloadDigest, bundle.SysextPayloadDigest) {
		t.Fatalf("operation %s Kubernetes bundle request = %#v, want %#v", record.OperationID, req, bundle)
	}
	if record.CandidateGenerationID == "" {
		t.Fatalf("operation %s missing candidate generation ID", record.OperationID)
	}
}

func assertGenerationKubernetesBundle(t *testing.T, ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir string, record operation.OperationRecord, bundle threeControlPlaneKubernetesPayloadBundle) {
	t.Helper()
	_, generationRecord, err := collectGenerationEvidence(ctx, node, evidenceDir, record.CandidateGenerationID)
	if err != nil {
		t.Fatalf("collect %s generation evidence: %v", node.Name, err)
	}
	var found bool
	for _, ref := range generationRecord.Spec.Sysexts {
		if ref.Name != sysextcatalog.KubernetesName {
			continue
		}
		found = true
		if ref.PayloadVersion != bundle.PayloadVersion || ref.ArtifactVersion == "" ||
			!resolvedBundleDigestMatches(ref.SHA256, bundle.SysextPayloadDigest) {
			t.Fatalf("%s Kubernetes sysext ref = %#v, want bundle %#v", node.Name, ref, bundle)
		}
	}
	if !found {
		t.Fatalf("%s generation %s missing Kubernetes sysext ref: %#v", node.Name, record.CandidateGenerationID, generationRecord.Spec.Sysexts)
	}
}

func resolvedBundleDigestMatches(actual, expected string) bool {
	actual = strings.TrimPrefix(strings.TrimSpace(actual), "sha256:")
	expected = strings.TrimPrefix(strings.TrimSpace(expected), "sha256:")
	return actual != "" && (expected == "" || actual == expected)
}

func threeControlPlaneFixtureInputs(cp1Disk, cp1Format, cp2Disk, cp2Format, cp3Disk, cp3Format, cp1ESP, cp2ESP, cp3ESP, cp1Fixture, cp2Fixture, cp3Fixture, cp1Metadata, cp2Metadata, cp3Metadata string) map[string]nodeFixtureInput {
	return map[string]nodeFixtureInput{
		"cp-1": fixtureInput(cp1Disk, cp1Format, cp1ESP, cp1Fixture, cp1Metadata),
		"cp-2": fixtureInput(cp2Disk, cp2Format, cp2ESP, cp2Fixture, cp2Metadata),
		"cp-3": fixtureInput(cp3Disk, cp3Format, cp3ESP, cp3Fixture, cp3Metadata),
	}
}

func assertThreeControlPlaneBootstrapPhases(t *testing.T, output string) {
	t.Helper()
	for _, want := range []string{
		"katlctl cluster bootstrap init-node=cp-1",
		"phase=bootstrap-init node=cp-1 status=passed",
		"phase=control-plane-join node=cp-2 status=passed",
		"phase=control-plane-join node=cp-3 status=passed",
		"phase=kubeconfig status=passed",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("katlctl output missing %q:\n%s", want, output)
		}
	}
	for _, forbidden := range []string{
		"phase=worker-join",
		"phase=kubeadm-init node=cp-2",
		"phase=kubeadm-init node=cp-3",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("katlctl output contains forbidden phase %q:\n%s", forbidden, output)
		}
	}
}

func verifyBootstrapTranscripts(transcriptDir string, nodes []string) error {
	for _, node := range nodes {
		path := twoNodeBootstrapTranscriptPath(transcriptDir, node)
		entries, err := readTranscriptFile(path)
		if err != nil {
			return fmt.Errorf("%s transcript %s: %w", node, path, err)
		}
		var runCommand, readFile, writeFile, sensitiveCommand, sensitiveFile, sensitiveWriteFile bool
		for _, entry := range entries {
			switch entry.Method {
			case "RunCommand":
				runCommand = true
				if entry.SensitiveOutput || entry.Redaction == "output" {
					sensitiveCommand = true
				}
			case "ReadFile":
				readFile = true
				if entry.SensitiveOutput || (entry.Redaction != "" && entry.Redaction != "none") {
					sensitiveFile = true
				}
			case "WriteFile":
				writeFile = true
				if entry.SensitiveOutput || (entry.Redaction != "" && entry.Redaction != "none") {
					sensitiveWriteFile = true
				}
			}
		}
		if !runCommand {
			return fmt.Errorf("%s transcript has no RunCommand entry", node)
		}
		if !readFile {
			return fmt.Errorf("%s transcript has no ReadFile entry", node)
		}
		if !sensitiveCommand {
			return fmt.Errorf("%s transcript has no redacted sensitive command entry", node)
		}
		if !sensitiveFile {
			return fmt.Errorf("%s transcript has no sensitive file entry", node)
		}
		if node != "cp-1" {
			if !writeFile {
				return fmt.Errorf("%s transcript has no WriteFile entry", node)
			}
			if !sensitiveWriteFile {
				return fmt.Errorf("%s transcript has no sensitive write file entry", node)
			}
		}
		if err := verifyThreeControlPlaneKubeadmTranscript(node, entries); err != nil {
			return fmt.Errorf("%s transcript: %w", node, err)
		}
	}
	return nil
}

func verifyThreeControlPlaneKubeadmTranscript(node string, entries []transcriptEntry) error {
	switch node {
	case "cp-1":
		if transcriptHasCommand(entries, "kubeadm", "join") {
			return errors.New("unexpected kubeadm join command on init control-plane")
		}
		if !transcriptHasCommand(entries, "kubeadm", "init") {
			return errors.New("missing kubeadm init command")
		}
		if !transcriptHasCommandFlagValue(entries, "kubeadm", "init", "--config", "/var/lib/katl/test-artifacts/kubeadm-init-cp-1.yaml") {
			return errors.New("kubeadm init command missing generated control-plane config path")
		}
		if !transcriptHasCommand(entries, "kubeadm", "init", "phase", "upload-certs") {
			return errors.New("missing kubeadm certificate upload command")
		}
		if !transcriptHasCommandArgAfterPrefix(entries, []string{"kubeadm", "init", "phase", "upload-certs"}, "--upload-certs") {
			return errors.New("kubeadm certificate upload command missing --upload-certs")
		}
	case "cp-2", "cp-3":
		if transcriptHasCommand(entries, "kubeadm", "init") {
			return errors.New("unexpected kubeadm init command on joining control-plane")
		}
		if !transcriptHasCommand(entries, "kubeadm", "join") {
			return errors.New("missing kubeadm control-plane join command")
		}
		if !transcriptHasCommandFlagValue(entries, "kubeadm", "join", "--config", "/var/lib/katl/test-artifacts/kubeadm-join-"+node+".yaml") {
			return errors.New("kubeadm control-plane join command missing generated control-plane config path")
		}
		if transcriptHasCommandArg(entries, "kubeadm", "join", "--control-plane") {
			return errors.New("kubeadm control-plane join command must not include --control-plane")
		}
		if transcriptHasCommandArg(entries, "kubeadm", "join", "--certificate-key") {
			return errors.New("kubeadm control-plane join command must not include --certificate-key")
		}
	}
	return nil
}

func transcriptHasCommandArg(entries []transcriptEntry, first, second, arg string) bool {
	for _, entry := range entries {
		if entry.Method != "RunCommand" || len(entry.Argv) < 2 || entry.Argv[0] != first || entry.Argv[1] != second {
			continue
		}
		for _, got := range entry.Argv[2:] {
			if got == arg {
				return true
			}
		}
	}
	return false
}

func transcriptHasCommandArgAfterPrefix(entries []transcriptEntry, prefix []string, arg string) bool {
	for _, entry := range entries {
		if entry.Method != "RunCommand" || len(entry.Argv) < len(prefix) {
			continue
		}
		matched := true
		for i, want := range prefix {
			if entry.Argv[i] != want {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		for _, got := range entry.Argv[len(prefix):] {
			if got == arg {
				return true
			}
		}
	}
	return false
}

type threeControlPlaneEtcdReport struct {
	FailureSummary string                        `json:"failureSummary,omitempty"`
	StaticPods     []controlPlaneStaticPodReport `json:"staticPods"`
	Health         cluster.EtcdReport            `json:"health"`
	Snapshot       cluster.EtcdSnapshotReport    `json:"snapshot"`
	Transcript     string                        `json:"transcript"`
}

func writeThreeControlPlaneEtcdReport(path string, report threeControlPlaneEtcdReport) error {
	return writeTwoNodeDiagnosticJSON(path, report)
}

type controlPlaneStaticPodReport struct {
	Node      string            `json:"node"`
	Container map[string]string `json:"container"`
}

func verifyThreeControlPlaneEtcd(ctx context.Context, transcriptDir string, nodes []vmtest.RunningInstalledRuntimeNode) (threeControlPlaneEtcdReport, error) {
	return verifyThreeControlPlaneEtcdAt(ctx, transcriptDir, nodes, "/var/lib/etcd/katl-snapshots/three-control-plane.db")
}

func verifyThreeControlPlaneEtcdAt(ctx context.Context, transcriptDir string, nodes []vmtest.RunningInstalledRuntimeNode, snapshotPath string) (threeControlPlaneEtcdReport, error) {
	planned := plannedControlPlaneNodes(nodes)
	transport := vmtestNodeTransport{Nodes: nodeMap(nodes), TranscriptDir: transcriptDir}
	staticPods, err := verifyControlPlaneStaticPods(ctx, transport, planned)
	if err != nil {
		return threeControlPlaneEtcdReport{StaticPods: staticPods}, err
	}
	checker := cluster.EtcdChecker{Transport: transport}
	report, err := checker.Check(ctx, planned["cp-1"])
	if err != nil {
		return threeControlPlaneEtcdReport{StaticPods: staticPods, Transcript: twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1")}, err
	}
	if !report.Healthy {
		return threeControlPlaneEtcdReport{StaticPods: staticPods, Health: report, Transcript: twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1")}, fmt.Errorf("etcd health failed: %s", report.Diagnostics)
	}
	for _, name := range []string{"cp-1", "cp-2", "cp-3"} {
		if !report.HasMember(name) {
			return threeControlPlaneEtcdReport{StaticPods: staticPods, Health: report, Transcript: twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1")}, fmt.Errorf("etcd member %s missing", name)
		}
	}
	if report.Quorum != 2 {
		return threeControlPlaneEtcdReport{StaticPods: staticPods, Health: report, Transcript: twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1")}, fmt.Errorf("etcd quorum = %d, want 2", report.Quorum)
	}
	snapshot, err := checker.CreateSnapshot(ctx, planned["cp-1"], snapshotPath)
	if err != nil {
		return threeControlPlaneEtcdReport{StaticPods: staticPods, Health: report, Transcript: twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1")}, err
	}
	if len(snapshot.Diagnostics) != 0 {
		return threeControlPlaneEtcdReport{StaticPods: staticPods, Health: report, Snapshot: snapshot, Transcript: twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1")}, fmt.Errorf("etcd snapshot failed: %s", snapshot.Diagnostics)
	}
	if snapshot.Hash == "" || snapshot.Revision == "" || snapshot.TotalKeys == "" {
		return threeControlPlaneEtcdReport{StaticPods: staticPods, Health: report, Snapshot: snapshot, Transcript: twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1")}, errors.New("etcd snapshot status missing hash, revision, or key count")
	}
	return threeControlPlaneEtcdReport{StaticPods: staticPods, Health: report, Snapshot: snapshot, Transcript: twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1")}, nil
}

func verifyControlPlaneStaticPods(ctx context.Context, transport vmtestNodeTransport, planned map[string]inventory.PlannedNode) ([]controlPlaneStaticPodReport, error) {
	var reports []controlPlaneStaticPodReport
	for _, nodeName := range []string{"cp-1", "cp-2", "cp-3"} {
		node, ok := planned[nodeName]
		if !ok {
			return reports, fmt.Errorf("planned node %s missing", nodeName)
		}
		report := controlPlaneStaticPodReport{Node: nodeName, Container: map[string]string{}}
		for _, podName := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "etcd"} {
			result, err := transport.RunCommand(ctx, node, readiness.CommandRequest{
				Argv:        []string{"crictl", "ps", "--name", podName, "--state", "Running", "--quiet"},
				StdoutLimit: 4096,
			})
			if err != nil {
				return reports, fmt.Errorf("%s static pod %s: %w", nodeName, podName, err)
			}
			if result.ExitStatus != 0 {
				return reports, fmt.Errorf("%s static pod %s check exited %d: %s", nodeName, podName, result.ExitStatus, strings.TrimSpace(result.Stderr))
			}
			containerID := strings.Fields(result.Stdout)
			if len(containerID) == 0 {
				return reports, fmt.Errorf("%s static pod %s is not running", nodeName, podName)
			}
			report.Container[podName] = containerID[0]
		}
		reports = append(reports, report)
	}
	return reports, nil
}

type vmtestNodeTransport struct {
	Nodes         map[string]vmtest.RunningInstalledRuntimeNode
	TranscriptDir string
}

func (t vmtestNodeTransport) RunCommand(ctx context.Context, node inventory.PlannedNode, req readiness.CommandRequest) (readiness.CommandResult, error) {
	result, err := retryAgentOp(ctx, t, node, safeRetryCommand(req.Argv), func(opCtx context.Context, client *vmtest.AgentClient) (*vmtestpb.CommandResult, error) {
		return client.RunCommand(opCtx, &vmtestpb.RunCommandRequest{
			Argv:             req.Argv,
			StdoutLimit:      req.StdoutLimit,
			StderrLimit:      req.StderrLimit,
			SensitiveOutput:  req.SensitiveOutput,
			WorkingDirectory: "",
		})
	}, req.Timeout)
	if err != nil {
		return readiness.CommandResult{}, err
	}
	return readiness.CommandResult{
		ExitStatus:      result.ExitStatus,
		Stdout:          string(result.Stdout),
		Stderr:          string(result.Stderr),
		StdoutTruncated: result.StdoutTruncated,
		StderrTruncated: result.StderrTruncated,
	}, nil
}

func (t vmtestNodeTransport) ReadFile(ctx context.Context, node inventory.PlannedNode, req readiness.FileRequest) (readiness.FileResult, error) {
	result, err := retryAgentOp(ctx, t, node, true, func(opCtx context.Context, client *vmtest.AgentClient) (*vmtestpb.FileResult, error) {
		return client.ReadFile(opCtx, &vmtestpb.ReadFileRequest{
			Path:      req.Path,
			MaxBytes:  req.MaxBytes,
			Sensitive: req.Sensitive,
		})
	}, req.Timeout)
	if err != nil {
		return readiness.FileResult{}, err
	}
	return readiness.FileResult{Content: result.Content, Truncated: result.Truncated, Redaction: result.Redaction}, nil
}

func (t vmtestNodeTransport) WriteFile(ctx context.Context, node inventory.PlannedNode, req readiness.WriteFileRequest) (readiness.WriteFileResult, error) {
	result, err := retryAgentOp(ctx, t, node, true, func(opCtx context.Context, client *vmtest.AgentClient) (*vmtestpb.WriteFileResult, error) {
		return client.WriteFile(opCtx, &vmtestpb.WriteFileRequest{
			Path:      req.Path,
			Content:   req.Content,
			Mode:      req.Mode,
			Sensitive: req.Sensitive,
		})
	}, req.Timeout)
	if err != nil {
		return readiness.WriteFileResult{}, err
	}
	return readiness.WriteFileResult{SizeBytes: result.SizeBytes, Redaction: result.Redaction}, nil
}

func retryAgentOp[T any](ctx context.Context, transport vmtestNodeTransport, node inventory.PlannedNode, retry bool, op func(context.Context, *vmtest.AgentClient) (T, error), timeout time.Duration) (T, error) {
	var zero T
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	attempts := 1
	if retry {
		attempts = 3
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		client, err := transport.client(ctx, node)
		if err != nil {
			lastErr = err
		} else {
			result, err := op(ctx, client)
			_ = client.Close()
			if err == nil {
				return result, nil
			}
			lastErr = err
		}
		if attempt == attempts-1 || !transientAgentTransportError(lastErr) {
			return zero, lastErr
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return zero, lastErr
}

func safeRetryCommand(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	switch argv[0] {
	case "systemctl", "test", "findmnt":
		return true
	case "kubeadm":
		return len(argv) >= 2 && argv[1] == "version"
	case "crictl":
		return len(argv) >= 2 && (argv[1] == "info" || argv[1] == "ps")
	case "kubectl":
		for _, arg := range argv[1:] {
			if arg == "get" {
				return true
			}
		}
	}
	return false
}

func transientAgentTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "connection reset by peer") || strings.Contains(text, "broken pipe")
}

func (t vmtestNodeTransport) client(ctx context.Context, node inventory.PlannedNode) (*vmtest.AgentClient, error) {
	running, ok := t.Nodes[node.Name]
	if !ok {
		return nil, fmt.Errorf("node %q is not running", node.Name)
	}
	return vmtest.DialAgent(ctx, running.VSock.GuestCID, running.VSock.Port, twoNodeBootstrapTranscriptPath(t.TranscriptDir, node.Name))
}

func plannedControlPlaneNodes(nodes []vmtest.RunningInstalledRuntimeNode) map[string]inventory.PlannedNode {
	planned := make(map[string]inventory.PlannedNode, len(nodes))
	for _, node := range nodes {
		planned[node.Name] = inventory.PlannedNode{
			Name:              node.Name,
			SystemRole:        inventory.RoleControlPlane,
			Action:            inventory.ActionControlPlaneJoin,
			Access:            inventory.Access{Method: "agent", CredentialRef: "vsock:" + uint32String(node.VSock.GuestCID) + ":" + uint32String(node.VSock.Port)},
			KubeadmConfig:     inventory.KubeadmConfig{Ref: "control-plane", Path: "/etc/katl/kubeadm/control-plane/config.yaml", Intent: inventory.IntentControlPlane},
			KubernetesVersion: firstString(os.Getenv("KATL_KUBERNETES_VERSION"), "v1.36.1"),
		}
	}
	if cp1, ok := planned["cp-1"]; ok {
		cp1.Action = inventory.ActionInit
		planned["cp-1"] = cp1
	}
	return planned
}

func nodeMap(nodes []vmtest.RunningInstalledRuntimeNode) map[string]vmtest.RunningInstalledRuntimeNode {
	out := make(map[string]vmtest.RunningInstalledRuntimeNode, len(nodes))
	for _, node := range nodes {
		out[node.Name] = node
	}
	return out
}

func nodeRunDirs(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	out := make(map[string]string, len(nodes))
	for _, node := range nodes {
		out[node.Name] = node.Result.RunDir
	}
	return out
}

func transcriptPaths(root string, nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	out := make(map[string]string, len(nodes))
	for _, node := range nodes {
		out[node.Name] = twoNodeBootstrapTranscriptPath(root, node.Name)
	}
	return out
}

func operationEvidencePaths(root string, nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	out := make(map[string]string, len(nodes))
	for _, node := range nodes {
		out[node.Name] = filepath.Join(root, node.Name)
	}
	return out
}

func TestThreeControlPlaneInventoryAndEtcdVerificationHelpers(t *testing.T) {
	nodes := []vmtest.RunningInstalledRuntimeNode{
		{Name: "cp-1", VSock: vmtest.VSockPlan{Enabled: true, GuestCID: 1, Port: 10240}},
		{Name: "cp-2", VSock: vmtest.VSockPlan{Enabled: true, GuestCID: 2, Port: 10240}},
		{Name: "cp-3", VSock: vmtest.VSockPlan{Enabled: true, GuestCID: 3, Port: 10240}},
	}
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := writeThreeControlPlaneInventory(path, "v1.36.1", nodes); err != nil {
		t.Fatalf("writeThreeControlPlaneInventory() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	for _, want := range []string{"name: cp-1", "name: cp-2", "name: cp-3", "credentialRef: vsock:3:10240", "intent: control-plane"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("inventory missing %q:\n%s", want, data)
		}
	}
	planned := plannedControlPlaneNodes(nodes)
	if got := []inventory.BootstrapAction{planned["cp-1"].Action, planned["cp-2"].Action, planned["cp-3"].Action}; !reflect.DeepEqual(got, []inventory.BootstrapAction{inventory.ActionInit, inventory.ActionControlPlaneJoin, inventory.ActionControlPlaneJoin}) {
		t.Fatalf("planned actions = %#v", got)
	}
	config := threeControlPlaneNodeConfig("cp-2", "disk.qcow2", "esp", "fixture.json", "node.json", vmtest.DiskQCOW2, vmtest.KVMOff, 43202)
	if config.Runtime.FixtureManifest != "fixture.json" || config.Runtime.NodeMetadata != "node.json" {
		t.Fatalf("runtime provenance = fixture %q metadata %q", config.Runtime.FixtureManifest, config.Runtime.NodeMetadata)
	}
}

func TestControlPlaneProfilingConfigOnlyAddsSupportedFields(t *testing.T) {
	live := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\ncertificateValidityPeriod: 8760h0m0s\napiServer:\n  extraArgs:\n    - name: authorization-mode\n      value: Node,RBAC\ncontrollerManager:\n  extraArgs:\n    - name: bind-address\n      value: 0.0.0.0\n")
	desired, deltas, err := controlPlaneProfilingConfig(live)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ClusterConfiguration.apiServer.extraArgs.profiling=false",
		"ClusterConfiguration.controllerManager.extraArgs.profiling=false",
		"ClusterConfiguration.scheduler.extraArgs.profiling=false",
	}
	if !reflect.DeepEqual(deltas, want) {
		t.Fatalf("deltas = %v, want %v\ndesired:\n%s", deltas, want, desired)
	}
}

func TestThreeControlPlaneSmokeArtifactManifestUsesPlannedNodeArtifacts(t *testing.T) {
	result, err := vmtest.NewRunner(vmtest.Options{
		StateRoot: t.TempDir(),
		RunID:     "run-1",
	}).Plan(vmtest.Scenario{Name: "three-cp"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	nodes := make([]vmtest.RunningInstalledRuntimeNode, 0, 3)
	for _, name := range []string{"cp-1", "cp-2", "cp-3"} {
		nodeResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, name)
		if err != nil {
			t.Fatalf("plan %s: %v", name, err)
		}
		nodeResult.DomainName = "katl-" + name
		nodeResult.MACAddress = map[string]string{
			"cp-1": "52:54:00:00:10:01",
			"cp-2": "52:54:00:00:10:02",
			"cp-3": "52:54:00:00:10:03",
		}[name]
		nodeResult.IPAddress = map[string]string{
			"cp-1": "192.0.2.21",
			"cp-2": "192.0.2.22",
			"cp-3": "192.0.2.23",
		}[name]
		nodes = append(nodes, vmtest.RunningInstalledRuntimeNode{Name: name, Result: nodeResult})
	}
	bundleManifestDigest := "sha256:" + strings.Repeat("a", 64)
	sysextPayloadDigest := "sha256:" + strings.Repeat("b", 64)
	kubernetesBundle := threeControlPlaneKubernetesPayloadBundle{
		Source:               "https://192.0.2.1:9443",
		Ref:                  "v1.36.1@" + bundleManifestDigest,
		PayloadVersion:       "v1.36.1",
		BundleManifestDigest: bundleManifestDigest,
		SysextPayloadDigest:  sysextPayloadDigest,
		Root:                 filepath.Join(result.ManifestDir, "kubernetes-payload-bundles"),
		IndexPath:            filepath.Join(result.ManifestDir, "kubernetes-payload-bundles", "index.json"),
		CatalogPath:          filepath.Join(result.ManifestDir, "kubernetes-payload-bundles", "kubernetes-sysext-bundles-v1.36.json"),
		LogPath:              filepath.Join(result.RunDir, "kubernetes-payload-bundle.log"),
		BundlePaths: map[string]string{
			"v1.36.0": filepath.Join(result.ManifestDir, "kubernetes-payload-bundles", "katl-kubernetes-v1.36.0.bundle.json"),
			"v1.36.1": filepath.Join(result.ManifestDir, "kubernetes-payload-bundles", "katl-kubernetes-v1.36.1.bundle.json"),
		},
		CACertPath:      filepath.Join(result.ManifestDir, "kubernetes-bundle-ca.pem"),
		CACertGuestPath: "/var/lib/katl/test-artifacts/kubernetes-bundle-ca.pem",
		CACertPEM:       []byte("test-ca"),
	}
	imageFixtures := map[string][]nodeImageFixture{
		"cp-1": {{
			Image:     "localhost/katl-vmtest/gateway-proxy:latest",
			Source:    "/tmp/gateway-proxy.tar",
			GuestPath: "/var/lib/katl/test-artifacts/bootstrap-images/gateway-proxy.tar",
		}},
	}
	workloadStack := &releaseWorkloadStackEvidence{
		Manifest:            "/tmp/run/manifests/release-workload-stack.yaml",
		CRDManifest:         "/tmp/run/manifests/release-workload-stack-crds.yaml",
		ApplyOutput:         "/tmp/run/release-workload-stack/kubectl-apply.txt",
		CRDApplyOutput:      "/tmp/run/release-workload-stack/kubectl-apply-crds.txt",
		CiliumPods:          "/tmp/run/release-workload-stack/kubectl-get-cilium-pods.txt",
		CoreDNSPods:         "/tmp/run/release-workload-stack/kubectl-get-coredns-pods.txt",
		EnvoyGatewayPods:    "/tmp/run/release-workload-stack/kubectl-get-envoy-gateway-pods.txt",
		EchoPods:            "/tmp/run/release-workload-stack/kubectl-get-echo-pods.txt",
		GatewayClasses:      "/tmp/run/release-workload-stack/kubectl-get-gateway-classes.txt",
		GatewayAPIResources: "/tmp/run/release-workload-stack/kubectl-get-gateway-api.txt",
		GatewayURL:          "http://192.0.2.21:31080/hostname",
		GatewayResponse:     "/tmp/run/release-workload-stack/gateway-response.txt",
	}
	if err := writeThreeControlPlaneSmokeArtifactManifest(result, threeControlPlaneSmokeInputs{
		CP1Disk:     "cp1.raw",
		CP1ESP:      "esp",
		CP1Fixture:  "cp1-fixture.json",
		CP1Metadata: "cp1-node.json",
		CP2Disk:     "cp2.raw",
		CP2ESP:      "esp",
		CP2Fixture:  "cp2-fixture.json",
		CP2Metadata: "cp2-node.json",
		CP3Disk:     "cp3.raw",
		CP3ESP:      "esp",
		CP3Fixture:  "cp3-fixture.json",
		CP3Metadata: "cp3-node.json",
		WorldProvenance: multiNodeWorldProvenancePaths{
			WorldManifest:            "/tmp/world.json",
			HostCapabilities:         "/tmp/host-capabilities.json",
			MkosiArtifactIndex:       "/tmp/mkosi-artifacts.json",
			NetworkLeaseFile:         "/tmp/network-leases.json",
			FixtureProducerScenarios: map[string]string{"cp-2": "/tmp/fixture-cp-2/scenario.json"},
			FixtureProducerResults:   map[string]string{"cp-3": "/tmp/fixture-cp-3/result.json"},
		},
	}, filepath.Join(result.RunDir, "agent-transcripts"), filepath.Join(result.RunDir, "etcd-transcripts"), nodes, bootstrapFixtureInputs{}, kubernetesBundle, nil, imageFixtures, workloadStack); err != nil {
		t.Fatalf("writeThreeControlPlaneSmokeArtifactManifest() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(result.ManifestDir, "three-control-plane-artifacts.json"))
	if err != nil {
		t.Fatalf("read artifact manifest: %v", err)
	}
	var manifest threeControlPlaneArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode artifact manifest: %v", err)
	}
	if manifest.NodeRunDirs["cp-2"] != nodes[1].Result.RunDir || manifest.SerialLogs["cp-3"] != nodes[2].Result.Artifacts.RuntimeSerial {
		t.Fatalf("planned run dirs/serials = %#v %#v", manifest.NodeRunDirs, manifest.SerialLogs)
	}
	if manifest.NodeScenarios["cp-1"] != nodes[0].Result.Artifacts.Scenario || manifest.NodeScenarios["cp-3"] != nodes[2].Result.Artifacts.Scenario {
		t.Fatalf("planned node scenarios = %#v", manifest.NodeScenarios)
	}
	if manifest.LaunchCommands["cp-1"] != nodes[0].Result.Artifacts.LaunchCommand || manifest.DomainXMLs["cp-2"] != nodes[1].Result.Artifacts.DomainXML || manifest.InstalledRuntimeInputs["cp-3"] != nodes[2].Result.Artifacts.InstalledRuntime {
		t.Fatalf("planned artifact indexes = launch %#v domain %#v installed %#v", manifest.LaunchCommands, manifest.DomainXMLs, manifest.InstalledRuntimeInputs)
	}
	if manifest.LibvirtLeases["cp-1"] != nodes[0].Result.Artifacts.LibvirtLease || manifest.LibvirtLeases["cp-3"] != nodes[2].Result.Artifacts.LibvirtLease {
		t.Fatalf("planned libvirt lease artifacts = %#v", manifest.LibvirtLeases)
	}
	if manifest.NodeDomains["cp-2"] != "katl-cp-2" || manifest.NodeMACs["cp-3"] != "52:54:00:00:10:03" || manifest.NodeIPs["cp-1"] != "192.0.2.21" {
		t.Fatalf("planned node identity = domains %#v macs %#v ips %#v", manifest.NodeDomains, manifest.NodeMACs, manifest.NodeIPs)
	}
	if manifest.EtcdTranscripts["cp-2"] != twoNodeBootstrapTranscriptPath(filepath.Join(result.RunDir, "etcd-transcripts"), "cp-2") {
		t.Fatalf("etcd transcripts = %#v", manifest.EtcdTranscripts)
	}
	if manifest.OperationEvidence["cp-2"] != filepath.Join(result.RunDir, "operation-evidence", "cp-2") {
		t.Fatalf("operation evidence = %#v", manifest.OperationEvidence)
	}
	if manifest.WorldManifest != "/tmp/world.json" || manifest.NetworkLeases != "/tmp/network-leases.json" || manifest.FixtureProducerResults["cp-3"] != "/tmp/fixture-cp-3/result.json" {
		t.Fatalf("planned provenance = %#v", manifest)
	}
	if manifest.KubernetesPayloadBundle == nil ||
		manifest.KubernetesPayloadBundle.Source != kubernetesBundle.Source ||
		manifest.KubernetesPayloadBundle.Ref != kubernetesBundle.Ref ||
		manifest.KubernetesPayloadBundle.BundleManifestDigest != bundleManifestDigest ||
		manifest.KubernetesPayloadBundle.SysextPayloadDigest != sysextPayloadDigest ||
		manifest.KubernetesPayloadBundle.LogPath != kubernetesBundle.LogPath ||
		manifest.KubernetesPayloadBundle.CACertGuestPath != kubernetesBundle.CACertGuestPath {
		t.Fatalf("Kubernetes payload bundle evidence = %#v, want %#v", manifest.KubernetesPayloadBundle, kubernetesBundle)
	}
	if len(manifest.KubernetesPayloadBundle.CACertPEM) != 0 {
		t.Fatalf("Kubernetes payload bundle manifest leaked CA PEM bytes")
	}
	if manifest.ImageFixtures["cp-1"][0].Image != "localhost/katl-vmtest/gateway-proxy:latest" {
		t.Fatalf("image fixtures = %#v", manifest.ImageFixtures)
	}
	if manifest.ReleaseWorkloadStack == nil ||
		manifest.ReleaseWorkloadStack.Manifest != workloadStack.Manifest ||
		manifest.ReleaseWorkloadStack.GatewayClasses != workloadStack.GatewayClasses ||
		manifest.ReleaseWorkloadStack.GatewayURL != workloadStack.GatewayURL ||
		manifest.ReleaseWorkloadStack.GatewayResponse != workloadStack.GatewayResponse {
		t.Fatalf("release workload stack evidence = %#v, want %#v", manifest.ReleaseWorkloadStack, workloadStack)
	}
}

func TestThreeControlPlaneOperationBackedInventoryCarriesKubernetesBundle(t *testing.T) {
	nodes := []vmtest.RunningInstalledRuntimeNode{
		{Name: "cp-1"},
		{Name: "cp-2"},
		{Name: "cp-3"},
	}
	addresses := map[string]string{
		"cp-1": "192.0.2.21",
		"cp-2": "192.0.2.22",
		"cp-3": "192.0.2.23",
	}
	bundle := threeControlPlaneKubernetesPayloadBundle{
		Source: "https://192.0.2.1:9443",
		Ref:    "192.0.2.1:9443/katl-vmtest/kubernetes:v1.36.1-katl.0@sha256:" + strings.Repeat("a", 64),
	}
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := writeThreeControlPlaneOperationBackedInventory(path, "v1.36.1", bundle, nodes, addresses); err != nil {
		t.Fatalf("writeThreeControlPlaneOperationBackedInventory() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	for _, want := range []string{
		`kubernetesBundle: "192.0.2.1:9443/katl-vmtest/kubernetes:v1.36.1-katl.0@sha256:` + strings.Repeat("a", 64) + `"`,
		"name: cp-1",
		"address: 192.0.2.23",
		"intent: control-plane",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("inventory missing %q:\n%s", want, data)
		}
	}
}

func TestResolvedBundleDigestMatching(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, test := range []struct {
		name     string
		actual   string
		expected string
		want     bool
	}{
		{name: "published bundle resolves digest", actual: "sha256:" + digest, want: true},
		{name: "locally staged bundle matches digest", actual: digest, expected: "sha256:" + digest, want: true},
		{name: "missing resolved digest", expected: "sha256:" + digest, want: false},
		{name: "different resolved digest", actual: strings.Repeat("b", 64), expected: digest, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := resolvedBundleDigestMatches(test.actual, test.expected); got != test.want {
				t.Fatalf("resolvedBundleDigestMatches(%q, %q) = %t, want %t", test.actual, test.expected, got, test.want)
			}
		})
	}
}

func TestDecodeGenerationApplyResultUsesTerminalStatus(t *testing.T) {
	status := &agentapi.OperationStatus{
		OperationKind:         "generation-apply",
		Phase:                 operation.HostBookkeepingCompletionPhase,
		Terminal:              true,
		Result:                operation.ResultSucceeded,
		CandidateGenerationId: "generation-1",
		ActivationState:       operation.ActivationStateActiveLive,
	}
	data, err := protojson.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeGenerationApplyResult(data, "generation-1"); err != nil {
		t.Fatalf("decodeGenerationApplyResult() error = %v", err)
	}
}

func TestWriteKubernetesBundleSourceLogRecordsServedRefAndDigests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubernetes-payload-bundle.log")
	bundle := threeControlPlaneKubernetesPayloadBundle{
		Source:               "https://192.0.2.1:9443",
		Ref:                  "v1.36.1@sha256:" + strings.Repeat("a", 64),
		PayloadVersion:       "v1.36.1",
		BundleManifestDigest: "sha256:" + strings.Repeat("a", 64),
		SysextPayloadDigest:  "sha256:" + strings.Repeat("b", 64),
		Root:                 "/tmp/run/manifests/kubernetes-payload-bundles",
		IndexPath:            "/tmp/run/manifests/kubernetes-payload-bundles/index.json",
		CatalogPath:          "/tmp/run/manifests/kubernetes-payload-bundles/kubernetes-sysext-bundles-v1.36.json",
	}
	if err := writeKubernetesBundleSourceLog(path, bundle); err != nil {
		t.Fatalf("writeKubernetesBundleSourceLog() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, want := range []string{
		"source=" + bundle.Source,
		"ref=" + bundle.Ref,
		"bundleManifestDigest=" + bundle.BundleManifestDigest,
		"sysextPayloadDigest=" + bundle.SysextPayloadDigest,
		"indexPath=" + bundle.IndexPath,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("source log missing %q:\n%s", want, data)
		}
	}
}

func TestWriteThreeControlPlaneEtcdReportPreservesFailureEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etcd-report.json")
	if err := writeThreeControlPlaneEtcdReport(path, threeControlPlaneEtcdReport{
		FailureSummary: "etcd member cp-3 missing",
		StaticPods: []controlPlaneStaticPodReport{{
			Node:      "cp-1",
			Container: map[string]string{"etcd": "container-1"},
		}},
		Health: cluster.EtcdReport{
			Node:    "cp-1",
			Healthy: true,
			Members: []cluster.EtcdMember{
				{Name: "cp-1"},
				{Name: "cp-2"},
			},
			Quorum: 2,
		},
		Transcript: "/tmp/run/etcd-transcripts/cp-1.jsonl",
	}); err != nil {
		t.Fatalf("writeThreeControlPlaneEtcdReport() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read etcd report: %v", err)
	}
	var report threeControlPlaneEtcdReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode etcd report: %v", err)
	}
	if report.FailureSummary != "etcd member cp-3 missing" || report.Transcript != "/tmp/run/etcd-transcripts/cp-1.jsonl" {
		t.Fatalf("failure evidence = %#v", report)
	}
	if len(report.StaticPods) != 1 || report.StaticPods[0].Container["etcd"] != "container-1" {
		t.Fatalf("static pod evidence = %#v", report.StaticPods)
	}
	if report.Health.Quorum != 2 || report.Health.HasMember("cp-3") {
		t.Fatalf("health evidence = %#v", report.Health)
	}
}

func TestPlanThreeControlPlaneWorldSmokeRunWritesSetupFailureForMissingPublishedFixture(t *testing.T) {
	world := twoNodeTestWorld(t)
	run, err := planThreeControlPlaneWorldSmokeRun(world, t.TempDir(), "installed-runtime-three-control-plane-v01-workload-proof", "v1.36.1", vmtest.KVMOff, true)
	if err == nil || !strings.Contains(err.Error(), "published installed runtime fixture is missing") {
		t.Fatalf("planThreeControlPlaneWorldSmokeRun() error = %v, want missing published fixture", err)
	}
	if run.WorldScenario == nil {
		t.Fatal("planThreeControlPlaneWorldSmokeRun() did not return world scenario on setup failure")
	}
	data, err := os.ReadFile(run.WorldScenario.ResultPath)
	if err != nil {
		t.Fatalf("read world setup result: %v", err)
	}
	var result struct {
		Status         vmtest.WorldStatus `json:"status"`
		FailureSummary string             `json:"failureSummary"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode world setup result: %v", err)
	}
	if result.Status != vmtest.WorldStatusSetupFailed || !strings.Contains(result.FailureSummary, "published installed runtime fixture is missing") {
		t.Fatalf("world setup result = %#v", result)
	}
}

func TestPlanThreeControlPlaneWorldSmokeRunPrefersWorldPublishedFixtures(t *testing.T) {
	world := twoNodeTestWorld(t)
	world.RunIndex = filepath.Join(world.RunDir, "custom-run.json")
	repo := t.TempDir()
	for _, name := range []string{"cp-1", "cp-2", "cp-3"} {
		writeKatlctlPublishedInstalledRuntimeFixture(t, vmtest.DefaultVMTestCacheDir(repo), "repo-"+name, name, vmtest.ControlPlane)
		writeKatlctlPublishedInstalledRuntimeFixture(t, world.CacheDir, "world-"+name, name, vmtest.ControlPlane)
	}

	run, err := planThreeControlPlaneWorldSmokeRun(world, repo, "installed-runtime-three-control-plane-v01-workload-proof", "v1.36.1", vmtest.KVMOff, true)
	if err != nil {
		t.Fatalf("planThreeControlPlaneWorldSmokeRun() error = %v", err)
	}
	if !run.WorkloadProof || run.Scenario.Name != "installed-runtime-three-control-plane-v01-workload-proof" {
		t.Fatalf("planned workload proof run = %#v", run)
	}
	assertFileContent(t, run.Inputs.CP1Disk, "disk-world-cp-1")
	assertFileContent(t, run.Inputs.CP2Disk, "disk-world-cp-2")
	assertFileContent(t, run.Inputs.CP3Disk, "disk-world-cp-3")
	if run.Inputs.WorldProvenance.VMTestRun != filepath.Join(world.RunDir, "custom-run.json") || run.Inputs.WorldProvenance.WorldManifest != filepath.Join(world.RunDir, "world.json") || run.Inputs.WorldProvenance.HostCapabilities != filepath.Join(world.RunDir, "host-capabilities.json") {
		t.Fatalf("world provenance = %#v", run.Inputs.WorldProvenance)
	}
	if run.Inputs.WorldProvenance.FixtureProducerResults["cp-3"] != filepath.Join(world.ScenarioDir, "first-install-installed-runtime-fixture-cp-3-control-plane", "result.json") {
		t.Fatalf("fixture producer results = %#v", run.Inputs.WorldProvenance.FixtureProducerResults)
	}
}

func TestVerifyThreeControlPlaneBootstrapTranscriptsChecksKubeadmRoles(t *testing.T) {
	dir := t.TempDir()
	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/var/lib/katl/test-artifacts/kubeadm-init-cp-1.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "phase", "upload-certs", "--upload-certs"}, Redaction: "output", SensitiveOutput: true},
	})
	for _, node := range []string{"cp-2", "cp-3"} {
		writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, node), []transcriptEntry{
			{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
			{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
			{Method: "WriteFile", Redaction: "sensitive", SensitiveOutput: true, WriteBytes: 256},
			{Method: "RunCommand", Argv: []string{"kubeadm", "join", "--config", "/var/lib/katl/test-artifacts/kubeadm-join-" + node + ".yaml"}, Redaction: "output", SensitiveOutput: true},
		})
	}
	if err := verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"}); err != nil {
		t.Fatalf("verifyBootstrapTranscripts() error = %v", err)
	}

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/var/lib/katl/test-artifacts/kubeadm-init-cp-1.yaml"}, Redaction: "output", SensitiveOutput: true},
	})
	err := verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"})
	if err == nil || !strings.Contains(err.Error(), "missing kubeadm certificate upload command") {
		t.Fatalf("verifyBootstrapTranscripts() error = %v, want certificate upload rejection", err)
	}
	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/var/lib/katl/test-artifacts/kubeadm-init-cp-1.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "phase", "upload-certs", "--upload-certs"}, Redaction: "output", SensitiveOutput: true},
	})

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-2"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "WriteFile", Redaction: "sensitive", SensitiveOutput: true, WriteBytes: 256},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "--config", "/var/lib/katl/test-artifacts/kubeadm-join-cp-2.yaml", "--certificate-key", "[REDACTED CERTIFICATE KEY]"}, Redaction: "output", SensitiveOutput: true},
	})
	err = verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"})
	if err == nil || !strings.Contains(err.Error(), "kubeadm control-plane join command must not include --certificate-key") {
		t.Fatalf("verifyBootstrapTranscripts() error = %v, want certificate-key leak rejection", err)
	}
	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-2"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "WriteFile", Redaction: "sensitive", SensitiveOutput: true, WriteBytes: 256},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "--config", "/var/lib/katl/test-artifacts/kubeadm-join-cp-2.yaml"}, Redaction: "output", SensitiveOutput: true},
	})

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-2"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "WriteFile", Redaction: "sensitive", SensitiveOutput: true, WriteBytes: 256},
	})
	err = verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"})
	if err == nil || !strings.Contains(err.Error(), "unexpected kubeadm init command on joining control-plane") {
		t.Fatalf("verifyBootstrapTranscripts() error = %v, want cp-2 init rejection", err)
	}

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-2"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "WriteFile", Redaction: "sensitive", SensitiveOutput: true, WriteBytes: 256},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "--config", "/var/lib/katl/test-artifacts/kubeadm-join-worker-1.yaml"}, Redaction: "output", SensitiveOutput: true},
	})
	err = verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"})
	if err == nil || !strings.Contains(err.Error(), "kubeadm control-plane join command missing generated control-plane config path") {
		t.Fatalf("verifyBootstrapTranscripts() error = %v, want cp-2 generated config path rejection", err)
	}
}

func TestThreeControlPlaneArtifactManifestRecordsWorldInputs(t *testing.T) {
	inputs := threeControlPlaneFixtureInputs("cp1.raw", string(vmtest.DiskRaw), "cp2.qcow2", "qcow2", "cp3.raw", string(vmtest.DiskRaw), "cp1-esp", "cp2-esp", "cp3-esp", "cp1-fixture.json", "cp2-fixture.json", "cp3-fixture.json", "cp1-node.json", "cp2-node.json", "cp3-node.json")
	if inputs["cp-2"].DiskFormat != "qcow2" || inputs["cp-3"].DiskFormat != string(vmtest.DiskRaw) {
		t.Fatalf("fixture input formats = %#v", inputs)
	}
	path := filepath.Join(t.TempDir(), "three-control-plane-artifacts.json")
	if err := writeThreeControlPlaneArtifactManifest(path, threeControlPlaneArtifactManifest{
		VMTestRun:          "/tmp/run.json",
		WorldManifest:      "/tmp/world.json",
		HostCapabilities:   "/tmp/host-capabilities.json",
		MkosiArtifactIndex: "/tmp/mkosi-artifacts.json",
		NodeRunDirs: map[string]string{
			"cp-1": "/tmp/cp-1-run",
		},
		NodeResults: map[string]string{
			"cp-1": "/tmp/cp-1-run/result.json",
		},
		NodeScenarios: map[string]string{
			"cp-1": "/tmp/cp-1-run/scenario.json",
		},
		LaunchCommands: map[string]string{
			"cp-1": "/tmp/cp-1-run/vm/launch-command.txt",
		},
		DomainXMLs: map[string]string{
			"cp-1": "/tmp/cp-1-run/vm/domain.xml",
		},
		InstalledRuntimeInputs: map[string]string{
			"cp-1": "/tmp/cp-1-run/manifests/installed-runtime.json",
		},
		VSockTranscripts: map[string]string{
			"cp-1": "/tmp/cp-1-run/vm/vsock-transcript.jsonl",
		},
		FixtureInputs:            inputs,
		FixtureProducerScenarios: map[string]string{"cp-1": "/tmp/fixture-cp-1/scenario.json", "cp-2": "/tmp/fixture-cp-2/scenario.json", "cp-3": "/tmp/fixture-cp-3/scenario.json"},
		FixtureProducerResults:   map[string]string{"cp-1": "/tmp/fixture-cp-1/result.json", "cp-2": "/tmp/fixture-cp-2/result.json", "cp-3": "/tmp/fixture-cp-3/result.json"},
		KubeconfigMetadata:       "/tmp/run/operator-kubeconfig-metadata.json",
		BootstrapFixture:         (&bootstrapFixtureInputs{Manifests: []string{"/tmp/ha-cni.yaml"}, Waits: []string{"nodes-ready"}}).manifestValue(),
		SerialLogs:               map[string]string{"cp-1": "/tmp/cp-1-run/vm/runtime-serial.log"},
		OperationEvidence:        map[string]string{"cp-2": "/tmp/run/operation-evidence/cp-2"},
		Diagnostics:              map[string]string{"cp-1": "/tmp/cp-1-guest/diagnostics-summary.json", "cp-2": "/tmp/cp-2-guest/diagnostics-summary.json", "cp-3": "/tmp/cp-3-guest/diagnostics-summary.json"},
		KubectlDiagnostics:       map[string]string{"kubeSystemPods": "/tmp/run/kubectl-get-pods-kube-system.txt"},
	}); err != nil {
		t.Fatalf("writeThreeControlPlaneArtifactManifest() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact manifest: %v", err)
	}
	var manifest threeControlPlaneArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode artifact manifest: %v", err)
	}
	if manifest.FixtureInputs["cp-1"].FixtureManifest != "cp1-fixture.json" || manifest.FixtureInputs["cp-3"].NodeMetadata != "cp3-node.json" {
		t.Fatalf("artifact manifest fixture inputs = %#v", manifest.FixtureInputs)
	}
	if manifest.VMTestRun != "/tmp/run.json" || manifest.WorldManifest != "/tmp/world.json" || manifest.HostCapabilities != "/tmp/host-capabilities.json" || manifest.MkosiArtifactIndex != "/tmp/mkosi-artifacts.json" {
		t.Fatalf("artifact manifest world provenance = %q %q %q %q", manifest.VMTestRun, manifest.WorldManifest, manifest.HostCapabilities, manifest.MkosiArtifactIndex)
	}
	if manifest.FixtureProducerScenarios["cp-2"] != "/tmp/fixture-cp-2/scenario.json" || manifest.FixtureProducerResults["cp-3"] != "/tmp/fixture-cp-3/result.json" {
		t.Fatalf("artifact manifest fixture provenance = %#v %#v", manifest.FixtureProducerScenarios, manifest.FixtureProducerResults)
	}
	if manifest.Diagnostics["cp-1"] != "/tmp/cp-1-guest/diagnostics-summary.json" || manifest.Diagnostics["cp-3"] != "/tmp/cp-3-guest/diagnostics-summary.json" {
		t.Fatalf("artifact manifest diagnostics = %#v", manifest.Diagnostics)
	}
	if manifest.SerialLogs["cp-1"] != "/tmp/cp-1-run/vm/runtime-serial.log" {
		t.Fatalf("artifact manifest serial logs = %#v", manifest.SerialLogs)
	}
	if manifest.OperationEvidence["cp-2"] != "/tmp/run/operation-evidence/cp-2" {
		t.Fatalf("artifact manifest operation evidence = %#v", manifest.OperationEvidence)
	}
	if manifest.NodeResults["cp-1"] != "/tmp/cp-1-run/result.json" || manifest.LaunchCommands["cp-1"] != "/tmp/cp-1-run/vm/launch-command.txt" {
		t.Fatalf("artifact manifest node artifacts = %#v %#v", manifest.NodeResults, manifest.LaunchCommands)
	}
	if manifest.DomainXMLs["cp-1"] != "/tmp/cp-1-run/vm/domain.xml" {
		t.Fatalf("artifact manifest domain XMLs = %#v", manifest.DomainXMLs)
	}
	if manifest.NodeScenarios["cp-1"] != "/tmp/cp-1-run/scenario.json" {
		t.Fatalf("artifact manifest node scenarios = %#v", manifest.NodeScenarios)
	}
	if manifest.InstalledRuntimeInputs["cp-1"] != "/tmp/cp-1-run/manifests/installed-runtime.json" || manifest.VSockTranscripts["cp-1"] != "/tmp/cp-1-run/vm/vsock-transcript.jsonl" {
		t.Fatalf("artifact manifest runtime artifacts = %#v %#v", manifest.InstalledRuntimeInputs, manifest.VSockTranscripts)
	}
	if manifest.KubeconfigMetadata != "/tmp/run/operator-kubeconfig-metadata.json" {
		t.Fatalf("artifact manifest kubeconfig metadata = %q", manifest.KubeconfigMetadata)
	}
	if manifest.BootstrapFixture == nil || !stringSlicesEqual(manifest.BootstrapFixture.Manifests, []string{"/tmp/ha-cni.yaml"}) || !stringSlicesEqual(manifest.BootstrapFixture.Waits, []string{"nodes-ready"}) {
		t.Fatalf("artifact manifest bootstrap fixture = %#v", manifest.BootstrapFixture)
	}
	if manifest.KubectlDiagnostics["kubeSystemPods"] != "/tmp/run/kubectl-get-pods-kube-system.txt" {
		t.Fatalf("artifact manifest kubectl diagnostics = %#v", manifest.KubectlDiagnostics)
	}
}

func TestThreeControlPlaneCNISpecs(t *testing.T) {
	specs := threeControlPlaneCNISpecs(map[string]string{
		"cp-1": "192.168.122.10",
		"cp-2": "192.168.122.20",
		"cp-3": "192.168.122.30",
	})
	if specs["cp-1"].PodSubnet != "10.244.0.0/24" || specs["cp-1"].PodGateway != "10.244.0.1" {
		t.Fatalf("cp-1 CNI spec = %#v", specs["cp-1"])
	}
	wantRoutes := []cniPeerRoute{
		{Subnet: "10.244.1.0/24", Address: "192.168.122.20"},
		{Subnet: "10.244.2.0/24", Address: "192.168.122.30"},
	}
	if !reflect.DeepEqual(specs["cp-1"].PeerRoutes, wantRoutes) {
		t.Fatalf("cp-1 peer routes = %#v, want %#v", specs["cp-1"].PeerRoutes, wantRoutes)
	}
	if specs["cp-2"].PodSubnet != "10.244.1.0/24" || specs["cp-3"].PodSubnet != "10.244.2.0/24" {
		t.Fatalf("control-plane CNI pod subnets = %#v", specs)
	}
}

func TestReleaseWorkloadStackFixtureContainsExpectedProofObjects(t *testing.T) {
	stackPath := filepath.Join(katlRepoRoot(t), "internal", "vmtest", "scenarios", "testdata", "bootstrap", "release-workload-stack.yaml")
	stackData, err := os.ReadFile(stackPath)
	if err != nil {
		t.Fatalf("read release workload stack fixture: %v", err)
	}
	crdPath := filepath.Join(katlRepoRoot(t), "internal", "vmtest", "scenarios", "testdata", "bootstrap", "release-workload-stack-crds.yaml")
	crdData, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read release workload stack CRD fixture: %v", err)
	}
	text := string(stackData) + "\n" + string(crdData)
	for _, want := range []string{
		"kind: DaemonSet",
		"name: cilium",
		"name: cilium-operator",
		"name: envoy-gateway",
		"api-approved.kubernetes.io",
		"kind: GatewayClass",
		"kind: Gateway",
		"kind: HTTPRoute",
		"name: echo",
		"nodePort: 31080",
		"localhost/katl-vmtest/gateway-proxy:latest",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("release workload stack fixture missing %q:\n%s", want, text)
		}
	}
}
