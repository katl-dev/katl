package scenarios

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/vmtest"
	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

func TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke(t *testing.T) {
	if run, ok := operationBackedWorldSmokeRun(t); ok {
		runOperationBackedBootstrapSmoke(t, run, false)
		return
	}

	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run two-node operation-backed bootstrap smoke")
	}
	_ = vmtest.RequireWorld(t)
}

func TestKubeadmUpgradeOperationSmoke(t *testing.T) {
	if run, ok := operationBackedWorldSmokeRun(t); ok {
		runOperationBackedBootstrapSmoke(t, run, true)
		return
	}
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("run through scripts/vmtest-run to prove Kubernetes upgrade operations")
	}
	_ = vmtest.RequireWorld(t)
}

type operationBackedSmokeRun struct {
	WorldScenario *vmtest.WorldScenario
	Options       vmtest.Options
	Runner        vmtest.Runner
	Scenario      vmtest.Scenario
	Result        vmtest.Result
	Inputs        operationBackedSmokeInputs
	LibvirtURI    string
	Network       string
}

type operationBackedSmokeInputs struct {
	ControlPlaneDisk       string
	ControlPlaneDiskFormat string
	ControlPlaneESP        string
	ControlPlaneFixture    string
	ControlPlaneMetadata   string
	ControlPlaneAddress    string
	ControlPlaneMAC        string
	ControlPlaneInstall    firstInstallProvenance
	WorkerDisk             string
	WorkerDiskFormat       string
	WorkerESP              string
	WorkerFixture          string
	WorkerMetadata         string
	WorkerAddress          string
	WorkerMAC              string
	WorkerInstall          firstInstallProvenance
	KubernetesVersion      string
	WorldProvenance        multiNodeWorldProvenancePaths
}

type firstInstallProvenance struct {
	InstallerUKI         string
	InstallerKernel      string
	InstallerInitrd      string
	InstallerCommandLine []string
	RuntimeArtifact      string
	InstallManifest      string
	FirstInstallMode     string
}

func operationBackedWorldSmokeRun(t *testing.T) (operationBackedSmokeRun, bool) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(vmtest.WorldManifestEnv)) == "" {
		return operationBackedSmokeRun{}, false
	}
	world := vmtest.RequireWorld(t)
	world = operationBackedFreshFixtureWorld(world)
	repo := katlRepoRoot(t)
	kvm := vmtest.DefaultOptions().KVM
	specs := twoNodeWorldRuntimeSpecs()
	if err := ensurePublishedRuntimeFixturesForWorld(world, repo, specs, kvm); err != nil {
		failWorldFixtureSetup(t, world, "installed-runtime-two-node-operation-backed-bootstrap", err)
	}
	run, err := planOperationBackedWorldSmokeRun(world, repo, operationBackedKubernetesVersion(t, repo), kvm)
	if err != nil {
		failTwoNodeWorldSetup(t, run.WorldScenario, err)
	}
	missing := twoNodeHostToolPrereqs(exec.LookPath)
	requireSmokePrereqs(t, run.Runner, run.Scenario, run.Result, "two-node operation-backed bootstrap smoke prerequisites missing", missing)
	return run, true
}

func operationBackedFreshFixtureWorld(world vmtest.World) vmtest.World {
	world.CacheDir = filepath.Join(world.RunDir, "operation-backed-fixtures")
	return world
}

func operationBackedKubernetesVersion(t *testing.T, repo string) string {
	t.Helper()
	if value := strings.TrimSpace(os.Getenv("KATL_VMTEST_KUBERNETES_BUNDLE")); value != "" {
		image, err := kubernetesbundle.ParseImageReference(value)
		if err != nil {
			t.Fatalf("parse published Kubernetes bundle: %v", err)
		}
		return image.PayloadVersion
	}
	if version := firstString(os.Getenv("KATL_KUBERNETES_VERSION"), os.Getenv("KATL_KUBERNETES_PAYLOAD_VERSION")); version != "" {
		return version
	}
	for _, path := range []string{
		os.Getenv("KATL_KUBERNETES_SYSEXT_METADATA"),
		filepath.Join(repo, "_build/mkosi/katl-kubernetes.raw.json"),
	} {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var metadata struct {
			PayloadVersion string `json:"payloadVersion"`
		}
		if err := json.Unmarshal(data, &metadata); err != nil {
			t.Fatalf("decode Kubernetes sysext metadata %s: %v", path, err)
		}
		if strings.TrimSpace(metadata.PayloadVersion) != "" {
			return metadata.PayloadVersion
		}
	}
	return "v1.36.1"
}

func planOperationBackedWorldSmokeRun(world vmtest.World, repo, kubernetesVersion string, kvm vmtest.KVMPolicy) (operationBackedSmokeRun, error) {
	return planOperationBackedWorldSmokeRunNamed(world, repo, kubernetesVersion, kvm, "installed-runtime-two-node-operation-backed-bootstrap")
}

func planOperationBackedWorldSmokeRunNamed(world vmtest.World, repo, kubernetesVersion string, kvm vmtest.KVMPolicy, scenarioName string) (operationBackedSmokeRun, error) {
	scenario, err := world.PlanScenario(scenarioName)
	if err != nil {
		return operationBackedSmokeRun{}, err
	}
	run := operationBackedSmokeRun{WorldScenario: scenario}
	buildRoots := publishedRuntimeBuildRoots(world, repo)
	cpPublished, err := vmtest.FindPublishedFirstInstallRuntimeFixtureInBuildRoots(buildRoots, vmtest.NodeSpec{Name: "cp-1", Role: vmtest.ControlPlane})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	workerPublished, err := vmtest.FindPublishedFirstInstallRuntimeFixtureInBuildRoots(buildRoots, vmtest.NodeSpec{Name: "worker-1", Role: vmtest.Worker})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	cp, err := vmtest.AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, buildRoots, vmtest.NodeSpec{Name: "cp-1", Role: vmtest.ControlPlane})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	worker, err := vmtest.AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, buildRoots, vmtest.NodeSpec{Name: "worker-1", Role: vmtest.Worker})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
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
	return operationBackedSmokeRun{
		WorldScenario: scenario,
		Options:       options,
		Runner:        runner,
		Scenario:      vmtest.Scenario{Name: scenarioName},
		Result:        result,
		LibvirtURI:    world.Libvirt.URI,
		Network:       world.Libvirt.Network,
		Inputs: operationBackedSmokeInputs{
			ControlPlaneDisk:       cp.Config.Disk,
			ControlPlaneDiskFormat: string(cp.Config.DiskFormat),
			ControlPlaneESP:        cp.Config.ESPArtifacts,
			ControlPlaneFixture:    cp.Config.FixtureManifest,
			ControlPlaneMetadata:   cp.Config.NodeMetadata,
			ControlPlaneAddress:    cp.Node.Address,
			ControlPlaneMAC:        cp.Node.MACAddress,
			ControlPlaneInstall:    firstInstallProvenanceFromPublished(cpPublished),
			WorkerDisk:             worker.Config.Disk,
			WorkerDiskFormat:       string(worker.Config.DiskFormat),
			WorkerESP:              worker.Config.ESPArtifacts,
			WorkerFixture:          worker.Config.FixtureManifest,
			WorkerMetadata:         worker.Config.NodeMetadata,
			WorkerAddress:          worker.Node.Address,
			WorkerMAC:              worker.Node.MACAddress,
			WorkerInstall:          firstInstallProvenanceFromPublished(workerPublished),
			KubernetesVersion:      firstString(kubernetesVersion, "v1.36.1"),
			WorldProvenance:        multiNodeWorldProvenanceForSpecs(world, repo, twoNodeWorldRuntimeSpecs()),
		},
	}, nil
}

func firstInstallProvenanceFromPublished(published vmtest.PublishedFirstInstallRuntimeFixture) firstInstallProvenance {
	return firstInstallProvenance{
		InstallerUKI:         published.InstallerUKI,
		InstallerKernel:      published.InstallerKernel,
		InstallerInitrd:      published.InstallerInitrd,
		InstallerCommandLine: append([]string(nil), published.InstallerCommandLine...),
		RuntimeArtifact:      published.RuntimeArtifact,
		InstallManifest:      published.InstallManifest,
		FirstInstallMode:     published.FirstInstallMode,
	}
}

func runOperationBackedBootstrapSmoke(t *testing.T, smoke operationBackedSmokeRun, proveUpgrade bool) {
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
	inventoryPath := filepath.Join(result.ManifestDir, "bootstrap-inventory.yaml")
	cpTokenPath := filepath.Join(result.RunDir, "cp-1-katlc-agent.token")
	workerTokenPath := filepath.Join(result.RunDir, "worker-1-katlc-agent.token")
	kubeconfigPath := filepath.Join(result.RunDir, "operator-kubeconfig.yaml")
	kubeconfigMetadataPath := filepath.Join(result.RunDir, "operator-kubeconfig-metadata.json")
	stdoutPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stdout")
	stderrPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stderr")
	kubectlOut := filepath.Join(result.RunDir, "kubectl-get-nodes.txt")
	evidenceDir := filepath.Join(result.RunDir, "operation-evidence")
	artifactManifestPath := filepath.Join(result.ManifestDir, "operation-backed-bootstrap-artifacts.json")
	bootstrapFixture, err := stageBootstrapFixtureInputs(result.ManifestDir, bootstrapFixtureInputsForRun(katlRepoRoot(t)))
	if err != nil {
		t.Fatal(err)
	}
	kubernetesBundle, bundleServer, err := stageOperationBackedKubernetesPayloadBundle(katlRepoRoot(t), result, smoke.WorldScenario.World.Network.Gateway, inputs.KubernetesVersion)
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("stage Kubernetes payload bundle: %v", err)
	}
	defer bundleServer.Close()
	cpResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	workerResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	plannedNodes := []vmtest.RunningInstalledRuntimeNode{
		{Name: "cp-1", Result: cpResult},
		{Name: "worker-1", Result: workerResult},
	}
	if err := writeOperationBackedArtifactManifest(artifactManifestPath, result, inputs, plannedNodes, operationBackedArtifacts{
		Inventory:          inventoryPath,
		Kubeconfig:         kubeconfigPath,
		KubeconfigMetadata: kubeconfigMetadataPath,
		BootstrapStdout:    stdoutPath,
		BootstrapStderr:    stderrPath,
		KubectlOutput:      kubectlOut,
		BootstrapFixture:   bootstrapFixture,
		KubernetesBundle:   kubernetesBundle,
		EvidenceDir:        evidenceDir,
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cpNode, err := vmtest.StartInstalledRuntimeNode(ctx, result, vmtest.InstalledRuntimeNodeConfig{
		Name: "cp-1",
		Runtime: vmtest.InstalledRuntimeConfig{
			Disk:            inputs.ControlPlaneDisk,
			DiskFormat:      vmtest.DiskFormat(inputs.ControlPlaneDiskFormat),
			ESPArtifacts:    inputs.ControlPlaneESP,
			FixtureManifest: inputs.ControlPlaneFixture,
			NodeMetadata:    inputs.ControlPlaneMetadata,
			VM:              operationBackedVMConfigForRun(smoke, inputs.ControlPlaneMAC, 0),
		},
	}, vmtest.VMRunner{})
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start control-plane VM: %v", err)
	}
	defer stopNode(t, cpNode)

	workerNode, err := vmtest.StartInstalledRuntimeNode(ctx, result, vmtest.InstalledRuntimeNodeConfig{
		Name: "worker-1",
		Runtime: vmtest.InstalledRuntimeConfig{
			Disk:            inputs.WorkerDisk,
			DiskFormat:      vmtest.DiskFormat(inputs.WorkerDiskFormat),
			ESPArtifacts:    inputs.WorkerESP,
			FixtureManifest: inputs.WorkerFixture,
			NodeMetadata:    inputs.WorkerMetadata,
			VM:              operationBackedVMConfigForRun(smoke, inputs.WorkerMAC, 0),
		},
	}, vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics("", cpNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start worker VM: %v", err)
	}
	defer stopNode(t, workerNode)

	nodes := []vmtest.RunningInstalledRuntimeNode{cpNode, workerNode}
	for _, node := range nodes {
		if err := installKubernetesBundleCA(ctx, node, kubernetesBundle); err != nil {
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("install Kubernetes bundle CA on %s: %v", node.Name, err)
		}
	}
	cpAddress, err := liveNodeIPv4Address(ctx, cpNode, firstString(cpNode.Result.IPAddress, inputs.ControlPlaneAddress))
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("read control-plane IP address: %v", err)
	}
	workerAddress, err := liveNodeIPv4Address(ctx, workerNode, firstString(workerNode.Result.IPAddress, inputs.WorkerAddress))
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("read worker IP address: %v", err)
	}
	cniFixtures, err := stageTwoNodeCNIFixtures(ctx, katlRepoRoot(t), cpNode, workerNode, cpAddress, workerAddress)
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("stage test CNI fixtures: %v", err)
	}
	imageFixtures, err := stageTwoNodeImageFixtures(ctx, katlRepoRoot(t), result.RunDir, nodes...)
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("stage test workload images: %v", err)
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bootSelectionsBefore := map[string]string{}
	for _, node := range nodes {
		nodeEvidenceDir := filepath.Join(evidenceDir, node.Name)
		if err := os.MkdirAll(nodeEvidenceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		beforeSelection, err := readNodeFileWithRetry(ctx, node, "/var/lib/katl/boot/selection.json", 128<<10, 2*time.Minute)
		if err != nil {
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("read %s boot selection before bootstrap: %v", node.Name, err)
		}
		beforeSelectionPath := filepath.Join(nodeEvidenceDir, "boot-selection-before.json")
		if err := os.WriteFile(beforeSelectionPath, beforeSelection, 0o600); err != nil {
			t.Fatal(err)
		}
		assertGeneration0Selection(t, beforeSelection)
		bootSelectionsBefore[node.Name] = beforeSelectionPath
	}
	tokenFiles := map[string]string{"cp-1": cpTokenPath, "worker-1": workerTokenPath}
	for _, node := range nodes {
		token, err := readNodeFileWithRetry(ctx, node, "/var/lib/katl/agent/token", 4<<10, 2*time.Minute)
		if err != nil {
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("read %s katlc agent token: %v", node.Name, err)
		}
		if err := os.WriteFile(tokenFiles[node.Name], token, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeOperationBackedInventory(inventoryPath, inputs.KubernetesVersion, kubernetesBundle, cpAddress, workerAddress, tokenFiles); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []struct {
		name    string
		address string
	}{
		{name: "cp-1", address: cpAddress},
		{name: "worker-1", address: workerAddress},
	} {
		if err := waitForKatlcAgentTCP(ctx, endpoint.name, endpoint.address, 2*time.Minute); err != nil {
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("wait for %s katlc agent TCP endpoint: %v", endpoint.name, err)
		}
	}
	if err := writeOperationBackedArtifactManifest(artifactManifestPath, result, inputs, nodes, operationBackedArtifacts{
		Inventory:            inventoryPath,
		Kubeconfig:           kubeconfigPath,
		KubeconfigMetadata:   kubeconfigMetadataPath,
		BootstrapStdout:      stdoutPath,
		BootstrapStderr:      stderrPath,
		KubectlOutput:        kubectlOut,
		EvidenceDir:          evidenceDir,
		CNIFixtures:          cniFixtures,
		ImageFixtures:        imageFixtures,
		KubernetesBundle:     kubernetesBundle,
		BootSelectionsBefore: bootSelectionsBefore,
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err = runKatlctlCommand(t, ctx, katlRepoRoot(t), appendBootstrapFixtureArgs([]string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--init-node", "cp-1",
		"--control-plane-endpoint", cpAddress + ":6443",
		"--kubernetes-bundle", kubernetesBundle.Ref,
		"--node-address", "cp-1=" + cpAddress,
		"--node-address", "worker-1=" + workerAddress,
		"--kubeconfig-out", kubeconfigPath,
		"--overwrite-kubeconfig",
	}, bootstrapFixture), &stdout, &stderr)
	_ = os.WriteFile(stdoutPath, stdout.Bytes(), 0o644)
	_ = os.WriteFile(stderrPath, stderr.Bytes(), 0o644)
	_ = writeKubeconfigMetadata(kubeconfigPath, kubeconfigMetadataPath)
	err = bootstrapCommandError(err, stdout.String())
	if err != nil {
		_ = os.WriteFile(filepath.Join(result.RunDir, "katlctl-bootstrap-error.txt"), []byte(err.Error()+"\n"), 0o644)
		collectOperationBackedFailureEvidence(ctx, cpNode, filepath.Join(evidenceDir, "cp-1"), "bootstrap-init")
		collectOperationBackedFailureEvidence(ctx, workerNode, filepath.Join(evidenceDir, "worker-1"), "bootstrap-join-worker")
		nodeStatus := collectNodeLocalStatusFailureEvidence(ctx, evidenceDir, nodes...)
		collectKubectlDiagnosticsForFailure(ctx, cpNode, kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics("", nodes...)
		_ = writeOperationBackedArtifactManifest(artifactManifestPath, result, inputs, nodes, operationBackedArtifacts{
			Inventory:            inventoryPath,
			Kubeconfig:           kubeconfigPath,
			KubeconfigMetadata:   kubeconfigMetadataPath,
			BootstrapStdout:      stdoutPath,
			BootstrapStderr:      stderrPath,
			KubectlOutput:        kubectlOut,
			BootstrapFixture:     bootstrapFixture,
			EvidenceDir:          evidenceDir,
			CNIFixtures:          cniFixtures,
			ImageFixtures:        imageFixtures,
			KubernetesBundle:     kubernetesBundle,
			BootSelectionsBefore: bootSelectionsBefore,
			NodeStatus:           nodeStatus,
		})
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("katlctl cluster bootstrap failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	cpEvidenceDir := filepath.Join(evidenceDir, "cp-1")
	workerEvidenceDir := filepath.Join(evidenceDir, "worker-1")
	cpRecordPath, cpRecord, err := collectOperationEvidence(ctx, cpNode, cpEvidenceDir, "bootstrap-init")
	if err != nil {
		collectKubectlDiagnosticsForFailure(ctx, cpNode, kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("collect control-plane operation evidence: %v", err)
	}
	workerRecordPath, workerRecord, err := collectOperationEvidence(ctx, workerNode, workerEvidenceDir, "bootstrap-join-worker")
	if err != nil {
		collectKubectlDiagnosticsForFailure(ctx, cpNode, kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("collect worker operation evidence: %v", err)
	}
	assertOperationBackedInitRecord(t, cpRecord, cpAddress+":6443")
	assertOperationBackedWorkerRecord(t, workerRecord, cpAddress+":6443")
	assertOperationJournalOrder(t, cpEvidenceDir, "bootstrap-runtime-ready-complete", "kubeadm-init-complete", "post-kubeadm-health-start", "operation-complete")
	assertOperationJournalOrder(t, workerEvidenceDir, "bootstrap-runtime-ready-complete", "kubeadm-join-worker-complete", "post-kubeadm-health-start", "operation-complete")
	assertOperationBackedBootstrapPhases(t, stdout.String())
	cpGenerationPath, cpGenerationRecord, err := collectGenerationEvidence(ctx, cpNode, cpEvidenceDir, cpRecord.CandidateGenerationID)
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("collect control-plane generation evidence: %v", err)
	}
	assertCommittedGeneration(t, cpGenerationRecord, cpRecord.CandidateGenerationID)
	workerGenerationPath, workerGenerationRecord, err := collectGenerationEvidence(ctx, workerNode, workerEvidenceDir, workerRecord.CandidateGenerationID)
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("collect worker generation evidence: %v", err)
	}
	assertCommittedGeneration(t, workerGenerationRecord, workerRecord.CandidateGenerationID)
	cpSelectionPath, cpSelection, err := collectBootSelectionEvidence(ctx, cpNode, cpEvidenceDir)
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("collect control-plane boot selection evidence: %v", err)
	}
	assertPostBootstrapSelection(t, cpSelection, cpRecord.CandidateGenerationID)
	workerSelectionPath, workerSelection, err := collectBootSelectionEvidence(ctx, workerNode, workerEvidenceDir)
	if err != nil {
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("collect worker boot selection evidence: %v", err)
	}
	assertPostBootstrapSelection(t, workerSelection, workerRecord.CandidateGenerationID)
	nodeStatus := map[string]string{}
	for _, node := range nodes {
		path, err := collectNodeLocalStatusEvidence(ctx, node, filepath.Join(evidenceDir, node.Name))
		if err != nil {
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("collect %s node status evidence: %v", node.Name, err)
		}
		nodeStatus[node.Name] = path
	}
	evidenceArtifacts := operationBackedArtifacts{
		Inventory:            inventoryPath,
		Kubeconfig:           kubeconfigPath,
		KubeconfigMetadata:   kubeconfigMetadataPath,
		BootstrapStdout:      stdoutPath,
		BootstrapStderr:      stderrPath,
		KubectlOutput:        kubectlOut,
		BootstrapFixture:     bootstrapFixture,
		EvidenceDir:          evidenceDir,
		CNIFixtures:          cniFixtures,
		ImageFixtures:        imageFixtures,
		KubernetesBundle:     kubernetesBundle,
		BootSelectionsBefore: bootSelectionsBefore,
		BootSelectionsAfter:  map[string]string{"cp-1": cpSelectionPath, "worker-1": workerSelectionPath},
		OperationRecords:     map[string]string{"cp-1": cpRecordPath, "worker-1": workerRecordPath},
		OperationJournals: map[string]string{
			"cp-1":     filepath.Join(cpEvidenceDir, "operation-journal-files.txt"),
			"worker-1": filepath.Join(workerEvidenceDir, "operation-journal-files.txt"),
		},
		GenerationMetadata: map[string]string{"cp-1": cpGenerationPath, "worker-1": workerGenerationPath},
		NodeStatus:         nodeStatus,
	}
	if err := writeOperationBackedArtifactManifest(artifactManifestPath, result, inputs, nodes, evidenceArtifacts); err != nil {
		t.Fatal(err)
	}
	output, err := waitForKubectlNodes(ctx, kubeconfigPath, kubectlOut, 3*time.Minute, "node/cp-1", "node/worker-1")
	if err != nil {
		collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics("", nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("kubectl nodes did not converge: %v\n%s", err, output)
	}
	assertKubeconfigOutput(t, kubeconfigPath, kubeconfigMetadataPath, "https://"+cpAddress+":6443")
	if proveUpgrade {
		if err := runTwoNodeKubeadmUpgradeProof(t, ctx, smoke, cpNode, workerNode, cpAddress, workerAddress, cpTokenPath, workerTokenPath, cpRecord, workerRecord, cniFixtures, kubeconfigPath, evidenceDir); err != nil {
			collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
			collectTwoNodeDiagnostics("", nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
			t.Fatalf("Kubernetes upgrade proof failed: %v", err)
		}
	}
	collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
	if err := writeOperationBackedArtifactManifest(artifactManifestPath, result, inputs, nodes, evidenceArtifacts); err != nil {
		t.Fatal(err)
	}
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusPassed, "")
}

func runTwoNodeKubeadmUpgradeProof(t *testing.T, ctx context.Context, smoke operationBackedSmokeRun, cpNode, workerNode vmtest.RunningInstalledRuntimeNode, cpAddress, workerAddress, cpTokenPath, workerTokenPath string, cpBootstrap, workerBootstrap operation.OperationRecord, cniFixtures map[string]nodeCNIFixture, kubeconfigPath, evidenceDir string) error {
	t.Helper()
	const targetVersion = "v1.36.1"
	if smoke.Inputs.KubernetesVersion != "v1.36.0" {
		return fmt.Errorf("upgrade proof requires base v1.36.0, got %s", smoke.Inputs.KubernetesVersion)
	}
	for _, node := range []struct {
		running    vmtest.RunningInstalledRuntimeNode
		generation string
	}{{cpNode, cpBootstrap.CandidateGenerationID}, {workerNode, workerBootstrap.CandidateGenerationID}} {
		if err := rebootIntoGeneration(ctx, node.running, node.generation); err != nil {
			return fmt.Errorf("reboot %s into bootstrap generation: %w", node.running.Name, err)
		}
		if err := activateNodeCNIFixture(ctx, node.running, cniFixtures[node.running.Name]); err != nil {
			return fmt.Errorf("reactivate %s CNI fixture after bootstrap reboot: %w", node.running.Name, err)
		}
	}
	if _, err := waitForKubectlNodes(ctx, kubeconfigPath, filepath.Join(evidenceDir, "kubectl-before-upgrade.txt"), 5*time.Minute, "node/cp-1", "node/worker-1"); err != nil {
		return fmt.Errorf("wait for cluster readiness after bootstrap generation reboot: %w", err)
	}
	bootIDs, err := captureNodeBootIDs(ctx, cpNode, workerNode)
	if err != nil {
		return err
	}
	if bundle := strings.TrimSpace(os.Getenv("KATL_VMTEST_KUBERNETES_UPGRADE_BUNDLE")); bundle != "" {
		return runPublishedKubernetesUpgradeCLIProof(ctx, katlRepoRoot(t), bundle, cpNode, workerNode, cpAddress, workerAddress, cpTokenPath, workerTokenPath, kubeconfigPath, evidenceDir, bootIDs)
	}
	targetHost := filepath.Join(katlRepoRoot(t), "_build/mkosi/katl-kubernetes-upgrade.raw")
	metadataHost := targetHost + ".json"
	if value := strings.TrimSpace(os.Getenv("KATL_VMTEST_KUBERNETES_UPGRADE_BUNDLE")); value != "" {
		image, err := kubernetesbundle.ParseImageReference(value)
		if err != nil {
			return fmt.Errorf("parse published target Kubernetes bundle: %w", err)
		}
		staged, err := kubernetesbundle.FetchAndStage(ctx, kubernetesbundle.Request{
			Source:           image.Source,
			Ref:              image.Value,
			CacheDir:         filepath.Join(smoke.Result.ManifestDir, "published-kubernetes-upgrade"),
			RuntimeInterface: "katl-runtime-1",
			Architecture:     "x86_64",
		})
		if err != nil {
			return fmt.Errorf("fetch published target Kubernetes bundle: %w", err)
		}
		if staged.PayloadVersion != targetVersion {
			return fmt.Errorf("published target Kubernetes bundle payload is %s, want %s", staged.PayloadVersion, targetVersion)
		}
		targetHost = staged.SysextPath
		metadataHost = staged.MetadataPath
	}
	target, err := os.ReadFile(targetHost)
	if err != nil {
		return fmt.Errorf("read target Kubernetes sysext: %w", err)
	}
	metadata, err := artifact.ReadLocal(metadataHost)
	if err != nil {
		return fmt.Errorf("read target Kubernetes sysext metadata: %w", err)
	}
	if metadata.PayloadVersion != targetVersion {
		return fmt.Errorf("target Kubernetes sysext payload is %s, want %s", metadata.PayloadVersion, targetVersion)
	}
	targetDigest := sha256.Sum256(target)
	targetSHA := hex.EncodeToString(targetDigest[:])
	if targetSHA != metadata.SHA256 {
		return fmt.Errorf("target Kubernetes sysext digest %s does not match metadata %s", targetSHA, metadata.SHA256)
	}
	guestTarget := "/var/lib/katl/artifacts/kubernetes-upgrade-v1.36.1.raw"
	for _, node := range []vmtest.RunningInstalledRuntimeNode{cpNode, workerNode} {
		if err := writeNodeFileChunked(ctx, node, guestTarget, target, 0o600); err != nil {
			return fmt.Errorf("upload target sysext to %s: %w", node.Name, err)
		}
	}
	snapshot, err := createUpgradeSnapshotEvidence(ctx, cpNode)
	if err != nil {
		return err
	}
	cpToken, err := os.ReadFile(cpTokenPath)
	if err != nil {
		return err
	}
	workerToken, err := os.ReadFile(workerTokenPath)
	if err != nil {
		return err
	}
	cpStatus, err := submitKubeadmUpgrade(ctx, cpAddress, strings.TrimSpace(string(cpToken)), agentapi.KubernetesSysextUpdateOperationRequest{
		TargetPayloadVersion: targetVersion, TargetSysextPath: guestTarget, TargetSysextSha256: targetSHA, TargetSysextSizeBytes: uint64(len(target)), CandidateGenerationId: "upgrade-v1361-cp", UpgradeRole: "apply", SourcePayloadVersion: "v1.36.0",
		SnapshotRef: snapshot.Ref, SnapshotDigest: snapshot.Digest, SnapshotRevision: snapshot.Revision, SnapshotCreatedAt: snapshot.CreatedAt, CapturedMemberListDigest: snapshot.MemberListDigest, SourceEtcdVersion: snapshot.EtcdVersion, SnapshotStorageLocation: snapshot.Location, SnapshotOperatorIdentity: "vmtest:kubeadm-upgrade",
	})
	if err != nil {
		return fmt.Errorf("control-plane upgrade: %w", err)
	}
	if err := assertSuccessfulUpgradeStatus(cpStatus, "upgrade-v1361-cp"); err != nil {
		return err
	}
	if _, err := waitForKubectlNodes(ctx, kubeconfigPath, filepath.Join(evidenceDir, "kubectl-after-control-plane-upgrade.txt"), 5*time.Minute, "node/cp-1", "node/worker-1"); err != nil {
		return err
	}
	workerStatus, err := submitKubeadmUpgrade(ctx, workerAddress, strings.TrimSpace(string(workerToken)), agentapi.KubernetesSysextUpdateOperationRequest{
		TargetPayloadVersion: targetVersion, TargetSysextPath: guestTarget, TargetSysextSha256: targetSHA, TargetSysextSizeBytes: uint64(len(target)), CandidateGenerationId: "upgrade-v1361-worker", UpgradeRole: "worker", SourcePayloadVersion: "v1.36.0", SnapshotRef: snapshot.Ref, SnapshotDigest: snapshot.Digest,
	})
	if err != nil {
		return fmt.Errorf("worker upgrade: %w", err)
	}
	if err := assertSuccessfulUpgradeStatus(workerStatus, "upgrade-v1361-worker"); err != nil {
		return err
	}
	if _, err := waitForKubectlNodes(ctx, kubeconfigPath, filepath.Join(evidenceDir, "kubectl-after-worker-upgrade.txt"), 5*time.Minute, "node/cp-1", "node/worker-1"); err != nil {
		return err
	}
	if err := assertNodeBootIDsUnchanged(ctx, bootIDs, cpNode, workerNode); err != nil {
		return err
	}
	for _, item := range []struct {
		node vmtest.RunningInstalledRuntimeNode
		kind string
	}{{cpNode, "kubeadm-upgrade"}, {workerNode, "kubeadm-upgrade"}} {
		dir := filepath.Join(evidenceDir, item.node.Name, "upgrade")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		_, record, err := collectOperationEvidence(ctx, item.node, dir, item.kind)
		if err != nil {
			return err
		}
		if record.KubernetesSysextUpdate == nil || record.KubernetesSysextUpdate.TargetPayloadVersion != targetVersion || record.KubeadmUpgradeEvidence == nil || record.KubeadmUpgradeEvidence.TargetKubeadmObservedVersion != targetVersion || record.KubeadmUpgradeEvidence.KubeletGateState != "target-observed" {
			return fmt.Errorf("%s upgrade evidence incomplete: %+v", item.node.Name, record)
		}
		_, generationRecord, err := collectGenerationEvidence(ctx, item.node, dir, record.CandidateGenerationID)
		if err != nil {
			return err
		}
		if generationRecord.Status.CommitState != generation.CommitStateCommitted {
			return fmt.Errorf("%s upgrade candidate is %s", item.node.Name, generationRecord.Status.CommitState)
		}
	}
	return nil
}

func runPublishedKubernetesUpgradeCLIProof(ctx context.Context, repoRoot, bundle string, cpNode, workerNode vmtest.RunningInstalledRuntimeNode, cpAddress, workerAddress, cpTokenPath, workerTokenPath, kubeconfigPath, evidenceDir string, bootIDs map[string]string) error {
	configPath := filepath.Join(evidenceDir, "katlctl-upgrade.yaml")
	config := fmt.Sprintf("currentContext: vmtest\ncontexts:\n  - name: vmtest\n    cluster: upgrade\nclusters:\n  - name: upgrade\n    controlPlaneEndpoint: %s:6443\n    nodes:\n      - name: cp-1\n        managementEndpoint: %s:9443\n        systemRole: control-plane\n        credentialRef: file:%s\n      - name: worker-1\n        managementEndpoint: %s:9443\n        systemRole: worker\n        credentialRef: file:%s\n", cpAddress, cpAddress, cpTokenPath, workerAddress, workerTokenPath)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return fmt.Errorf("write katlctl upgrade context: %w", err)
	}
	image, err := kubernetesbundle.ParseImageReference(bundle)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "go", "run", "./cmd/katlctl", "kubernetes", "upgrade", image.ArtifactVersion, "--context-file", configPath, "--timeout", "25m")
	command.Dir = repoRoot
	stdout, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			_ = os.WriteFile(filepath.Join(evidenceDir, "katlctl-kubernetes-upgrade.stderr"), exitErr.Stderr, 0o600)
		}
		return fmt.Errorf("katlctl Kubernetes upgrade rollout: %w", err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "katlctl-kubernetes-upgrade.json"), stdout, 0o600); err != nil {
		return err
	}
	var report struct {
		SourceVersion string `json:"sourceVersion"`
		TargetVersion string `json:"targetVersion"`
		Nodes         []struct {
			Name   string `json:"name"`
			Result string `json:"result"`
			Phase  string `json:"phase"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(stdout, &report); err != nil {
		return fmt.Errorf("decode katlctl Kubernetes upgrade report: %w", err)
	}
	if report.SourceVersion != "v1.36.0" || report.TargetVersion != "v1.36.1" || len(report.Nodes) != 2 {
		return fmt.Errorf("unexpected katlctl Kubernetes upgrade report: %+v", report)
	}
	for i, want := range []string{"cp-1", "worker-1"} {
		if report.Nodes[i].Name != want || report.Nodes[i].Result != operation.ResultSucceeded || report.Nodes[i].Phase != "healthy" {
			return fmt.Errorf("unexpected katlctl Kubernetes upgrade node report %d: %+v", i, report.Nodes[i])
		}
	}
	if _, err := waitForKubectlNodes(ctx, kubeconfigPath, filepath.Join(evidenceDir, "kubectl-after-upgrade.txt"), 5*time.Minute, "node/cp-1", "node/worker-1"); err != nil {
		return err
	}
	if err := assertNodeBootIDsUnchanged(ctx, bootIDs, cpNode, workerNode); err != nil {
		return err
	}
	for _, item := range []vmtest.RunningInstalledRuntimeNode{cpNode, workerNode} {
		dir := filepath.Join(evidenceDir, item.Name, "upgrade")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		_, record, err := collectOperationEvidence(ctx, item, dir, "kubeadm-upgrade")
		if err != nil {
			return err
		}
		versionRef := image.Repository + ":" + image.Tag
		if record.KubernetesSysextUpdate == nil || record.KubernetesSysextUpdate.KubernetesBundleRef != versionRef || record.KubernetesSysextUpdate.BundleManifestDigest == "" || record.KubernetesSysextUpdate.TargetSysextSHA256 == "" {
			return fmt.Errorf("%s upgrade did not resolve the published bundle internally: %+v", item.Name, record.KubernetesSysextUpdate)
		}
		if item.Name == cpNode.Name && (record.KubernetesSysextUpdate.SnapshotDigest == "" || record.KubernetesSysextUpdate.SnapshotStorageLocation == "") {
			return fmt.Errorf("%s upgrade did not capture etcd snapshot evidence", item.Name)
		}
	}
	return nil
}

type upgradeSnapshotEvidence struct{ Ref, Digest, Revision, CreatedAt, MemberListDigest, EtcdVersion, Location string }

func createUpgradeSnapshotEvidence(ctx context.Context, node vmtest.RunningInstalledRuntimeNode) (upgradeSnapshotEvidence, error) {
	deadline := time.Now().Add(2 * time.Minute)
	var container *vmtestpb.CommandResult
	var lastErr error
	var id []string
	for {
		container, lastErr = runNodeCommandWithRetry(ctx, node, []string{"crictl", "ps", "--state", "Running", "--name", "etcd", "-q"}, 16<<10)
		if lastErr == nil && container.ExitStatus == 0 {
			id = strings.Fields(string(container.Stdout))
			if len(id) == 1 {
				break
			}
			lastErr = fmt.Errorf("expected one running etcd container, got %q", container.Stdout)
		} else {
			lastErr = errors.Join(lastErr, commandErrorDetail(container))
		}
		if time.Now().After(deadline) {
			return upgradeSnapshotEvidence{}, fmt.Errorf("locate etcd container: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return upgradeSnapshotEvidence{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	location := "/var/lib/etcd/katl-upgrade-v1361.db"
	etcdArgs := []string{"crictl", "exec", id[0], "etcdctl", "--endpoints=https://127.0.0.1:2379", "--cacert=/etc/kubernetes/pki/etcd/ca.crt", "--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt", "--key=/etc/kubernetes/pki/etcd/healthcheck-client.key"}
	saved, err := runNodeCommandWithRetry(ctx, node, append(append([]string(nil), etcdArgs...), "snapshot", "save", location), 64<<10)
	if err != nil || saved.ExitStatus != 0 {
		return upgradeSnapshotEvidence{}, fmt.Errorf("save etcd snapshot: %w", errors.Join(err, commandErrorDetail(saved)))
	}
	hashResult, err := runNodeCommandWithRetry(ctx, node, []string{"sha256sum", location}, 4<<10)
	if err != nil || hashResult.ExitStatus != 0 {
		return upgradeSnapshotEvidence{}, fmt.Errorf("hash etcd snapshot: %w", errors.Join(err, commandErrorDetail(hashResult)))
	}
	digest := strings.Fields(string(hashResult.Stdout))
	if len(digest) == 0 {
		return upgradeSnapshotEvidence{}, fmt.Errorf("empty etcd snapshot digest")
	}
	members, err := runNodeCommandWithRetry(ctx, node, append(append([]string(nil), etcdArgs...), "member", "list"), 64<<10)
	if err != nil || members.ExitStatus != 0 {
		return upgradeSnapshotEvidence{}, fmt.Errorf("list etcd members: %w", errors.Join(err, commandErrorDetail(members)))
	}
	memberDigest := sha256.Sum256(members.Stdout)
	return upgradeSnapshotEvidence{Ref: "vmtest-cp1-v1360-before-v1361", Digest: digest[0], CreatedAt: time.Now().UTC().Format(time.RFC3339), MemberListDigest: hex.EncodeToString(memberDigest[:]), Location: location}, nil
}

func submitKubeadmUpgrade(ctx context.Context, address, token string, request agentapi.KubernetesSysextUpdateOperationRequest) (*agentapi.OperationStatus, error) {
	connector := cluster.TCPAgentConnector{AuthToken: token, DialTimeout: 10 * time.Second}
	conn, err := connector.Connect(ctx, inventory.PlannedNode{Name: address, Address: address, Access: inventory.Access{Method: "agent"}})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	nodeStatus, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return nil, err
	}
	accepted, err := conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{ApiVersion: operation.APIVersion, Kind: "SubmitOperationRequest", ClientRequestId: "vmtest-" + request.CandidateGenerationId, OperationKind: "kubeadm-upgrade", Actor: "vmtest:kubeadm-upgrade", ExpectedMachineId: nodeStatus.MachineId, KubernetesSysextUpdate: &request})
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(20 * time.Minute)
	for {
		status, err := conn.Client.GetOperation(ctx, &agentapi.GetOperationRequest{OperationId: accepted.OperationId, ExpectedRequestDigest: accepted.RequestDigest, IncludeDiagnostics: "normal"})
		if err != nil {
			return nil, err
		}
		if status.Terminal {
			return status, nil
		}
		if time.Now().After(deadline) {
			return status, fmt.Errorf("operation %s did not finish before deadline", accepted.OperationId)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func assertSuccessfulUpgradeStatus(status *agentapi.OperationStatus, candidate string) error {
	if status == nil || !status.Terminal || status.Result != operation.ResultSucceeded || status.RecoveryRequired || status.CandidateGenerationId != candidate || status.GenerationCommitState != operation.GenerationCommitCommitted || status.PostKubeadmHealthState != operation.PostKubeadmHealthPassed || status.BootHealthPending || status.ActivationState != operation.ActivationStateActiveLive {
		return fmt.Errorf("upgrade operation did not commit healthy candidate %s: %+v", candidate, status)
	}
	return nil
}

func captureNodeBootIDs(ctx context.Context, nodes ...vmtest.RunningInstalledRuntimeNode) (map[string]string, error) {
	result := make(map[string]string, len(nodes))
	for _, node := range nodes {
		bootID, err := nodeBootID(ctx, node)
		if err != nil {
			return nil, fmt.Errorf("read %s boot id before Kubernetes upgrade: %w", node.Name, err)
		}
		result[node.Name] = bootID
	}
	return result, nil
}

func assertNodeBootIDsUnchanged(ctx context.Context, before map[string]string, nodes ...vmtest.RunningInstalledRuntimeNode) error {
	for _, node := range nodes {
		after, err := nodeBootID(ctx, node)
		if err != nil {
			return fmt.Errorf("read %s boot id after Kubernetes upgrade: %w", node.Name, err)
		}
		if after == "" || after != before[node.Name] {
			return fmt.Errorf("node %s rebooted during online Kubernetes upgrade: boot id %q -> %q", node.Name, before[node.Name], after)
		}
	}
	return nil
}

func nodeBootID(ctx context.Context, node vmtest.RunningInstalledRuntimeNode) (string, error) {
	health, err := retryDirectAgentOp(ctx, node, 10*time.Second, func(opCtx context.Context, client *vmtest.AgentClient) (*vmtestpb.HealthResponse, error) {
		return client.Health(opCtx)
	})
	if err != nil {
		return "", err
	}
	if bootID := strings.TrimSpace(health.BootId); bootID != "" {
		return bootID, nil
	}
	return "", fmt.Errorf("vmtest agent returned an empty boot id")
}

func rebootIntoGeneration(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, generationID string) error {
	result, err := runNodeCommand(ctx, node, []string{
		"systemd-run", "--quiet", "--collect", "--unit=katl-vmtest-reboot-into-generation", "--on-active=1s",
		"/usr/bin/systemctl", "reboot", "--no-block",
	}, 4<<10)
	if err != nil {
		return fmt.Errorf("schedule reboot: %w", err)
	}
	if result.ExitStatus != 0 {
		return fmt.Errorf("schedule reboot: %w", commandErrorDetail(result))
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		data, err := readNodeFile(ctx, node, "/proc/cmdline", 64<<10)
		if err == nil && strings.Contains(string(data), "katl.generation="+generationID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("node %s did not boot generation %s", node.Name, generationID)
}

func operationBackedVMConfigForRun(run operationBackedSmokeRun, mac string, cid uint32) vmtest.VMConfig {
	config := twoNodeVMConfig(run.Options.KVM, cid)
	config.LibvirtURI = run.LibvirtURI
	config.LibvirtNetwork = run.Network
	config.Network.MAC = mac
	return config
}

func stageOperationBackedKubernetesPayloadBundle(repo string, result vmtest.Result, gateway, kubernetesVersion string) (threeControlPlaneKubernetesPayloadBundle, guestReachableBundleServer, error) {
	if value := strings.TrimSpace(os.Getenv("KATL_VMTEST_KUBERNETES_BUNDLE")); value != "" {
		image, err := kubernetesbundle.ParseImageReference(value)
		if err != nil {
			return threeControlPlaneKubernetesPayloadBundle{}, guestReachableBundleServer{}, err
		}
		if image.PayloadVersion != kubernetesVersion {
			return threeControlPlaneKubernetesPayloadBundle{}, guestReachableBundleServer{}, fmt.Errorf("published Kubernetes bundle payload is %s, want %s", image.PayloadVersion, kubernetesVersion)
		}
		bundle := threeControlPlaneKubernetesPayloadBundle{
			Source:         image.Source,
			Ref:            image.Value,
			PayloadVersion: image.PayloadVersion,
			LogPath:        filepath.Join(result.RunDir, "kubernetes-payload-bundle.log"),
		}
		if err := writeKubernetesBundleSourceLog(bundle.LogPath, bundle); err != nil {
			return threeControlPlaneKubernetesPayloadBundle{}, guestReachableBundleServer{}, err
		}
		return bundle, guestReachableBundleServer{}, nil
	}
	bundle, err := stageThreeControlPlaneKubernetesPayloadBundles(repo, result, kubernetesVersion)
	if err != nil {
		return threeControlPlaneKubernetesPayloadBundle{}, guestReachableBundleServer{}, err
	}
	server, err := startGuestReachableKubernetesBundleServer(gateway, bundle)
	if err != nil {
		return threeControlPlaneKubernetesPayloadBundle{}, guestReachableBundleServer{}, err
	}
	bundle.Source = server.Source
	bundle.Ref = server.Ref
	bundle.BundleManifestDigest = server.BundleManifestDigest
	bundle.CACertPEM = server.CACertPEM
	bundle.CACertPath = filepath.Join(result.ManifestDir, "kubernetes-bundle-ca.pem")
	bundle.CACertGuestPath = "/var/lib/katl/test-artifacts/kubernetes-bundle-ca.pem"
	bundle.LogPath = filepath.Join(result.RunDir, "kubernetes-payload-bundle.log")
	if err := os.WriteFile(bundle.CACertPath, bundle.CACertPEM, 0o644); err != nil {
		server.Close()
		return threeControlPlaneKubernetesPayloadBundle{}, guestReachableBundleServer{}, err
	}
	if err := writeKubernetesBundleSourceLog(bundle.LogPath, bundle); err != nil {
		server.Close()
		return threeControlPlaneKubernetesPayloadBundle{}, guestReachableBundleServer{}, err
	}
	return bundle, server, nil
}

func TestInstalledRuntimeTwoNodeKubeadmJoinSmoke(t *testing.T) {
	if run, ok := twoNodeWorldSmokeRun(t); ok {
		runTwoNodeKubeadmJoinSmoke(t, run)
		return
	}

	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run two-node kubeadm join smoke")
	}
	_ = vmtest.RequireWorld(t)
}

type twoNodeSmokeRun struct {
	WorldScenario *vmtest.WorldScenario
	Options       vmtest.Options
	Runner        vmtest.Runner
	Scenario      vmtest.Scenario
	Result        vmtest.Result
	Inputs        twoNodeSmokeInputs
	LibvirtURI    string
	Network       string
}

func twoNodeWorldSmokeRun(t *testing.T) (twoNodeSmokeRun, bool) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(vmtest.WorldManifestEnv)) == "" {
		return twoNodeSmokeRun{}, false
	}
	world := vmtest.RequireWorld(t)
	repo := katlRepoRoot(t)
	kvm := vmtest.DefaultOptions().KVM
	if err := ensurePublishedRuntimeFixturesForWorld(world, repo, twoNodeWorldRuntimeSpecs(), kvm); err != nil {
		failWorldFixtureSetup(t, world, "installed-runtime-two-node-kubeadm-join", err)
	}
	run, err := planTwoNodeWorldSmokeRun(world, repo, firstString(os.Getenv("KATL_KUBERNETES_VERSION"), "v1.36.1"), kvm)
	if err != nil {
		failTwoNodeWorldSetup(t, run.WorldScenario, err)
	}
	missing := twoNodeHostToolPrereqs(exec.LookPath)
	requireSmokePrereqs(t, run.Runner, run.Scenario, run.Result, "two-node kubeadm join smoke prerequisites missing", missing)
	return run, true
}

func twoNodeWorldRuntimeSpecs() []vmtest.NodeSpec {
	return []vmtest.NodeSpec{
		{Name: "cp-1", Role: vmtest.ControlPlane},
		{Name: "worker-1", Role: vmtest.Worker},
	}
}

func planTwoNodeWorldSmokeRun(world vmtest.World, repo, kubernetesVersion string, kvm vmtest.KVMPolicy) (twoNodeSmokeRun, error) {
	scenario, err := world.PlanScenario("installed-runtime-two-node-kubeadm-join")
	if err != nil {
		return twoNodeSmokeRun{}, err
	}
	run := twoNodeSmokeRun{WorldScenario: scenario}
	buildRoots := publishedRuntimeBuildRoots(world, repo)
	cp, err := vmtest.AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, buildRoots, vmtest.NodeSpec{Name: "cp-1", Role: vmtest.ControlPlane})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	worker, err := vmtest.AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, buildRoots, vmtest.NodeSpec{Name: "worker-1", Role: vmtest.Worker})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	options := vmtest.Options{
		Enabled:   true,
		StateRoot: filepath.Join(scenario.Dir, "vm-runs"),
		Keep:      vmtest.KeepFailed,
		KVM:       kvm,
		Missing:   vmtest.MissingFails,
	}
	runner := vmtest.NewRunner(options)
	vmScenario := vmtest.Scenario{Name: "installed-runtime-two-node-kubeadm-join"}
	result, err := runner.Plan(vmScenario)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	result.Started = time.Now().UTC()
	return twoNodeSmokeRun{
		WorldScenario: scenario,
		Options:       options,
		Runner:        runner,
		Scenario:      vmScenario,
		Result:        result,
		LibvirtURI:    world.Libvirt.URI,
		Network:       world.Libvirt.Network,
		Inputs: twoNodeSmokeInputs{
			ControlPlaneDisk:       cp.Config.Disk,
			ControlPlaneDiskFormat: string(cp.Config.DiskFormat),
			ControlPlaneESP:        cp.Config.ESPArtifacts,
			ControlPlaneFixture:    cp.Config.FixtureManifest,
			ControlPlaneMetadata:   cp.Config.NodeMetadata,
			ControlPlaneAddress:    cp.Node.Address,
			ControlPlaneMAC:        cp.Node.MACAddress,
			WorkerDisk:             worker.Config.Disk,
			WorkerDiskFormat:       string(worker.Config.DiskFormat),
			WorkerESP:              worker.Config.ESPArtifacts,
			WorkerFixture:          worker.Config.FixtureManifest,
			WorkerMetadata:         worker.Config.NodeMetadata,
			WorkerAddress:          worker.Node.Address,
			WorkerMAC:              worker.Node.MACAddress,
			KubernetesVersion:      firstString(kubernetesVersion, "v1.36.1"),
			WorldProvenance:        twoNodeWorldProvenance(world, repo),
		},
	}, nil
}

func failTwoNodeWorldSetup(t *testing.T, scenario *vmtest.WorldScenario, err error) {
	t.Helper()
	if scenario == nil {
		t.Fatalf("%v", err)
	}
	if writeErr := scenario.WriteSetupFailure(err); writeErr != nil {
		t.Fatalf("write VM world setup failure: %v; original error: %v", writeErr, err)
	}
	t.Fatalf("%v\nworld scenario dir: %s", err, scenario.Dir)
}

func runTwoNodeKubeadmJoinSmoke(t *testing.T, smoke twoNodeSmokeRun) {
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
	transcriptDir := filepath.Join(result.RunDir, "agent-transcripts")
	inventoryPath := filepath.Join(result.ManifestDir, "bootstrap-inventory.yaml")
	kubeconfigPath := filepath.Join(result.RunDir, "operator-kubeconfig.yaml")
	kubeconfigMetadataPath := filepath.Join(result.RunDir, "operator-kubeconfig-metadata.json")
	stdoutPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stdout")
	stderrPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stderr")
	kubectlOut := filepath.Join(result.RunDir, "kubectl-get-nodes.txt")
	bootstrapFixture, err := stageBootstrapFixtureInputs(result.ManifestDir, bootstrapFixtureInputsForRun(katlRepoRoot(t)))
	if err != nil {
		t.Fatal(err)
	}
	cpResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	workerResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	plannedNodes := []vmtest.RunningInstalledRuntimeNode{
		{Name: "cp-1", Result: cpResult},
		{Name: "worker-1", Result: workerResult},
	}
	if err := writeTwoNodeSmokeArtifactManifest(result, inputs, transcriptDir, plannedNodes, bootstrapFixture, nil, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cpNode, err := vmtest.StartInstalledRuntimeNode(ctx, result, vmtest.InstalledRuntimeNodeConfig{
		Name: "cp-1",
		Runtime: vmtest.InstalledRuntimeConfig{
			Disk:            inputs.ControlPlaneDisk,
			DiskFormat:      vmtest.DiskFormat(inputs.ControlPlaneDiskFormat),
			ESPArtifacts:    inputs.ControlPlaneESP,
			FixtureManifest: inputs.ControlPlaneFixture,
			NodeMetadata:    inputs.ControlPlaneMetadata,
			VM:              twoNodeVMConfigForRun(smoke, inputs.ControlPlaneMAC, 0),
		},
	}, vmtest.VMRunner{})
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start control-plane VM: %v", err)
	}
	defer stopNode(t, cpNode)

	workerNode, err := vmtest.StartInstalledRuntimeNode(ctx, result, vmtest.InstalledRuntimeNodeConfig{
		Name: "worker-1",
		Runtime: vmtest.InstalledRuntimeConfig{
			Disk:            inputs.WorkerDisk,
			DiskFormat:      vmtest.DiskFormat(inputs.WorkerDiskFormat),
			ESPArtifacts:    inputs.WorkerESP,
			FixtureManifest: inputs.WorkerFixture,
			NodeMetadata:    inputs.WorkerMetadata,
			VM:              twoNodeVMConfigForRun(smoke, inputs.WorkerMAC, 0),
		},
	}, vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics(transcriptDir, cpNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start worker VM: %v", err)
	}
	defer stopNode(t, workerNode)

	nodes := []vmtest.RunningInstalledRuntimeNode{cpNode, workerNode}
	controlPlaneAddress := firstString(cpNode.Result.IPAddress, inputs.ControlPlaneAddress)
	workerAddress := firstString(workerNode.Result.IPAddress, inputs.WorkerAddress)
	cniFixtures, err := stageTwoNodeCNIFixtures(ctx, katlRepoRoot(t), cpNode, workerNode, controlPlaneAddress, workerAddress)
	if err != nil {
		collectTwoNodeDiagnostics(transcriptDir, cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("stage test CNI fixtures: %v", err)
	}
	imageFixtures, err := stageTwoNodeImageFixtures(ctx, katlRepoRoot(t), result.RunDir, nodes...)
	if err != nil {
		collectTwoNodeDiagnostics(transcriptDir, cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("stage test workload images: %v", err)
	}
	if err := writeTwoNodeInventory(inventoryPath, inputs.KubernetesVersion, cpNode, workerNode); err != nil {
		t.Fatal(err)
	}
	if err := writeTwoNodeSmokeArtifactManifest(result, inputs, transcriptDir, nodes, bootstrapFixture, cniFixtures, imageFixtures); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = runKatlctlCommand(t, ctx, katlRepoRoot(t), appendBootstrapFixtureArgs([]string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--init-node", "cp-1",
		"--control-plane-endpoint", controlPlaneAddress + ":6443",
		"--node-address", "cp-1=" + controlPlaneAddress,
		"--node-address", "worker-1=" + workerAddress,
		"--kubeconfig-out", kubeconfigPath,
		"--overwrite-kubeconfig",
		"--vmtest-transcript-dir", transcriptDir,
	}, bootstrapFixture), &stdout, &stderr)
	_ = os.WriteFile(stdoutPath, stdout.Bytes(), 0o644)
	_ = os.WriteFile(stderrPath, stderr.Bytes(), 0o644)
	_ = writeKubeconfigMetadata(kubeconfigPath, kubeconfigMetadataPath)
	err = bootstrapCommandError(err, stdout.String())
	if err != nil {
		collectKubectlDiagnosticsForFailure(ctx, cpNode, kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics(transcriptDir, cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("katlctl cluster bootstrap failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	assertBootstrapPhases(t, stdout.String())
	if err := verifyTwoNodeBootstrapTranscripts(transcriptDir); err != nil {
		collectTwoNodeDiagnostics(transcriptDir, cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("bootstrap transcripts: %v", err)
	}

	output, err := waitForKubectlNodes(ctx, kubeconfigPath, kubectlOut, 3*time.Minute, "node/cp-1", "node/worker-1")
	if err != nil {
		collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics(transcriptDir, cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("kubectl nodes did not converge: %v\n%s", err, output)
	}
	for _, want := range []string{"node/cp-1", "node/worker-1"} {
		if !strings.Contains(string(output), want) {
			collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
			collectTwoNodeDiagnostics(transcriptDir, cpNode, workerNode)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, "kubectl output missing "+want)
			t.Fatalf("kubectl output missing %q:\n%s", want, output)
		}
	}
	collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusPassed, "")
}

func twoNodeVMConfig(kvm vmtest.KVMPolicy, cid uint32) vmtest.VMConfig {
	return vmtest.VMConfig{
		KVM:     kvm,
		RAMMiB:  2048,
		CPUs:    2,
		Timeout: 25 * time.Minute,
		VSock: vmtest.VSockConfig{
			Enabled:  true,
			GuestCID: cid,
		},
	}
}

func twoNodeVMConfigForRun(run twoNodeSmokeRun, mac string, cid uint32) vmtest.VMConfig {
	config := twoNodeVMConfig(run.Options.KVM, cid)
	config.LibvirtURI = run.LibvirtURI
	config.LibvirtNetwork = run.Network
	config.Network.MAC = mac
	return config
}

type twoNodeSmokeInputs struct {
	ControlPlaneDisk       string
	ControlPlaneDiskFormat string
	ControlPlaneESP        string
	ControlPlaneFixture    string
	ControlPlaneMetadata   string
	ControlPlaneAddress    string
	ControlPlaneMAC        string
	WorkerDisk             string
	WorkerDiskFormat       string
	WorkerESP              string
	WorkerFixture          string
	WorkerMetadata         string
	WorkerAddress          string
	WorkerMAC              string
	KubernetesVersion      string
	WorldProvenance        multiNodeWorldProvenancePaths
}

type multiNodeWorldProvenancePaths struct {
	VMTestRun                string
	WorldManifest            string
	HostCapabilities         string
	ResourceManifest         string
	ResourceManifestSHA256   string
	PackageLock              string
	PackageLockSHA256        string
	MkosiArtifactIndex       string
	NetworkLeaseFile         string
	FixtureProducerScenarios map[string]string
	FixtureProducerResults   map[string]string
}

func multiNodeWorldProvenanceForSpecs(world vmtest.World, repo string, specs []vmtest.NodeSpec) multiNodeWorldProvenancePaths {
	provenance := multiNodeWorldProvenancePaths{
		VMTestRun:              firstString(world.RunIndex, filepath.Join(world.RunDir, "run.json")),
		WorldManifest:          firstString(os.Getenv(vmtest.WorldManifestEnv), filepath.Join(world.RunDir, "world.json")),
		HostCapabilities:       filepath.Join(world.RunDir, "host-capabilities.json"),
		ResourceManifest:       world.ResourceManifest,
		ResourceManifestSHA256: world.ResourceDigest,
		PackageLock:            world.PackageLock,
		PackageLockSHA256:      world.PackageLockDigest,
		MkosiArtifactIndex:     os.Getenv("KATL_MKOSI_ARTIFACT_INDEX"),
		NetworkLeaseFile:       world.Network.LeaseFile,
	}
	if len(specs) == 0 {
		return provenance
	}
	provenance.FixtureProducerScenarios = make(map[string]string, len(specs))
	provenance.FixtureProducerResults = make(map[string]string, len(specs))
	for _, spec := range specs {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			continue
		}
		scenarioID := vmtest.FirstInstallRuntimeFixtureScenarioName(spec)
		scenarioDir := filepath.Join(world.ScenarioDir, scenarioID)
		provenance.FixtureProducerScenarios[name] = filepath.Join(scenarioDir, "scenario.json")
		provenance.FixtureProducerResults[name] = filepath.Join(scenarioDir, "result.json")
	}
	return provenance
}

func twoNodeWorldProvenance(world vmtest.World, repo string) multiNodeWorldProvenancePaths {
	return multiNodeWorldProvenanceForSpecs(world, repo, twoNodeWorldRuntimeSpecs())
}

func planStartedVMResult(t *testing.T, runner vmtest.Runner, scenario vmtest.Scenario) vmtest.Result {
	t.Helper()
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result.Started = time.Now().UTC()
	return result
}

func twoNodeHostToolPrereqs(lookPath func(string) (string, error)) []vmtest.MissingPrerequisite {
	var missing []vmtest.MissingPrerequisite
	if _, err := lookPath(selectedKubectl()); err != nil {
		missing = append(missing, vmtest.MissingPrerequisite{
			Name:   "kubectl",
			Detail: "required for host-side kubeconfig verification: " + err.Error(),
		})
	}
	return missing
}

func selectedKubectl() string {
	return firstString(os.Getenv("KATL_VMTEST_KUBECTL"), "kubectl")
}

func katlRepoRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(output))
}

func requireVMHost(t *testing.T, runner vmtest.Runner, scenario vmtest.Scenario, result vmtest.Result, requirements vmtest.HostRequirements) {
	t.Helper()
	err := runner.CheckHost(requirements)
	if err == nil {
		return
	}
	missing := []vmtest.MissingPrerequisite{{Name: "host prerequisites", Detail: err.Error()}}
	var prereq vmtest.PrereqError
	if errors.As(err, &prereq) {
		missing = prereq.Missing
	}
	if runner.Options.Missing == vmtest.MissingSkips {
		skipVMResult(t, runner, scenario, result, err.Error(), missing)
	}
	result.Missing = append(result.Missing, missing...)
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
	t.Fatalf("%v\nvmtest run dir: %s", err, result.RunDir)
}

func requireSmokePrereqs(t *testing.T, runner vmtest.Runner, scenario vmtest.Scenario, result vmtest.Result, prefix string, missing []vmtest.MissingPrerequisite) {
	t.Helper()
	if len(missing) == 0 {
		return
	}
	message := prefix + ": " + missingPrerequisiteSummary(missing)
	if runner.Options.Missing == vmtest.MissingSkips {
		skipVMResult(t, runner, scenario, result, message, missing)
	}
	result.Missing = append(result.Missing, missing...)
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, message)
	t.Fatalf("%s\nvmtest run dir: %s", message, result.RunDir)
}

func skipVMResult(t *testing.T, runner vmtest.Runner, scenario vmtest.Scenario, result vmtest.Result, message string, missing []vmtest.MissingPrerequisite) {
	t.Helper()
	result.Missing = append(result.Missing, missing...)
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusSkipped, message)
	t.Skip(message)
}

func missingPrerequisiteSummary(missing []vmtest.MissingPrerequisite) string {
	parts := make([]string, 0, len(missing))
	for _, item := range missing {
		if item.Detail == "" {
			parts = append(parts, item.Name)
			continue
		}
		parts = append(parts, item.Name+": "+item.Detail)
	}
	return strings.Join(parts, "; ")
}

func writeTwoNodeInventory(path string, kubernetesVersion string, cpNode vmtest.RunningInstalledRuntimeNode, workerNode vmtest.RunningInstalledRuntimeNode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data := `controlPlaneEndpoint: ""
kubernetesVersion: ` + kubernetesVersion + `
nodes:
- name: cp-1
  systemRole: control-plane
  access:
    method: agent
    credentialRef: vsock:` + uint32String(cpNode.VSock.GuestCID) + `:` + uint32String(cpNode.VSock.Port) + `
  kubeadmConfig:
    ref: control-plane
    path: /etc/katl/kubeadm/control-plane/config.yaml
    intent: control-plane
  kubernetesVersion: ` + kubernetesVersion + `
- name: worker-1
  systemRole: worker
  access:
    method: agent
    credentialRef: vsock:` + uint32String(workerNode.VSock.GuestCID) + `:` + uint32String(workerNode.VSock.Port) + `
  kubeadmConfig:
    ref: worker
    path: /etc/katl/kubeadm/worker/config.yaml
    intent: worker
  kubernetesVersion: ` + kubernetesVersion + `
`
	return os.WriteFile(path, []byte(data), 0o644)
}

func writeOperationBackedInventory(path, kubernetesVersion string, kubernetesBundle threeControlPlaneKubernetesPayloadBundle, cpAddress, workerAddress string, tokenFiles map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data := `controlPlaneEndpoint: ` + cpAddress + `:6443
kubernetesVersion: ` + kubernetesVersion + `
kubernetesBundle: ` + strconv.Quote(kubernetesBundle.Ref) + `
nodes:
- name: cp-1
  address: ` + cpAddress + `
  systemRole: control-plane
  access:
    method: agent
    credentialRef: ` + strconv.Quote("file:"+tokenFiles["cp-1"]) + `
  kubeadmConfig:
    ref: control-plane
    path: /etc/katl/kubeadm/control-plane/config.yaml
    intent: control-plane
  kubernetesVersion: ` + kubernetesVersion + `
- name: worker-1
  address: ` + workerAddress + `
  systemRole: worker
  access:
    method: agent
    credentialRef: ` + strconv.Quote("file:"+tokenFiles["worker-1"]) + `
  kubeadmConfig:
    ref: worker
    path: /etc/katl/kubeadm/worker/config.yaml
    intent: worker
  kubernetesVersion: ` + kubernetesVersion + `
`
	return os.WriteFile(path, []byte(data), 0o644)
}

type operationBackedArtifacts struct {
	Inventory            string `json:"inventory"`
	Kubeconfig           string `json:"kubeconfig"`
	KubeconfigMetadata   string `json:"kubeconfigMetadata,omitempty"`
	BootstrapStdout      string `json:"bootstrapStdout"`
	BootstrapStderr      string `json:"bootstrapStderr"`
	KubectlOutput        string `json:"kubectlOutput"`
	BootstrapFixture     bootstrapFixtureInputs
	CNIFixtures          map[string]nodeCNIFixture
	ImageFixtures        map[string][]nodeImageFixture
	KubernetesBundle     threeControlPlaneKubernetesPayloadBundle
	EvidenceDir          string            `json:"evidenceDir"`
	BootSelectionsBefore map[string]string `json:"bootSelectionsBefore,omitempty"`
	BootSelectionsAfter  map[string]string `json:"bootSelectionsAfter,omitempty"`
	OperationRecords     map[string]string `json:"operationRecords,omitempty"`
	OperationJournals    map[string]string `json:"operationJournals,omitempty"`
	GenerationMetadata   map[string]string `json:"generationMetadata,omitempty"`
	NodeStatus           map[string]string `json:"nodeStatus,omitempty"`
}

type operationBackedArtifactManifest struct {
	VMTestRun                string                                    `json:"vmtestRun,omitempty"`
	WorldManifest            string                                    `json:"worldManifest,omitempty"`
	HostCapabilities         string                                    `json:"hostCapabilities,omitempty"`
	ResourceManifest         string                                    `json:"resourceManifest,omitempty"`
	ResourceManifestSHA256   string                                    `json:"resourceManifestSHA256,omitempty"`
	PackageLock              string                                    `json:"packageLock,omitempty"`
	PackageLockSHA256        string                                    `json:"packageLockSHA256,omitempty"`
	MkosiArtifactIndex       string                                    `json:"mkosiArtifactIndex,omitempty"`
	ControlPlaneRunDir       string                                    `json:"controlPlaneRunDir"`
	WorkerRunDir             string                                    `json:"workerRunDir,omitempty"`
	NodeScenarios            map[string]string                         `json:"nodeScenarios,omitempty"`
	NodeResults              map[string]string                         `json:"nodeResults,omitempty"`
	LaunchCommands           map[string]string                         `json:"launchCommands,omitempty"`
	DomainXMLs               map[string]string                         `json:"domainXMLs,omitempty"`
	InstalledRuntimeInputs   map[string]string                         `json:"installedRuntimeInputs,omitempty"`
	VSockTranscripts         map[string]string                         `json:"vsockTranscripts,omitempty"`
	LibvirtLeases            map[string]string                         `json:"libvirtLeases,omitempty"`
	NodeDomains              map[string]string                         `json:"nodeDomains,omitempty"`
	NodeMACs                 map[string]string                         `json:"nodeMACs,omitempty"`
	NodeIPs                  map[string]string                         `json:"nodeIPs,omitempty"`
	FixtureInputs            map[string]nodeFixtureInput               `json:"fixtureInputs,omitempty"`
	FixtureProducerScenarios map[string]string                         `json:"fixtureProducerScenarios,omitempty"`
	FixtureProducerResults   map[string]string                         `json:"fixtureProducerResults,omitempty"`
	Inventory                string                                    `json:"inventory"`
	Kubeconfig               string                                    `json:"kubeconfig"`
	KubeconfigMetadata       string                                    `json:"kubeconfigMetadata,omitempty"`
	BootstrapStdout          string                                    `json:"bootstrapStdout"`
	BootstrapStderr          string                                    `json:"bootstrapStderr"`
	KubectlOutput            string                                    `json:"kubectlOutput"`
	BootstrapFixture         *bootstrapFixtureInputs                   `json:"bootstrapFixture,omitempty"`
	KubernetesPayloadBundle  *threeControlPlaneKubernetesPayloadBundle `json:"kubernetesPayloadBundle,omitempty"`
	CNIFixtures              map[string]nodeCNIFixture                 `json:"cniFixtures,omitempty"`
	ImageFixtures            map[string][]nodeImageFixture             `json:"imageFixtures,omitempty"`
	EvidenceDir              string                                    `json:"evidenceDir"`
	BootSelectionsBefore     map[string]string                         `json:"bootSelectionsBefore,omitempty"`
	BootSelectionsAfter      map[string]string                         `json:"bootSelectionsAfter,omitempty"`
	OperationRecords         map[string]string                         `json:"operationRecords,omitempty"`
	OperationJournals        map[string]string                         `json:"operationJournals,omitempty"`
	GenerationMetadata       map[string]string                         `json:"generationMetadata,omitempty"`
	NodeStatus               map[string]string                         `json:"nodeStatus,omitempty"`
	KubectlDiagnostics       map[string]string                         `json:"kubectlDiagnostics,omitempty"`
	SerialLogs               map[string]string                         `json:"serialLogs,omitempty"`
	NetworkLeases            string                                    `json:"networkLeases,omitempty"`
	Diagnostics              map[string]string                         `json:"diagnostics,omitempty"`
}

func operationBackedFixtureInputs(inputs operationBackedSmokeInputs) map[string]nodeFixtureInput {
	return map[string]nodeFixtureInput{
		"cp-1":     fixtureInput(inputs.ControlPlaneDisk, inputs.ControlPlaneDiskFormat, inputs.ControlPlaneESP, inputs.ControlPlaneFixture, inputs.ControlPlaneMetadata),
		"worker-1": fixtureInput(inputs.WorkerDisk, inputs.WorkerDiskFormat, inputs.WorkerESP, inputs.WorkerFixture, inputs.WorkerMetadata),
	}
}

func nodeRunDir(nodes []vmtest.RunningInstalledRuntimeNode, name string) string {
	for _, node := range nodes {
		if node.Name == name {
			return node.Result.RunDir
		}
	}
	return ""
}

func writeOperationBackedArtifactManifest(path string, result vmtest.Result, inputs operationBackedSmokeInputs, nodes []vmtest.RunningInstalledRuntimeNode, artifacts operationBackedArtifacts) error {
	var bundle *threeControlPlaneKubernetesPayloadBundle
	if strings.TrimSpace(artifacts.KubernetesBundle.Source) != "" || strings.TrimSpace(artifacts.KubernetesBundle.Ref) != "" {
		value := artifacts.KubernetesBundle
		bundle = &value
	}
	return writeTwoNodeDiagnosticJSON(path, operationBackedArtifactManifest{
		VMTestRun:                inputs.WorldProvenance.VMTestRun,
		WorldManifest:            inputs.WorldProvenance.WorldManifest,
		HostCapabilities:         inputs.WorldProvenance.HostCapabilities,
		ResourceManifest:         inputs.WorldProvenance.ResourceManifest,
		ResourceManifestSHA256:   inputs.WorldProvenance.ResourceManifestSHA256,
		PackageLock:              inputs.WorldProvenance.PackageLock,
		PackageLockSHA256:        inputs.WorldProvenance.PackageLockSHA256,
		MkosiArtifactIndex:       inputs.WorldProvenance.MkosiArtifactIndex,
		ControlPlaneRunDir:       nodeRunDir(nodes, "cp-1"),
		WorkerRunDir:             nodeRunDir(nodes, "worker-1"),
		NodeScenarios:            nodeScenarioPaths(nodes),
		NodeResults:              nodeResultPaths(nodes),
		LaunchCommands:           launchCommandPaths(nodes),
		DomainXMLs:               domainXMLPaths(nodes),
		InstalledRuntimeInputs:   installedRuntimeInputPaths(nodes),
		VSockTranscripts:         vsockTranscriptPaths(nodes),
		LibvirtLeases:            libvirtLeasePaths(nodes),
		NodeDomains:              nodeDomainNames(nodes),
		NodeMACs:                 nodeMACAddresses(nodes),
		NodeIPs:                  nodeIPAddresses(nodes),
		FixtureInputs:            operationBackedFixtureInputs(inputs),
		FixtureProducerScenarios: inputs.WorldProvenance.FixtureProducerScenarios,
		FixtureProducerResults:   inputs.WorldProvenance.FixtureProducerResults,
		Inventory:                artifacts.Inventory,
		Kubeconfig:               artifacts.Kubeconfig,
		KubeconfigMetadata:       artifacts.KubeconfigMetadata,
		BootstrapStdout:          artifacts.BootstrapStdout,
		BootstrapStderr:          artifacts.BootstrapStderr,
		KubectlOutput:            artifacts.KubectlOutput,
		BootstrapFixture:         artifacts.BootstrapFixture.manifestValue(),
		KubernetesPayloadBundle:  bundle,
		CNIFixtures:              artifacts.CNIFixtures,
		ImageFixtures:            artifacts.ImageFixtures,
		EvidenceDir:              artifacts.EvidenceDir,
		BootSelectionsBefore:     artifacts.BootSelectionsBefore,
		BootSelectionsAfter:      artifacts.BootSelectionsAfter,
		OperationRecords:         artifacts.OperationRecords,
		OperationJournals:        artifacts.OperationJournals,
		GenerationMetadata:       artifacts.GenerationMetadata,
		NodeStatus:               artifacts.NodeStatus,
		KubectlDiagnostics:       kubectlDiagnosticPaths(result.RunDir),
		SerialLogs:               serialLogPaths(nodes),
		NetworkLeases:            inputs.WorldProvenance.NetworkLeaseFile,
		Diagnostics:              diagnosticSummaryPaths(nodes),
	})
}

type twoNodeArtifactManifest struct {
	VMTestRun                string                        `json:"vmtestRun,omitempty"`
	WorldManifest            string                        `json:"worldManifest,omitempty"`
	HostCapabilities         string                        `json:"hostCapabilities,omitempty"`
	ResourceManifest         string                        `json:"resourceManifest,omitempty"`
	ResourceManifestSHA256   string                        `json:"resourceManifestSHA256,omitempty"`
	PackageLock              string                        `json:"packageLock,omitempty"`
	PackageLockSHA256        string                        `json:"packageLockSHA256,omitempty"`
	MkosiArtifactIndex       string                        `json:"mkosiArtifactIndex,omitempty"`
	ControlPlaneRunDir       string                        `json:"controlPlaneRunDir"`
	WorkerRunDir             string                        `json:"workerRunDir"`
	NodeScenarios            map[string]string             `json:"nodeScenarios,omitempty"`
	NodeResults              map[string]string             `json:"nodeResults,omitempty"`
	LaunchCommands           map[string]string             `json:"launchCommands,omitempty"`
	DomainXMLs               map[string]string             `json:"domainXMLs,omitempty"`
	InstalledRuntimeInputs   map[string]string             `json:"installedRuntimeInputs,omitempty"`
	VSockTranscripts         map[string]string             `json:"vsockTranscripts,omitempty"`
	LibvirtLeases            map[string]string             `json:"libvirtLeases,omitempty"`
	NodeDomains              map[string]string             `json:"nodeDomains,omitempty"`
	NodeMACs                 map[string]string             `json:"nodeMACs,omitempty"`
	NodeIPs                  map[string]string             `json:"nodeIPs,omitempty"`
	FixtureInputs            map[string]nodeFixtureInput   `json:"fixtureInputs,omitempty"`
	FixtureProducerScenarios map[string]string             `json:"fixtureProducerScenarios,omitempty"`
	FixtureProducerResults   map[string]string             `json:"fixtureProducerResults,omitempty"`
	Inventory                string                        `json:"inventory"`
	Kubeconfig               string                        `json:"kubeconfig"`
	KubeconfigMetadata       string                        `json:"kubeconfigMetadata,omitempty"`
	BootstrapStdout          string                        `json:"bootstrapStdout"`
	BootstrapStderr          string                        `json:"bootstrapStderr"`
	BootstrapFixture         *bootstrapFixtureInputs       `json:"bootstrapFixture,omitempty"`
	CNIFixtures              map[string]nodeCNIFixture     `json:"cniFixtures,omitempty"`
	ImageFixtures            map[string][]nodeImageFixture `json:"imageFixtures,omitempty"`
	KubectlOutput            string                        `json:"kubectlOutput"`
	KubectlDiagnostics       map[string]string             `json:"kubectlDiagnostics,omitempty"`
	ControlPlaneTranscript   string                        `json:"controlPlaneTranscript"`
	WorkerTranscript         string                        `json:"workerTranscript"`
	SerialLogs               map[string]string             `json:"serialLogs,omitempty"`
	NetworkLeases            string                        `json:"networkLeases,omitempty"`
	Diagnostics              map[string]string             `json:"diagnostics,omitempty"`
}

func writeTwoNodeSmokeArtifactManifest(result vmtest.Result, inputs twoNodeSmokeInputs, transcriptDir string, nodes []vmtest.RunningInstalledRuntimeNode, bootstrapFixture bootstrapFixtureInputs, cniFixtures map[string]nodeCNIFixture, imageFixtures map[string][]nodeImageFixture) error {
	nodeByName := nodeMap(nodes)
	return writeTwoNodeArtifactManifest(filepath.Join(result.ManifestDir, "two-node-artifacts.json"), twoNodeArtifactManifest{
		VMTestRun:                inputs.WorldProvenance.VMTestRun,
		WorldManifest:            inputs.WorldProvenance.WorldManifest,
		HostCapabilities:         inputs.WorldProvenance.HostCapabilities,
		ResourceManifest:         inputs.WorldProvenance.ResourceManifest,
		ResourceManifestSHA256:   inputs.WorldProvenance.ResourceManifestSHA256,
		PackageLock:              inputs.WorldProvenance.PackageLock,
		PackageLockSHA256:        inputs.WorldProvenance.PackageLockSHA256,
		MkosiArtifactIndex:       inputs.WorldProvenance.MkosiArtifactIndex,
		ControlPlaneRunDir:       nodeByName["cp-1"].Result.RunDir,
		WorkerRunDir:             nodeByName["worker-1"].Result.RunDir,
		NodeScenarios:            nodeScenarioPaths(nodes),
		NodeResults:              nodeResultPaths(nodes),
		LaunchCommands:           launchCommandPaths(nodes),
		DomainXMLs:               domainXMLPaths(nodes),
		InstalledRuntimeInputs:   installedRuntimeInputPaths(nodes),
		VSockTranscripts:         vsockTranscriptPaths(nodes),
		LibvirtLeases:            libvirtLeasePaths(nodes),
		NodeDomains:              nodeDomainNames(nodes),
		NodeMACs:                 nodeMACAddresses(nodes),
		NodeIPs:                  nodeIPAddresses(nodes),
		FixtureInputs:            twoNodeFixtureInputs(inputs.ControlPlaneDisk, inputs.ControlPlaneDiskFormat, inputs.WorkerDisk, inputs.WorkerDiskFormat, inputs.ControlPlaneESP, inputs.WorkerESP, inputs.ControlPlaneFixture, inputs.WorkerFixture, inputs.ControlPlaneMetadata, inputs.WorkerMetadata),
		FixtureProducerScenarios: inputs.WorldProvenance.FixtureProducerScenarios,
		FixtureProducerResults:   inputs.WorldProvenance.FixtureProducerResults,
		Inventory:                filepath.Join(result.ManifestDir, "bootstrap-inventory.yaml"),
		Kubeconfig:               filepath.Join(result.RunDir, "operator-kubeconfig.yaml"),
		KubeconfigMetadata:       filepath.Join(result.RunDir, "operator-kubeconfig-metadata.json"),
		BootstrapStdout:          filepath.Join(result.RunDir, "katlctl-bootstrap.stdout"),
		BootstrapStderr:          filepath.Join(result.RunDir, "katlctl-bootstrap.stderr"),
		BootstrapFixture:         bootstrapFixture.manifestValue(),
		CNIFixtures:              cniFixtures,
		ImageFixtures:            imageFixtures,
		KubectlOutput:            filepath.Join(result.RunDir, "kubectl-get-nodes.txt"),
		KubectlDiagnostics:       kubectlDiagnosticPaths(result.RunDir),
		ControlPlaneTranscript:   twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1"),
		WorkerTranscript:         twoNodeBootstrapTranscriptPath(transcriptDir, "worker-1"),
		SerialLogs:               serialLogPaths(nodes),
		NetworkLeases:            inputs.WorldProvenance.NetworkLeaseFile,
		Diagnostics:              diagnosticSummaryPaths(nodes),
	})
}

type nodeFixtureInput struct {
	Disk            string `json:"disk"`
	DiskFormat      string `json:"diskFormat"`
	ESPArtifacts    string `json:"espArtifacts"`
	FixtureManifest string `json:"fixtureManifest"`
	NodeMetadata    string `json:"nodeMetadata"`
}

type bootstrapFixtureInputs struct {
	Manifests []string `json:"manifests,omitempty"`
	PreWaits  []string `json:"preWaits,omitempty"`
	Waits     []string `json:"waits,omitempty"`
}

type nodeCNIFixture struct {
	Source           string `json:"source"`
	GuestSource      string `json:"guestSource"`
	GuestTarget      string `json:"guestTarget"`
	PodSubnet        string `json:"podSubnet"`
	PodGateway       string `json:"podGateway"`
	PeerSubnet       string `json:"peerSubnet"`
	PeerAddress      string `json:"peerAddress"`
	PluginSource     string `json:"pluginSource"`
	PluginTarget     string `json:"pluginTarget"`
	ContainerdConfig string `json:"containerdConfig"`
	ContainerdDropIn string `json:"containerdDropIn"`
}

type nodeImageFixture struct {
	Image     string `json:"image"`
	Source    string `json:"source"`
	GuestPath string `json:"guestPath"`
}

func (i bootstrapFixtureInputs) empty() bool {
	return len(i.Manifests) == 0 && len(i.PreWaits) == 0 && len(i.Waits) == 0
}

func (i bootstrapFixtureInputs) manifestValue() *bootstrapFixtureInputs {
	if i.empty() {
		return nil
	}
	return &i
}

func bootstrapFixtureInputsFromEnv() bootstrapFixtureInputs {
	return bootstrapFixtureInputs{
		Manifests: bootstrapManifestInputsFromEnv(),
		Waits:     bootstrapWaitInputsFromEnv(),
	}
}

func bootstrapFixtureInputsForRun(repo string) bootstrapFixtureInputs {
	inputs := usableClusterBootstrapFixtureInputs(repo)
	env := bootstrapFixtureInputsFromEnv()
	inputs.Manifests = append(inputs.Manifests, env.Manifests...)
	inputs.Waits = append(inputs.Waits, env.Waits...)
	return inputs
}

func usableClusterBootstrapFixtureInputs(repo string) bootstrapFixtureInputs {
	root := filepath.Join(repo, "internal", "vmtest", "scenarios", "testdata", "bootstrap")
	return bootstrapFixtureInputs{
		Manifests: []string{
			filepath.Join(root, "cross-node-workload.yaml"),
		},
		PreWaits: []string{
			"nodes-ready",
		},
		Waits: []string{
			"rollout-status:katl-vmtest:deployment/net-server",
			"condition:katl-vmtest:job/net-client:Complete",
		},
	}
}

func stageTwoNodeCNIFixtures(ctx context.Context, repo string, cpNode, workerNode vmtest.RunningInstalledRuntimeNode, cpAddress, workerAddress string) (map[string]nodeCNIFixture, error) {
	source := filepath.Join(repo, "internal", "vmtest", "scenarios", "testdata", "bootstrap", "bridge-cni.conflist")
	fixtures := map[string]nodeCNIFixture{}
	cp, err := stageNodeCNIFixture(ctx, cpNode, source, "10.244.0.0/24", "10.244.0.1", "10.244.1.0/24", workerAddress)
	if err != nil {
		return nil, fmt.Errorf("stage cp-1 CNI: %w", err)
	}
	worker, err := stageNodeCNIFixture(ctx, workerNode, source, "10.244.1.0/24", "10.244.1.1", "10.244.0.0/24", cpAddress)
	if err != nil {
		return nil, fmt.Errorf("stage worker-1 CNI: %w", err)
	}
	fixtures["cp-1"] = cp
	fixtures["worker-1"] = worker
	return fixtures, nil
}

func stageNodeCNIFixture(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, source, podSubnet, podGateway, peerSubnet, peerAddress string) (nodeCNIFixture, error) {
	data, err := os.ReadFile(source)
	if err != nil {
		return nodeCNIFixture{}, err
	}
	text := strings.ReplaceAll(string(data), "__POD_SUBNET__", podSubnet)
	text = strings.ReplaceAll(text, "__POD_GATEWAY__", podGateway)
	data = []byte(text)
	fixture := nodeCNIFixture{
		Source:           source,
		GuestSource:      "/var/lib/katl/test-artifacts/bootstrap-cni/source/10-katl-vmtest-bridge.conflist",
		GuestTarget:      "/var/lib/katl/test-artifacts/bootstrap-cni/net.d/10-katl-vmtest-bridge.conflist",
		PodSubnet:        podSubnet,
		PodGateway:       podGateway,
		PeerSubnet:       peerSubnet,
		PeerAddress:      peerAddress,
		PluginSource:     "/usr/libexec/cni",
		PluginTarget:     "/var/lib/katl/test-artifacts/bootstrap-cni/bin",
		ContainerdConfig: "/var/lib/katl/test-artifacts/bootstrap-cni/containerd-config.toml",
		ContainerdDropIn: "/run/systemd/system/containerd.service.d/10-katl-vmtest-cni.conf",
	}
	if err := writeNodeFile(ctx, node, fixture.GuestSource, data, 0o644, false); err != nil {
		return nodeCNIFixture{}, err
	}
	for _, plugin := range []string{"bridge", "host-local", "portmap", "loopback"} {
		argv := []string{"install", "-D", "-m", "0755", filepath.Join(fixture.PluginSource, plugin), filepath.Join(fixture.PluginTarget, plugin)}
		if result, err := runNodeCommand(ctx, node, argv, 32<<10); err != nil {
			return nodeCNIFixture{}, fmt.Errorf("install CNI plugin %s: %w", plugin, err)
		} else if result.ExitStatus != 0 {
			return nodeCNIFixture{}, fmt.Errorf("%s exited %d: %s", strings.Join(argv, " "), result.ExitStatus, strings.TrimSpace(string(result.Stderr)))
		}
	}
	if result, err := runNodeCommand(ctx, node, []string{"install", "-D", "-m", "0644", fixture.GuestSource, fixture.GuestTarget}, 32<<10); err != nil {
		return nodeCNIFixture{}, err
	} else if result.ExitStatus != 0 {
		return nodeCNIFixture{}, fmt.Errorf("install CNI config exited %d: %s", result.ExitStatus, strings.TrimSpace(string(result.Stderr)))
	}
	if err := activateNodeCNIFixture(ctx, node, fixture); err != nil {
		return nodeCNIFixture{}, err
	}
	return fixture, nil
}

func activateNodeCNIFixture(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, fixture nodeCNIFixture) error {
	if err := configureNodeContainerdCNI(ctx, node, fixture); err != nil {
		return err
	}
	if result, err := runNodeCommand(ctx, node, []string{"sysctl", "-w", "net.ipv4.ip_forward=1"}, 32<<10); err != nil {
		return err
	} else if result.ExitStatus != 0 {
		return fmt.Errorf("enable IPv4 forwarding: %s", commandErrorDetail(result))
	}
	route := []string{"ip", "route", "replace", fixture.PeerSubnet, "via", fixture.PeerAddress}
	deadline := time.Now().Add(time.Minute)
	for {
		result, err := runNodeCommand(ctx, node, route, 32<<10)
		if err == nil && result.ExitStatus == 0 {
			break
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("%s: %s", strings.Join(route, " "), commandErrorDetail(result))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil
}

func configureNodeContainerdCNI(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, fixture nodeCNIFixture) error {
	const (
		containerdLog = "/var/lib/katl/test-artifacts/bootstrap-cni/containerd.log"
		kubeletLog    = "/var/lib/katl/test-artifacts/bootstrap-cni/kubelet.log"
	)
	config := fmt.Sprintf(`version = 4

[plugins.'io.containerd.cri.v1.images']
  use_local_image_pull = true

[plugins.'io.containerd.cri.v1.images'.pinned_images]
  sandbox = "registry.k8s.io/pause:3.10.2"

[plugins.'io.containerd.cri.v1.runtime'.containerd]
  default_runtime_name = "crun"

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.crun]
  runtime_type = "io.containerd.runc.v2"

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.crun.options]
  BinaryName = "/usr/bin/crun"
  SystemdCgroup = true

[plugins.'io.containerd.cri.v1.runtime'.cni]
  bin_dirs = [%q]
  conf_dir = %q
`, fixture.PluginTarget, filepath.Dir(fixture.GuestTarget))
	if err := writeNodeFile(ctx, node, fixture.ContainerdConfig, []byte(config), 0o644, false); err != nil {
		return fmt.Errorf("write containerd CNI config: %w", err)
	}
	dropInSource := "/var/lib/katl/test-artifacts/bootstrap-cni/containerd-service-dropin.conf"
	dropIn := fmt.Sprintf(`[Service]
ExecStart=
ExecStart=/usr/bin/containerd --config %s
StandardOutput=append:%s
StandardError=append:%s
`, fixture.ContainerdConfig, containerdLog, containerdLog)
	if err := writeNodeFile(ctx, node, dropInSource, []byte(dropIn), 0o644, false); err != nil {
		return fmt.Errorf("write containerd drop-in source: %w", err)
	}
	kubeletDropInSource := "/var/lib/katl/test-artifacts/bootstrap-cni/kubelet-service-dropin.conf"
	kubeletDropIn := fmt.Sprintf(`[Service]
StandardOutput=append:%s
StandardError=append:%s
`, kubeletLog, kubeletLog)
	if err := writeNodeFile(ctx, node, kubeletDropInSource, []byte(kubeletDropIn), 0o644, false); err != nil {
		return fmt.Errorf("write kubelet drop-in source: %w", err)
	}
	for _, command := range []struct {
		name string
		argv []string
	}{
		{name: "install containerd drop-in", argv: []string{"install", "-D", "-m", "0644", dropInSource, fixture.ContainerdDropIn}},
		{name: "install kubelet drop-in", argv: []string{"install", "-D", "-m", "0644", kubeletDropInSource, "/run/systemd/system/kubelet.service.d/10-katl-vmtest-log.conf"}},
		{name: "reload systemd", argv: []string{"systemctl", "daemon-reload"}},
		{name: "restart containerd", argv: []string{"systemctl", "restart", "containerd.service"}},
		{name: "check containerd", argv: []string{"systemctl", "is-active", "--quiet", "containerd.service"}},
	} {
		if result, err := runNodeCommand(ctx, node, command.argv, 32<<10); err != nil {
			return fmt.Errorf("%s: %w", command.name, err)
		} else if result.ExitStatus != 0 {
			return fmt.Errorf("%s: %w", command.name, commandErrorDetail(result))
		}
	}
	return nil
}

func stageTwoNodeImageFixtures(ctx context.Context, repo, workDir string, nodes ...vmtest.RunningInstalledRuntimeNode) (map[string][]nodeImageFixture, error) {
	fixtures, err := buildBootstrapImageFixtures(ctx, repo, filepath.Join(workDir, "bootstrap-images"))
	if err != nil {
		return nil, err
	}
	staged := map[string][]nodeImageFixture{}
	for _, node := range nodes {
		for _, fixture := range fixtures {
			data, err := os.ReadFile(fixture.Source)
			if err != nil {
				return nil, fmt.Errorf("read image fixture %s: %w", fixture.Source, err)
			}
			nodeFixture := fixture
			nodeFixture.GuestPath = filepath.Join("/var/lib/katl/test-artifacts/bootstrap-images", filepath.Base(fixture.Source))
			if err := writeNodeFileChunked(ctx, node, nodeFixture.GuestPath, data, 0o644); err != nil {
				return nil, fmt.Errorf("stage %s image fixture on %s: %w", fixture.Image, node.Name, err)
			}
			if result, err := runNodeCommandWithRetry(ctx, node, []string{"ctr", "-n", "k8s.io", "images", "import", nodeFixture.GuestPath}, 64<<10); err != nil {
				return nil, fmt.Errorf("import %s image fixture on %s: %w", fixture.Image, node.Name, err)
			} else if result.ExitStatus != 0 {
				return nil, fmt.Errorf("import %s image fixture on %s: %w", fixture.Image, node.Name, commandErrorDetail(result))
			}
			staged[node.Name] = append(staged[node.Name], nodeFixture)
		}
	}
	return staged, nil
}

func buildBootstrapImageFixtures(ctx context.Context, repo, workDir string) ([]nodeImageFixture, error) {
	specs := []struct {
		image      string
		pkg        string
		binaryName string
		entrypoint string
		archive    string
	}{
		{
			image:      "localhost/katl-vmtest/net-server:latest",
			pkg:        "./internal/vmtest/testcmd/net-server",
			binaryName: "net-server",
			entrypoint: "/net-server",
			archive:    "net-server.tar",
		},
		{
			image:      "localhost/katl-vmtest/net-client:latest",
			pkg:        "./internal/vmtest/testcmd/net-client",
			binaryName: "net-client",
			entrypoint: "/net-client",
			archive:    "net-client.tar",
		},
		{
			image:      "localhost/katl-vmtest/gateway-proxy:latest",
			pkg:        "./internal/vmtest/testcmd/gateway-proxy",
			binaryName: "gateway-proxy",
			entrypoint: "/gateway-proxy",
			archive:    "gateway-proxy.tar",
		},
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	fixtures := make([]nodeImageFixture, 0, len(specs))
	for _, spec := range specs {
		binaryPath := filepath.Join(workDir, spec.binaryName)
		cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-trimpath", "-ldflags", "-s -w", "-o", binaryPath, spec.pkg)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("build %s fixture binary: %w\n%s", spec.binaryName, err, output)
		}
		archivePath := filepath.Join(workDir, spec.archive)
		if err := writeDockerArchive(archivePath, spec.image, binaryPath, spec.entrypoint); err != nil {
			return nil, fmt.Errorf("write %s image archive: %w", spec.image, err)
		}
		fixtures = append(fixtures, nodeImageFixture{
			Image:  spec.image,
			Source: archivePath,
		})
	}
	return fixtures, nil
}

func writeDockerArchive(path, image, binaryPath, entrypoint string) error {
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return err
	}
	var layer bytes.Buffer
	layerWriter := tar.NewWriter(&layer)
	if err := layerWriter.WriteHeader(&tar.Header{
		Name:    strings.TrimPrefix(entrypoint, "/"),
		Mode:    0o755,
		Size:    int64(len(binary)),
		ModTime: time.Unix(0, 0),
	}); err != nil {
		return err
	}
	if _, err := layerWriter.Write(binary); err != nil {
		return err
	}
	if err := layerWriter.Close(); err != nil {
		return err
	}
	diffID := sha256.Sum256(layer.Bytes())
	config := map[string]any{
		"architecture": "amd64",
		"created":      "1970-01-01T00:00:00Z",
		"os":           "linux",
		"config": map[string]any{
			"Entrypoint": []string{entrypoint},
		},
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{"sha256:" + fmt.Sprintf("%x", diffID[:])},
		},
	}
	configData, err := json.Marshal(config)
	if err != nil {
		return err
	}
	manifestData, err := json.Marshal([]map[string]any{{
		"Config":   "config.json",
		"RepoTags": []string{image},
		"Layers":   []string{"layer.tar"},
	}})
	if err != nil {
		return err
	}
	var archive bytes.Buffer
	archiveWriter := tar.NewWriter(&archive)
	for _, entry := range []struct {
		name string
		data []byte
		mode int64
	}{
		{name: "config.json", data: configData, mode: 0o644},
		{name: "manifest.json", data: manifestData, mode: 0o644},
		{name: "layer.tar", data: layer.Bytes(), mode: 0o644},
	} {
		if err := archiveWriter.WriteHeader(&tar.Header{
			Name:    entry.name,
			Mode:    entry.mode,
			Size:    int64(len(entry.data)),
			ModTime: time.Unix(0, 0),
		}); err != nil {
			return err
		}
		if _, err := archiveWriter.Write(entry.data); err != nil {
			return err
		}
	}
	if err := archiveWriter.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(path, archive.Bytes(), 0o644); err != nil {
		return err
	}
	return nil
}

func stageBootstrapFixtureInputs(manifestDir string, inputs bootstrapFixtureInputs) (bootstrapFixtureInputs, error) {
	if inputs.empty() {
		return inputs, nil
	}
	dir := filepath.Join(manifestDir, "bootstrap-fixtures")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return bootstrapFixtureInputs{}, err
	}
	staged := bootstrapFixtureInputs{
		PreWaits: append([]string(nil), inputs.PreWaits...),
		Waits:    append([]string(nil), inputs.Waits...),
	}
	for index, source := range inputs.Manifests {
		source = strings.TrimSpace(source)
		if source == "" {
			continue
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return bootstrapFixtureInputs{}, fmt.Errorf("read bootstrap fixture %s: %w", source, err)
		}
		target := filepath.Join(dir, fmt.Sprintf("%02d-%s", index+1, filepath.Base(source)))
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return bootstrapFixtureInputs{}, fmt.Errorf("stage bootstrap fixture %s: %w", source, err)
		}
		staged.Manifests = append(staged.Manifests, target)
	}
	return staged, nil
}

func bootstrapManifestInputsFromEnv() []string {
	return compactStrings(append(splitPathList(os.Getenv("KATL_BOOTSTRAP_MANIFESTS")), os.Getenv("KATL_BOOTSTRAP_MANIFEST")))
}

func bootstrapWaitInputsFromEnv() []string {
	return compactStrings(append(splitLines(os.Getenv("KATL_BOOTSTRAP_WAITS")), os.Getenv("KATL_BOOTSTRAP_WAIT")))
}

func appendBootstrapFixtureArgs(args []string, inputs bootstrapFixtureInputs) []string {
	for _, manifest := range inputs.Manifests {
		args = append(args, "--bootstrap-manifest", manifest)
	}
	for _, wait := range inputs.PreWaits {
		args = append(args, "--bootstrap-pre-wait", wait)
	}
	for _, wait := range inputs.Waits {
		args = append(args, "--bootstrap-wait", wait)
	}
	return args
}

func splitPathList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return filepath.SplitList(value)
}

func splitLines(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.Split(value, "\n")
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func writeTwoNodeArtifactManifest(path string, manifest twoNodeArtifactManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func twoNodeFixtureInputs(cpDisk, cpFormat, workerDisk, workerFormat, cpESP, workerESP, cpFixture, workerFixture, cpMetadata, workerMetadata string) map[string]nodeFixtureInput {
	return map[string]nodeFixtureInput{
		"cp-1":     fixtureInput(cpDisk, cpFormat, cpESP, cpFixture, cpMetadata),
		"worker-1": fixtureInput(workerDisk, workerFormat, workerESP, workerFixture, workerMetadata),
	}
}

func fixtureInput(disk, format, esp, fixture, metadata string) nodeFixtureInput {
	return nodeFixtureInput{
		Disk:            disk,
		DiskFormat:      firstString(format, string(vmtest.DiskRaw)),
		ESPArtifacts:    esp,
		FixtureManifest: fixture,
		NodeMetadata:    metadata,
	}
}

func assertBootstrapPhases(t *testing.T, output string) {
	t.Helper()
	for _, want := range []string{
		"katlctl cluster bootstrap init-node=cp-1",
		"phase=kubeadm-init node=cp-1 status=passed",
		"phase=worker-join node=worker-1 status=passed",
		"phase=worker-ready node=worker-1 status=passed",
		"phase=user-bootstrap status=passed",
		"phase=kubeconfig status=passed",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("katlctl output missing %q:\n%s", want, output)
		}
	}
	for _, forbidden := range []string{
		"phase=kubeadm-init node=worker-1",
		"phase=worker-join node=cp-1",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("katlctl output contains forbidden phase %q:\n%s", forbidden, output)
		}
	}
}

func assertOperationBackedBootstrapPhases(t *testing.T, output string) {
	t.Helper()
	for _, want := range []string{
		"katlctl cluster bootstrap init-node=cp-1",
		"phase=bootstrap-init node=cp-1 status=passed",
		"phase=worker-join node=worker-1 status=passed",
		"phase=user-bootstrap status=passed",
		"phase=kubeconfig status=passed",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("katlctl output missing %q:\n%s", want, output)
		}
	}
	for _, forbidden := range []string{
		"--vmtest-transcript-dir",
		"phase=bootstrap-init node=worker-1",
		"phase=worker-join node=cp-1",
		"phase=control-plane-join",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("katlctl output contains forbidden operation-backed bootstrap text %q:\n%s", forbidden, output)
		}
	}
}

func bootstrapCommandError(err error, output string) error {
	if err != nil {
		return err
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "phase=") && strings.Contains(line, " status=failed") {
			return fmt.Errorf("katlctl reported failed bootstrap phase: %s", line)
		}
	}
	return nil
}

func readNodeFileWithRetry(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, path string, maxBytes uint32, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		data, err := readNodeFile(ctx, node, path, maxBytes)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func readNodeFile(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, path string, maxBytes uint32) ([]byte, error) {
	result, err := retryDirectAgentOp(ctx, node, 10*time.Second, func(opCtx context.Context, client *vmtest.AgentClient) (*vmtestpb.FileResult, error) {
		return client.ReadFile(opCtx, &vmtestpb.ReadFileRequest{
			Path:      path,
			MaxBytes:  maxBytes,
			Sensitive: true,
		})
	})
	if err != nil {
		return nil, err
	}
	if result.Truncated {
		return nil, fmt.Errorf("guest file %s exceeded %d bytes", path, maxBytes)
	}
	return result.Content, nil
}

func writeNodeFile(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, path string, content []byte, mode uint32, sensitive bool) error {
	_, err := retryDirectAgentOp(ctx, node, 10*time.Second, func(opCtx context.Context, client *vmtest.AgentClient) (*vmtestpb.WriteFileResult, error) {
		return client.WriteFile(opCtx, &vmtestpb.WriteFileRequest{
			Path:      path,
			Content:   content,
			Mode:      mode,
			Sensitive: sensitive,
		})
	})
	return err
}

func retryDirectAgentOp[T any](ctx context.Context, node vmtest.RunningInstalledRuntimeNode, timeout time.Duration, op func(context.Context, *vmtest.AgentClient) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		opCtx, cancel := context.WithTimeout(ctx, timeout)
		client, err := vmtest.DialAgent(opCtx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
		if err != nil {
			cancel()
			lastErr = err
		} else {
			result, err := op(opCtx, client)
			_ = client.Close()
			cancel()
			if err == nil {
				return result, nil
			}
			lastErr = err
		}
		if attempt == 2 || !transientAgentTransportError(lastErr) {
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

func writeNodeFileChunked(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, path string, content []byte, mode uint32) error {
	const chunkSize = 512 << 10
	if result, err := runNodeCommandWithRetry(ctx, node, []string{"install", "-d", "-m", "0755", filepath.Dir(path)}, 16<<10); err != nil {
		return fmt.Errorf("create parent: %w", err)
	} else if result.ExitStatus != 0 {
		return fmt.Errorf("create parent: %w", commandErrorDetail(result))
	}
	if result, err := runNodeCommandWithRetry(ctx, node, []string{"dd", "if=/dev/null", "of=" + path, "bs=1", "count=0"}, 16<<10); err != nil {
		return fmt.Errorf("create target: %w", err)
	} else if result.ExitStatus != 0 {
		return fmt.Errorf("create target: %w", commandErrorDetail(result))
	}
	for offset := 0; offset < len(content); offset += chunkSize {
		end := offset + chunkSize
		if end > len(content) {
			end = len(content)
		}
		part := fmt.Sprintf("%s.part-%06d", path, offset/chunkSize)
		if err := writeNodeFile(ctx, node, part, content[offset:end], 0o644, true); err != nil {
			return fmt.Errorf("write part %d: %w", offset/chunkSize, err)
		}
		argv := []string{"dd", "if=" + part, "of=" + path, fmt.Sprintf("bs=%d", chunkSize), "seek=" + strconv.Itoa(offset/chunkSize), "conv=notrunc"}
		if result, err := runNodeCommandWithRetry(ctx, node, argv, 16<<10); err != nil {
			return fmt.Errorf("append part %d: %w", offset/chunkSize, err)
		} else if result.ExitStatus != 0 {
			return fmt.Errorf("append part %d: %w", offset/chunkSize, commandErrorDetail(result))
		}
	}
	if result, err := runNodeCommandWithRetry(ctx, node, []string{"chmod", fmt.Sprintf("%04o", mode), path}, 16<<10); err != nil {
		return fmt.Errorf("chmod target: %w", err)
	} else if result.ExitStatus != 0 {
		return fmt.Errorf("chmod target: %w", commandErrorDetail(result))
	}
	return nil
}

func runNodeCommandWithRetry(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, argv []string, stdoutLimit uint32) (*vmtestpb.CommandResult, error) {
	return retryDirectAgentOp(ctx, node, 30*time.Second, func(opCtx context.Context, client *vmtest.AgentClient) (*vmtestpb.CommandResult, error) {
		return client.RunCommand(opCtx, &vmtestpb.RunCommandRequest{
			Argv:        argv,
			StdoutLimit: stdoutLimit,
			StderrLimit: 32 << 10,
		})
	})
}

func runNodeCommand(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, argv []string, stdoutLimit uint32) (*vmtestpb.CommandResult, error) {
	opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := vmtest.DialAgent(opCtx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.RunCommand(opCtx, &vmtestpb.RunCommandRequest{
		Argv:        argv,
		StdoutLimit: stdoutLimit,
		StderrLimit: 32 << 10,
	})
}

func waitForKatlcAgentTCP(ctx context.Context, nodeName, address string, timeout time.Duration) error {
	if strings.TrimSpace(address) == "" {
		return fmt.Errorf("%s has no node address", nodeName)
	}
	deadline := time.Now().Add(timeout)
	target := net.JoinHostPort(address, "9443")
	var lastErr error
	for {
		dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", target)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not accept TCP connections on %s: %w", nodeName, target, lastErr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func liveNodeIPv4Address(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, fallback string) (string, error) {
	result, err := runNodeCommand(ctx, node, []string{"ip", "-4", "-o", "addr", "show", "scope", "global"}, 16<<10)
	if err != nil {
		if strings.TrimSpace(fallback) != "" {
			return strings.TrimSpace(fallback), nil
		}
		return "", err
	}
	if result.ExitStatus != 0 {
		if strings.TrimSpace(fallback) != "" {
			return strings.TrimSpace(fallback), nil
		}
		return "", commandErrorDetail(result)
	}
	address, err := parseIPAddrShowIPv4(result.Stdout)
	if err != nil {
		if strings.TrimSpace(fallback) != "" {
			return strings.TrimSpace(fallback), nil
		}
		return "", err
	}
	return address, nil
}

func parseIPAddrShowIPv4(output []byte) (string, error) {
	for _, field := range strings.Fields(string(output)) {
		if !strings.Contains(field, "/") {
			continue
		}
		address, _, ok := strings.Cut(field, "/")
		if !ok || net.ParseIP(address).To4() == nil {
			continue
		}
		return address, nil
	}
	return "", fmt.Errorf("no global IPv4 address found")
}

func commandErrorDetail(result *vmtestpb.CommandResult) error {
	if result == nil {
		return errors.New("command failed without a result")
	}
	return fmt.Errorf("exit=%d stdout=%q stderr=%q", result.GetExitStatus(), result.GetStdout(), result.GetStderr())
}

func assertGeneration0Selection(t *testing.T, data []byte) {
	t.Helper()
	selection, err := decodeBootSelection(data)
	if err != nil {
		t.Fatalf("decode initial boot selection: %v", err)
	}
	if selection.DefaultGenerationID != "0" ||
		selection.TargetBootGenerationID != "" ||
		selection.TrialGenerationID != "" ||
		selection.PendingHealthValidation {
		t.Fatalf("initial boot selection = %#v, want generation 0 persistent default with no pending transaction", selection)
	}
	if selection.PersistentDefaultPromotion != "" && selection.PersistentDefaultPromotion != generation.DefaultPromotionDone {
		t.Fatalf("initial default promotion = %q, want empty or promoted", selection.PersistentDefaultPromotion)
	}
}

func collectOperationEvidence(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir, expectedKind string) (string, operation.OperationRecord, error) {
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return "", operation.OperationRecord{}, err
	}
	result, err := runNodeCommand(ctx, node, []string{"find", "/var/lib/katl/operations", "-name", "record.json", "-print"}, 64<<10)
	if err != nil {
		return "", operation.OperationRecord{}, err
	}
	if result.ExitStatus != 0 {
		return "", operation.OperationRecord{}, fmt.Errorf("find operation records exited %d: %s", result.ExitStatus, strings.TrimSpace(string(result.Stderr)))
	}
	var selectedPath string
	var selected operation.OperationRecord
	var found []string
	for _, path := range strings.Fields(string(result.Stdout)) {
		data, err := readNodeFileWithRetry(ctx, node, path, 2<<20, 30*time.Second)
		if err != nil {
			return "", operation.OperationRecord{}, err
		}
		record, err := decodeOperationRecord(data)
		if err != nil {
			return "", operation.OperationRecord{}, fmt.Errorf("decode %s: %w", path, err)
		}
		found = append(found, record.OperationID+"="+record.OperationKind)
		if err := os.WriteFile(filepath.Join(evidenceDir, "operation-record-"+firstString(record.OperationID, filepath.Base(filepath.Dir(path)))+".json"), data, 0o600); err != nil {
			return "", operation.OperationRecord{}, err
		}
		if record.OperationKind != expectedKind {
			continue
		}
		if selectedPath != "" {
			return "", operation.OperationRecord{}, fmt.Errorf("multiple %s records found: %s and %s", expectedKind, selectedPath, path)
		}
		selectedPath = path
		selected = record
	}
	if selectedPath == "" {
		return "", operation.OperationRecord{}, fmt.Errorf("%s operation record not found; found records: %s", expectedKind, strings.Join(found, ", "))
	}
	hostRecord := filepath.Join(evidenceDir, expectedKind+"-record.json")
	data, err := readNodeFileWithRetry(ctx, node, selectedPath, 2<<20, 30*time.Second)
	if err != nil {
		return "", operation.OperationRecord{}, err
	}
	if err := os.WriteFile(hostRecord, data, 0o600); err != nil {
		return "", operation.OperationRecord{}, err
	}
	selected, err = decodeOperationRecord(data)
	if err != nil {
		return "", operation.OperationRecord{}, fmt.Errorf("decode selected operation record: %w", err)
	}
	if err := collectOperationJournalEvidence(ctx, node, evidenceDir, selected.OperationID); err != nil {
		return "", operation.OperationRecord{}, err
	}
	if err := collectOperationDiagnosticArtifacts(ctx, node, evidenceDir, selected); err != nil {
		return "", operation.OperationRecord{}, err
	}
	return hostRecord, selected, nil
}

func collectOperationBackedFailureEvidence(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir, expectedKind string) {
	hostRecord, record, err := collectOperationEvidence(ctx, node, evidenceDir, expectedKind)
	if err != nil {
		_ = os.WriteFile(filepath.Join(evidenceDir, "operation-evidence-error.txt"), []byte(err.Error()+"\n"), 0o644)
		return
	}
	if record.CandidateGenerationID != "" {
		_, _, _ = collectGenerationEvidence(ctx, node, evidenceDir, record.CandidateGenerationID)
	}
	_, _, _ = collectBootSelectionEvidence(ctx, node, evidenceDir)
	_ = os.WriteFile(filepath.Join(evidenceDir, "failure-evidence-manifest.txt"), []byte(hostRecord+"\n"), 0o644)
}

func decodeOperationRecord(data []byte) (operation.OperationRecord, error) {
	if envelope, err := persistedrecord.DecodeEnvelope(data); err == nil {
		snapshot, err := persistedrecord.DecodePayload[operation.Snapshot](envelope)
		if err != nil {
			return operation.OperationRecord{}, err
		}
		return snapshot.Record, nil
	}
	var record operation.OperationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return operation.OperationRecord{}, err
	}
	if record.Kind == operation.RecordKind {
		return record, nil
	}
	var snapshot struct {
		Record operation.OperationRecord `json:"record"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return operation.OperationRecord{}, err
	}
	return snapshot.Record, nil
}

func collectOperationJournalEvidence(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir string, operationID string) error {
	journalDir := "/var/lib/katl/operations/" + operationID + "/journal"
	result, err := runNodeCommand(ctx, node, []string{"find", journalDir, "-type", "f", "-name", "*.json", "-print"}, 256<<10)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		return fmt.Errorf("find operation journal exited %d: %s", result.ExitStatus, strings.TrimSpace(string(result.Stderr)))
	}
	paths := strings.Fields(string(result.Stdout))
	if len(paths) == 0 {
		return fmt.Errorf("operation %s journal has no events", operationID)
	}
	manifestPath := filepath.Join(evidenceDir, "operation-journal-files.txt")
	if err := os.WriteFile(manifestPath, []byte(strings.Join(paths, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	hostDir := filepath.Join(evidenceDir, "operation-journal")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return err
	}
	for _, guestPath := range paths {
		data, err := readNodeFileWithRetry(ctx, node, guestPath, 2<<20, 30*time.Second)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(hostDir, filepath.Base(guestPath)), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func collectOperationDiagnosticArtifacts(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir string, record operation.OperationRecord) error {
	if len(record.DiagnosticArtifacts) == 0 {
		return nil
	}
	hostDir := filepath.Join(evidenceDir, "operation-diagnostics")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return err
	}
	for _, artifact := range record.DiagnosticArtifacts {
		if !artifact.Redacted {
			return fmt.Errorf("diagnostic artifact %s is not marked redacted", artifact.ArtifactID)
		}
		if strings.TrimSpace(artifact.ArtifactID) == "" || strings.TrimSpace(artifact.Path) == "" {
			return fmt.Errorf("diagnostic artifact is missing id or path: %#v", artifact)
		}
		guestPath := "/var/lib/katl/operations/" + record.OperationID + "/" + filepath.ToSlash(artifact.Path)
		data, err := readNodeFileWithRetry(ctx, node, guestPath, 2<<20, 30*time.Second)
		if err != nil {
			return err
		}
		name := artifact.ArtifactID + filepath.Ext(artifact.Path)
		if err := os.WriteFile(filepath.Join(hostDir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func assertOperationBackedInitRecord(t *testing.T, record operation.OperationRecord, endpoint string) {
	t.Helper()
	if record.OperationKind != "bootstrap-init" ||
		record.ExpectedCurrentGenerationID != "0" ||
		record.CandidateGenerationID == "" ||
		record.CandidateGenerationID == "0" {
		t.Fatalf("operation identity = kind %q current %q candidate %q", record.OperationKind, record.ExpectedCurrentGenerationID, record.CandidateGenerationID)
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded || record.Phase != operation.HostBookkeepingCompletionPhase {
		t.Fatalf("operation terminal state = terminal %v result %q phase %q failure %q", record.Terminal, record.Result, record.Phase, record.FailureReason)
	}
	if record.ActivationState != operation.ActivationStateActiveLive ||
		record.GenerationCommitState != operation.GenerationCommitCommitted ||
		record.PostKubeadmHealthState != operation.PostKubeadmHealthPassed ||
		!record.BootHealthPending {
		t.Fatalf("operation lifecycle = activation %q commit %q health %q pending %v", record.ActivationState, record.GenerationCommitState, record.PostKubeadmHealthState, record.BootHealthPending)
	}
	if record.BootstrapRequest == nil ||
		record.BootstrapRequest.InventoryNodeName != "cp-1" ||
		record.BootstrapRequest.SystemRole != "control-plane" ||
		record.BootstrapRequest.ControlPlaneEndpoint != endpoint {
		t.Fatalf("bootstrap request = %#v, want cp-1 control-plane endpoint %s", record.BootstrapRequest, endpoint)
	}
	if record.ExecutorPlan == nil ||
		record.ExecutorPlan.Phase != "kubeadm-init" ||
		!stringSliceEqual(record.ExecutorPlan.Argv, []string{"/usr/bin/kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}) {
		t.Fatalf("executor plan = %#v, want kubeadm init through katlc agent executor", record.ExecutorPlan)
	}
	if !record.ExternalMutationStarted || !record.MutatingToolRan || len(record.PreExecMutationMarkers) == 0 {
		t.Fatalf("mutation tracking = external %v tool %v markers %d", record.ExternalMutationStarted, record.MutatingToolRan, len(record.PreExecMutationMarkers))
	}
	if !containsAllStrings(record.ResourceLocks, "generation-state.lock", "kubeadm-state.lock") {
		t.Fatalf("resource locks = %#v", record.ResourceLocks)
	}
	if !containsAllStrings(record.MutationScopes, "etc-kubernetes", "kubelet-state", "etcd-state", "cluster-objects") {
		t.Fatalf("mutation scopes = %#v", record.MutationScopes)
	}
	if !containsAllStrings(record.CompletedPhases, "accepted", "prepare-bootstrap-runtime", "bootstrap-runtime-ready", "kubeadm-init", "post-kubeadm-health", operation.HostBookkeepingCompletionPhase) {
		t.Fatalf("completed phases = %#v", record.CompletedPhases)
	}
	if !phaseOrder(record.CompletedPhases, "accepted", "prepare-bootstrap-runtime", "bootstrap-runtime-ready", "kubeadm-init", "post-kubeadm-health", operation.HostBookkeepingCompletionPhase) {
		t.Fatalf("completed phases out of order = %#v", record.CompletedPhases)
	}
	if !hasSuccessfulInvocation(record.Invocations, "/usr/bin/kubeadm", "init") {
		t.Fatalf("invocations missing successful kubeadm init: %#v", record.Invocations)
	}
	if len(record.DiagnosticArtifacts) == 0 {
		t.Fatalf("operation diagnostic artifacts are missing")
	}
	for _, artifact := range record.DiagnosticArtifacts {
		if !artifact.Redacted {
			t.Fatalf("diagnostic artifact %s is not marked redacted", artifact.ArtifactID)
		}
	}
}

func assertOperationBackedWorkerRecord(t *testing.T, record operation.OperationRecord, endpoint string) {
	t.Helper()
	if record.OperationKind != "bootstrap-join-worker" ||
		record.ExpectedCurrentGenerationID != "0" ||
		record.CandidateGenerationID == "" ||
		record.CandidateGenerationID == "0" {
		t.Fatalf("worker operation identity = kind %q current %q candidate %q", record.OperationKind, record.ExpectedCurrentGenerationID, record.CandidateGenerationID)
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded || record.Phase != operation.HostBookkeepingCompletionPhase {
		t.Fatalf("worker operation terminal state = terminal %v result %q phase %q failure %q", record.Terminal, record.Result, record.Phase, record.FailureReason)
	}
	if record.ActivationState != operation.ActivationStateActiveLive ||
		record.GenerationCommitState != operation.GenerationCommitCommitted ||
		record.PostKubeadmHealthState != operation.PostKubeadmHealthPassed ||
		!record.BootHealthPending {
		t.Fatalf("worker operation lifecycle = activation %q commit %q health %q pending %v", record.ActivationState, record.GenerationCommitState, record.PostKubeadmHealthState, record.BootHealthPending)
	}
	if record.BootstrapRequest == nil ||
		record.BootstrapRequest.InventoryNodeName != "worker-1" ||
		record.BootstrapRequest.SystemRole != "worker" ||
		record.BootstrapRequest.ControlPlaneEndpoint != endpoint ||
		record.BootstrapRequest.JoinMaterialRef == "" ||
		record.BootstrapRequest.JoinMaterialDigest == "" ||
		record.BootstrapRequest.JoinMaterialExpiresAt == "" ||
		!strings.HasPrefix(record.BootstrapRequest.TemporaryJoinConfigPath, "/run/katl/bootstrap-join/") {
		t.Fatalf("worker bootstrap request = %#v, want worker join material evidence for endpoint %s", record.BootstrapRequest, endpoint)
	}
	if record.ExecutorPlan == nil ||
		record.ExecutorPlan.Phase != "kubeadm-join-worker" ||
		!stringSliceEqual(record.ExecutorPlan.Argv, []string{"/usr/bin/kubeadm", "join", "--config", record.BootstrapRequest.TemporaryJoinConfigPath}) {
		t.Fatalf("worker executor plan = %#v, want kubeadm join through temporary config", record.ExecutorPlan)
	}
	if !record.ExternalMutationStarted || !record.MutatingToolRan || len(record.PreExecMutationMarkers) == 0 {
		t.Fatalf("worker mutation tracking = external %v tool %v markers %d", record.ExternalMutationStarted, record.MutatingToolRan, len(record.PreExecMutationMarkers))
	}
	if !containsAllStrings(record.ResourceLocks, "generation-state.lock", "kubeadm-state.lock") {
		t.Fatalf("worker resource locks = %#v", record.ResourceLocks)
	}
	if !containsAllStrings(record.MutationScopes, "etc-kubernetes", "kubelet-state", "etcd-state", "cluster-objects") {
		t.Fatalf("worker mutation scopes = %#v", record.MutationScopes)
	}
	if !containsAllStrings(record.CompletedPhases, "accepted", "prepare-bootstrap-runtime", "bootstrap-runtime-ready", "kubeadm-join-worker", "post-kubeadm-health", operation.HostBookkeepingCompletionPhase) {
		t.Fatalf("worker completed phases = %#v", record.CompletedPhases)
	}
	if !phaseOrder(record.CompletedPhases, "accepted", "prepare-bootstrap-runtime", "bootstrap-runtime-ready", "kubeadm-join-worker", "post-kubeadm-health", operation.HostBookkeepingCompletionPhase) {
		t.Fatalf("worker completed phases out of order = %#v", record.CompletedPhases)
	}
	if !hasSuccessfulInvocation(record.Invocations, "/usr/bin/kubeadm", "join") {
		t.Fatalf("worker invocations missing successful kubeadm join: %#v", record.Invocations)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal worker operation record: %v", err)
	}
	if strings.Contains(string(data), "--token") || strings.Contains(string(data), "discovery-token-ca-cert-hash") {
		t.Fatalf("worker operation record contains raw kubeadm join material")
	}
	if len(record.DiagnosticArtifacts) == 0 {
		t.Fatalf("worker operation diagnostic artifacts are missing")
	}
	for _, artifact := range record.DiagnosticArtifacts {
		if !artifact.Redacted {
			t.Fatalf("worker diagnostic artifact %s is not marked redacted", artifact.ArtifactID)
		}
	}
}

func assertOperationJournalOrder(t *testing.T, evidenceDir string, events ...string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(evidenceDir, "operation-journal"))
	if err != nil {
		t.Fatalf("read operation journal evidence: %v", err)
	}
	positions := make(map[string]int)
	for i, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		for _, event := range events {
			if strings.HasSuffix(name, "."+event+".json") {
				positions[event] = i
			}
		}
	}
	last := -1
	for _, event := range events {
		position, ok := positions[event]
		if !ok {
			t.Fatalf("operation journal missing event %q in %v", event, positions)
		}
		if position <= last {
			t.Fatalf("operation journal event %q is out of order: %v", event, positions)
		}
		last = position
	}
}

func phaseOrder(phases []string, wants ...string) bool {
	last := -1
	for _, want := range wants {
		found := -1
		for i, phase := range phases {
			if phase == want {
				found = i
				break
			}
		}
		if found <= last {
			return false
		}
		last = found
	}
	return true
}

type operationBackedGenerationRecord struct {
	Spec   generation.GenerationSpec   `json:"spec"`
	Status generation.GenerationStatus `json:"status"`
}

func collectGenerationEvidence(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir, generationID string) (string, operationBackedGenerationRecord, error) {
	specData, err := readNodeFileWithRetry(ctx, node, "/var/lib/katl/generations/"+generationID+"/spec.json", 2<<20, 30*time.Second)
	if err != nil {
		return "", operationBackedGenerationRecord{}, err
	}
	statusData, err := readNodeFileWithRetry(ctx, node, "/var/lib/katl/generations/"+generationID+"/status.json", 2<<20, 30*time.Second)
	if err != nil {
		return "", operationBackedGenerationRecord{}, err
	}
	var record operationBackedGenerationRecord
	record.Spec, err = decodePersistedOrLegacy[generation.GenerationSpec](specData)
	if err != nil {
		return "", operationBackedGenerationRecord{}, fmt.Errorf("decode generation spec: %w", err)
	}
	record.Status, err = decodePersistedOrLegacy[generation.GenerationStatus](statusData)
	if err != nil {
		return "", operationBackedGenerationRecord{}, fmt.Errorf("decode generation status: %w", err)
	}
	hostPath := filepath.Join(evidenceDir, "generation-"+generationID+".json")
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", operationBackedGenerationRecord{}, err
	}
	if err := os.WriteFile(hostPath, append(data, '\n'), 0o600); err != nil {
		return "", operationBackedGenerationRecord{}, err
	}
	return hostPath, record, nil
}

func decodePersistedOrLegacy[T any](data []byte) (T, error) {
	var zero T
	envelope, err := persistedrecord.DecodeEnvelope(data)
	if err == nil {
		return persistedrecord.DecodePayload[T](envelope)
	}
	var record T
	if legacyErr := json.Unmarshal(data, &record); legacyErr != nil {
		return zero, errors.Join(err, legacyErr)
	}
	return record, nil
}

func assertCommittedGeneration(t *testing.T, record operationBackedGenerationRecord, generationID string) {
	t.Helper()
	if record.Spec.GenerationID != generationID || record.Status.GenerationID != generationID {
		t.Fatalf("generation IDs = spec %q status %q want %q", record.Spec.GenerationID, record.Status.GenerationID, generationID)
	}
	if record.Status.CommitState != generation.CommitStateCommitted ||
		record.Status.BootState != generation.BootStateTrying ||
		record.Status.HealthState != generation.HealthStateUnknown ||
		record.Status.CommittedAt == nil ||
		record.Status.CommittedByOperation == "" {
		t.Fatalf("generation status = %#v, want committed trying with deferred boot health", record.Status)
	}
	if record.Spec.PreviousGenerationID != "0" {
		t.Fatalf("generation previous ID = %q, want 0", record.Spec.PreviousGenerationID)
	}
	if len(record.Spec.Sysexts) == 0 || len(record.Spec.Confexts) == 0 {
		t.Fatalf("generation spec missing sysext/confext refs: sysexts=%d confexts=%d", len(record.Spec.Sysexts), len(record.Spec.Confexts))
	}
	if record.Spec.Boot.LoaderEntryPath != "loader/entries/katl-"+generationID+".conf" {
		t.Fatalf("loader entry = %q, want generation %s loader entry", record.Spec.Boot.LoaderEntryPath, generationID)
	}
}

func collectBootSelectionEvidence(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir string) (string, generation.BootSelectionRecord, error) {
	data, err := readNodeFileWithRetry(ctx, node, "/var/lib/katl/boot/selection.json", 128<<10, 30*time.Second)
	if err != nil {
		return "", generation.BootSelectionRecord{}, err
	}
	selection, err := decodeBootSelection(data)
	if err != nil {
		return "", generation.BootSelectionRecord{}, fmt.Errorf("decode boot selection: %w", err)
	}
	hostPath := filepath.Join(evidenceDir, "boot-selection-after.json")
	if err := os.WriteFile(hostPath, data, 0o600); err != nil {
		return "", generation.BootSelectionRecord{}, err
	}
	return hostPath, selection, nil
}

func decodeBootSelection(data []byte) (generation.BootSelectionRecord, error) {
	envelope, err := persistedrecord.DecodeEnvelope(data)
	if err == nil {
		return persistedrecord.DecodePayload[generation.BootSelectionRecord](envelope)
	}
	var selection generation.BootSelectionRecord
	if legacyErr := json.Unmarshal(data, &selection); legacyErr != nil {
		return generation.BootSelectionRecord{}, errors.Join(err, legacyErr)
	}
	return selection, nil
}

type nodeLocalStatusEvidence struct {
	Node    string                         `json:"node"`
	Results map[string]nodeCommandEvidence `json:"results"`
	Files   map[string]nodeFileEvidence    `json:"files,omitempty"`
}

type nodeCommandEvidence struct {
	Argv       []string `json:"argv"`
	ExitStatus int32    `json:"exitStatus"`
	Stdout     string   `json:"stdout,omitempty"`
	Stderr     string   `json:"stderr,omitempty"`
}

type nodeFileEvidence struct {
	Path      string `json:"path"`
	SizeBytes int    `json:"sizeBytes,omitempty"`
	Error     string `json:"error,omitempty"`
}

func collectNodeLocalStatusEvidence(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, evidenceDir string) (string, error) {
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return "", err
	}
	commands := map[string][]string{
		"katlcAgent":       {"systemctl", "is-active", "katlc-agent.service"},
		"kubelet":          {"systemctl", "is-active", "kubelet.service"},
		"kubeadmReady":     {"systemctl", "is-active", "katl-kubeadm-ready.target"},
		"operationRecords": {"find", "/var/lib/katl/operations", "-maxdepth", "3", "-type", "f", "-name", "record.json", "-print"},
		"operationLocks":   {"find", "/var/lib/katl/operations", "-maxdepth", "2", "-type", "f", "-name", "*.lock", "-print"},
	}
	evidence := nodeLocalStatusEvidence{
		Node:    node.Name,
		Results: make(map[string]nodeCommandEvidence, len(commands)),
		Files:   map[string]nodeFileEvidence{},
	}
	for name, path := range map[string]string{
		"machineID":         "/etc/machine-id",
		"kubeletKubeconfig": "/etc/kubernetes/kubelet.conf",
	} {
		data, err := readNodeFile(ctx, node, path, 256<<10)
		file := nodeFileEvidence{Path: path}
		if err != nil {
			file.Error = err.Error()
		} else {
			file.SizeBytes = len(data)
		}
		evidence.Files[name] = file
	}
	for name, argv := range commands {
		result, err := runNodeCommand(ctx, node, argv, 128<<10)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		evidence.Results[name] = nodeCommandEvidence{
			Argv:       argv,
			ExitStatus: result.ExitStatus,
			Stdout:     string(result.Stdout),
			Stderr:     string(result.Stderr),
		}
	}
	hostPath := filepath.Join(evidenceDir, "node-local-status.json")
	return hostPath, writeTwoNodeDiagnosticJSON(hostPath, evidence)
}

func collectNodeLocalStatusFailureEvidence(ctx context.Context, evidenceDir string, nodes ...vmtest.RunningInstalledRuntimeNode) map[string]string {
	paths := map[string]string{}
	for _, node := range nodes {
		nodeEvidenceDir := filepath.Join(evidenceDir, node.Name)
		path, err := collectNodeLocalStatusEvidence(ctx, node, nodeEvidenceDir)
		if err != nil {
			errorPath := filepath.Join(nodeEvidenceDir, "node-local-status-error.txt")
			_ = os.MkdirAll(filepath.Dir(errorPath), 0o755)
			_ = os.WriteFile(errorPath, []byte(err.Error()+"\n"), 0o644)
			paths[node.Name] = errorPath
			continue
		}
		paths[node.Name] = path
	}
	if len(paths) == 0 {
		return nil
	}
	return paths
}

func assertPostBootstrapSelection(t *testing.T, selection generation.BootSelectionRecord, candidate string) {
	t.Helper()
	if selection.DefaultGenerationID != "0" ||
		selection.TargetBootGenerationID != candidate ||
		selection.TrialGenerationID != candidate ||
		selection.PreviousKnownGoodGenerationID != "0" ||
		selection.Generation0FallbackID != "0" ||
		!selection.PendingHealthValidation ||
		selection.PersistentDefaultPromotion != generation.DefaultPromotionPending {
		t.Fatalf("post-bootstrap selection = %#v, want generation %s armed for boot health with gen0 fallback", selection, candidate)
	}
	if selection.PendingTransactionID == "" ||
		selection.TargetBootEntry != "loader/entries/katl-"+candidate+".conf" ||
		selection.TrialBootEntry != selection.TargetBootEntry {
		t.Fatalf("post-bootstrap boot transaction = %#v", selection)
	}
}

func containsAllStrings(values []string, wants ...string) bool {
	for _, want := range wants {
		found := false
		for _, value := range values {
			if value == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func stringSliceEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func hasSuccessfulInvocation(invocations []operation.InvocationRecord, argv ...string) bool {
	for _, invocation := range invocations {
		if invocation.ExitStatus != 0 {
			continue
		}
		if len(invocation.ChildProcess) < len(argv) {
			continue
		}
		matched := true
		for i, want := range argv {
			if invocation.ChildProcess[i] != want {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func assertKubeconfigOutput(t *testing.T, kubeconfigPath, metadataPath, server string) {
	t.Helper()
	var metadata kubeconfigMetadata
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read kubeconfig metadata: %v", err)
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("decode kubeconfig metadata: %v", err)
	}
	if metadata.Path != kubeconfigPath || !metadata.Exists || metadata.SizeBytes == 0 || metadata.Mode != "0600" || metadata.StatError != "" {
		t.Fatalf("kubeconfig metadata = %#v", metadata)
	}
	kubeconfig, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}
	if !strings.Contains(string(kubeconfig), "server: "+server) {
		t.Fatalf("kubeconfig does not target %s", server)
	}
}

func verifyTwoNodeBootstrapTranscripts(transcriptDir string) error {
	for _, node := range []string{"cp-1", "worker-1"} {
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
		if node == "worker-1" {
			if !writeFile {
				return fmt.Errorf("%s transcript has no WriteFile entry", node)
			}
			if !sensitiveWriteFile {
				return fmt.Errorf("%s transcript has no sensitive write file entry", node)
			}
		}
		if err := verifyTwoNodeKubeadmTranscript(node, entries); err != nil {
			return fmt.Errorf("%s transcript: %w", node, err)
		}
	}
	return nil
}

func verifyTwoNodeKubeadmTranscript(node string, entries []transcriptEntry) error {
	switch node {
	case "cp-1":
		if transcriptHasCommand(entries, "kubeadm", "join") {
			return errors.New("unexpected kubeadm join command on init node")
		}
		if !transcriptHasCommand(entries, "kubeadm", "init") {
			return errors.New("missing kubeadm init command")
		}
		if !transcriptHasCommandFlagValue(entries, "kubeadm", "init", "--config", "/var/lib/katl/test-artifacts/kubeadm-init-cp-1.yaml") {
			return errors.New("kubeadm init command missing generated control-plane config path")
		}
	case "worker-1":
		if transcriptHasCommand(entries, "kubeadm", "init") {
			return errors.New("unexpected kubeadm init command on worker node")
		}
		if !transcriptHasCommand(entries, "kubeadm", "join") {
			return errors.New("missing kubeadm join command")
		}
		if !transcriptHasCommandFlagValue(entries, "kubeadm", "join", "--config", "/var/lib/katl/test-artifacts/kubeadm-join-worker-1.yaml") {
			return errors.New("worker kubeadm join command missing generated worker config path")
		}
		if transcriptHasCommandArg(entries, "kubeadm", "join", "--control-plane") {
			return errors.New("worker kubeadm join command must not include --control-plane")
		}
	}
	return nil
}

func transcriptHasCommand(entries []transcriptEntry, prefix ...string) bool {
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
		if matched {
			return true
		}
	}
	return false
}

func transcriptHasCommandFlagValue(entries []transcriptEntry, first, second, flag, value string) bool {
	for _, entry := range entries {
		if entry.Method != "RunCommand" || len(entry.Argv) < 2 || entry.Argv[0] != first || entry.Argv[1] != second {
			continue
		}
		for i := 2; i < len(entry.Argv); i++ {
			if entry.Argv[i] == flag && i+1 < len(entry.Argv) && entry.Argv[i+1] == value {
				return true
			}
			if entry.Argv[i] == flag+"="+value {
				return true
			}
		}
	}
	return false
}

func readTranscriptFile(path string) ([]transcriptEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errors.New("empty transcript")
	}
	lines := bytes.Split(data, []byte("\n"))
	entries := make([]transcriptEntry, 0, len(lines))
	for i, line := range lines {
		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func collectTwoNodeDiagnostics(transcriptDir string, nodes ...vmtest.RunningInstalledRuntimeNode) {
	diagCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, node := range nodes {
		if !node.VSock.Enabled {
			continue
		}
		summary := twoNodeDiagnosticSummaryFor(transcriptDir, node)
		client, err := vmtest.DialAgent(diagCtx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
		if err != nil {
			summary.DialError = err.Error()
			writeTwoNodeDiagnosticError(node, "dial-agent-error.txt", err, summary)
			_ = writeTwoNodeDiagnosticJSON(filepath.Join(node.Result.Artifacts.GuestDir, "diagnostics-summary.json"), summary)
			continue
		}
		guest := vmtest.NewGuestControl(node.Result, client)
		report := guest.CollectDiagnostics(diagCtx, bootstrapDiagnostics(node.Name))
		if len(report.Errors) > 0 {
			summary.DiagnosticErrors = filepath.Join(node.Result.Artifacts.GuestDir, "diagnostics-errors.json")
			summary.CollectionErrors = append(summary.CollectionErrors, report.Errors...)
			_ = writeTwoNodeDiagnosticJSON(summary.DiagnosticErrors, report.Errors)
		}
		_ = writeTwoNodeDiagnosticJSON(filepath.Join(node.Result.Artifacts.GuestDir, "diagnostics-summary.json"), summary)
		_ = client.Close()
	}
}

type kubectlDiagnosticCommand struct {
	Name string
	Argv []string
}

func collectKubectlDiagnostics(kubeconfigPath, runDir string) {
	diagCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	paths := kubectlDiagnosticPaths(runDir)
	for _, diagnostic := range kubectlDiagnosticCommands(kubeconfigPath) {
		outputPath, ok := paths[diagnostic.Name]
		if !ok {
			continue
		}
		commandCtx, commandCancel := context.WithTimeout(diagCtx, 20*time.Second)
		cmd := exec.CommandContext(commandCtx, diagnostic.Argv[0], diagnostic.Argv[1:]...)
		output, err := cmd.CombinedOutput()
		commandCancel()
		if err != nil {
			output = append(output, []byte("\ncommand error: "+err.Error()+"\n")...)
		}
		if len(output) == 0 {
			output = []byte("\n")
		}
		_ = os.MkdirAll(filepath.Dir(outputPath), 0o755)
		_ = os.WriteFile(outputPath, output, 0o644)
	}
}

func collectKubectlDiagnosticsIfKubeconfigExists(kubeconfigPath, runDir string) bool {
	if strings.TrimSpace(kubeconfigPath) == "" {
		return false
	}
	if _, err := os.Stat(kubeconfigPath); err != nil {
		return false
	}
	collectKubectlDiagnostics(kubeconfigPath, runDir)
	return true
}

func collectKubectlDiagnosticsForFailure(ctx context.Context, initNode vmtest.RunningInstalledRuntimeNode, kubeconfigPath, runDir string) bool {
	if collectKubectlDiagnosticsIfKubeconfigExists(kubeconfigPath, runDir) {
		return true
	}
	if strings.TrimSpace(kubeconfigPath) == "" {
		return false
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 35*time.Second)
	defer cancel()
	data, err := readNodeFileWithRetry(readCtx, initNode, "/etc/kubernetes/admin.conf", 2<<20, 30*time.Second)
	if err != nil {
		_ = os.WriteFile(filepath.Join(runDir, "kubectl-diagnostics-error.txt"), []byte("read admin kubeconfig: "+err.Error()+"\n"), 0o644)
		return false
	}
	if err := os.WriteFile(kubeconfigPath, data, 0o600); err != nil {
		_ = os.WriteFile(filepath.Join(runDir, "kubectl-diagnostics-error.txt"), []byte("write diagnostic kubeconfig: "+err.Error()+"\n"), 0o644)
		return false
	}
	collectKubectlDiagnostics(kubeconfigPath, runDir)
	return true
}

func waitForKubectlNodes(ctx context.Context, kubeconfigPath, outputPath string, timeout time.Duration, wants ...string) ([]byte, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last []byte
	var lastErr error
	for {
		readyCmd := exec.CommandContext(waitCtx, selectedKubectl(), "--kubeconfig", kubeconfigPath, "wait", "--for=condition=Ready", "nodes", "--all", "--timeout=10s")
		readyOutput, readyErr := readyCmd.CombinedOutput()
		cmd := exec.CommandContext(waitCtx, selectedKubectl(), "--kubeconfig", kubeconfigPath, "get", "nodes", "-o", "name")
		output, err := cmd.CombinedOutput()
		if len(output) > 0 {
			last = output
			_ = os.WriteFile(outputPath, output, 0o644)
		}
		if err == nil && readyErr == nil && containsAllText(string(output), wants...) {
			return output, nil
		}
		if readyErr != nil {
			lastErr = fmt.Errorf("nodes are not Ready: %s: %w", strings.TrimSpace(string(readyOutput)), readyErr)
		} else if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("missing nodes: %s", strings.Join(missingText(string(output), wants...), ", "))
		}
		select {
		case <-waitCtx.Done():
			if len(last) == 0 {
				last = []byte("\n")
				_ = os.WriteFile(outputPath, last, 0o644)
			}
			return last, fmt.Errorf("wait for Kubernetes nodes %s: %w", strings.Join(wants, ", "), lastErr)
		case <-time.After(5 * time.Second):
		}
	}
}

func containsAllText(text string, wants ...string) bool {
	return len(missingText(text, wants...)) == 0
}

func missingText(text string, wants ...string) []string {
	var missing []string
	for _, want := range wants {
		if !strings.Contains(text, want) {
			missing = append(missing, want)
		}
	}
	return missing
}

func kubectlDiagnosticPaths(runDir string) map[string]string {
	if strings.TrimSpace(runDir) == "" {
		return nil
	}
	return map[string]string{
		"clusterInfo":    filepath.Join(runDir, "kubectl-cluster-info.txt"),
		"events":         filepath.Join(runDir, "kubectl-get-events.txt"),
		"kubeSystemPods": filepath.Join(runDir, "kubectl-get-pods-kube-system.txt"),
		"nodesWide":      filepath.Join(runDir, "kubectl-get-nodes-wide.txt"),
		"workloadJob":    filepath.Join(runDir, "kubectl-get-job-net-client.txt"),
		"workloadLogs":   filepath.Join(runDir, "kubectl-logs-net-client.txt"),
		"workloadPods":   filepath.Join(runDir, "kubectl-get-pods-katl-vmtest.txt"),
	}
}

func kubectlDiagnosticCommands(kubeconfigPath string) []kubectlDiagnosticCommand {
	kubectl := selectedKubectl()
	return []kubectlDiagnosticCommand{
		{Name: "nodesWide", Argv: []string{kubectl, "--kubeconfig", kubeconfigPath, "get", "nodes", "-o", "wide"}},
		{Name: "kubeSystemPods", Argv: []string{kubectl, "--kubeconfig", kubeconfigPath, "-n", "kube-system", "get", "pods", "-o", "wide"}},
		{Name: "workloadPods", Argv: []string{kubectl, "--kubeconfig", kubeconfigPath, "-n", "katl-vmtest", "get", "pods", "-o", "wide"}},
		{Name: "workloadJob", Argv: []string{kubectl, "--kubeconfig", kubeconfigPath, "-n", "katl-vmtest", "get", "job/net-client", "-o", "wide"}},
		{Name: "workloadLogs", Argv: []string{kubectl, "--kubeconfig", kubeconfigPath, "-n", "katl-vmtest", "logs", "-l", "app=net-client", "--all-containers=true", "--prefix"}},
		{Name: "events", Argv: []string{kubectl, "--kubeconfig", kubeconfigPath, "get", "events", "-A", "--sort-by=.lastTimestamp"}},
		{Name: "clusterInfo", Argv: []string{kubectl, "--kubeconfig", kubeconfigPath, "cluster-info"}},
	}
}

func bootstrapDiagnostics(node string) vmtest.GuestDiagnostics {
	kubeadmRef := kubeadmRefForNode(node)
	plan := vmtest.GuestDiagnostics{
		Timeout: 20 * time.Second,
		Commands: []vmtest.GuestCommandRequest{
			{Name: "kubeadm-ready", Argv: []string{"systemctl", "status", "katl-kubeadm-ready.target"}},
			{Name: "containerd", Argv: []string{"systemctl", "status", "containerd.service"}},
			{Name: "kubelet", Argv: []string{"systemctl", "status", "kubelet.service"}},
			{Name: "crictl-ps", Argv: []string{"crictl", "ps", "-a"}},
			{Name: "crictl-pods", Argv: []string{"crictl", "pods"}},
			{Name: "etc-kubernetes-mount", Argv: []string{"findmnt", "--target", "/etc/kubernetes", "--output", "SOURCE,TARGET,FSTYPE,OPTIONS"}},
			{Name: "network-addresses", Argv: []string{"ip", "addr"}},
			{Name: "network-routes", Argv: []string{"ip", "route"}},
			{Name: "ip-forward", Argv: []string{"sysctl", "net.ipv4.ip_forward"}, AllowFailure: true},
			{Name: "kube-proxy-helper-conntrack", Argv: []string{"test", "-x", "/usr/bin/conntrack"}, AllowFailure: true},
			{Name: "kube-proxy-helper-iptables-nft", Argv: []string{"test", "-x", "/usr/bin/iptables-nft"}, AllowFailure: true},
			{Name: "kube-proxy-helper-ipvsadm", Argv: []string{"test", "-x", "/usr/bin/ipvsadm"}, AllowFailure: true},
			{Name: "kube-proxy-helper-ipset", Argv: []string{"test", "-x", "/usr/bin/ipset"}, AllowFailure: true},
			{Name: "kube-proxy-modules-loaded", Argv: []string{"lsmod"}, AllowFailure: true, StdoutLimit: 512 << 10},
			{Name: "kube-proxy-ipvs-module", Argv: []string{"modprobe", "-n", "-v", "ip_vs"}, AllowFailure: true},
			{Name: "kube-proxy-br-netfilter-module", Argv: []string{"modprobe", "-n", "-v", "br_netfilter"}, AllowFailure: true},
			{Name: "cni-fixture-config", Argv: []string{"find", "/var/lib/katl/test-artifacts/bootstrap-cni", "-maxdepth", "4", "-type", "f", "-printf", "%M %u %g %s %p\n"}, AllowFailure: true},
			{Name: "kubeadm-version", Argv: []string{"kubeadm", "version", "-o", "short"}},
			{Name: "kubeadm-journal", Argv: []string{"journalctl", "--no-pager", "--output=short-monotonic", "-b", "_COMM=kubeadm"}},
			{Name: "kubeadm-pki", Argv: []string{"find", "/etc/kubernetes/pki", "-maxdepth", "2", "-type", "f", "-printf", "%M %u %g %s %p\n"}},
			{Name: "kubelet-state", Argv: []string{"find", "/var/lib/kubelet", "-maxdepth", "2", "-printf", "%M %u %g %s %p\n"}},
			{Name: "kubernetes-logs", Argv: []string{"find", "/var/log/containers", "/var/log/pods", "-maxdepth", "2", "-printf", "%M %u %g %s %p\n"}, AllowFailure: true},
			{Name: "kubernetes-log-tail", Argv: []string{"find", "/var/log/pods", "-maxdepth", "4", "-type", "f", "-name", "*.log", "-print", "-exec", "tail", "-n", "120", "{}", ";"}, StdoutLimit: 1 << 20, AllowFailure: true},
		},
		Files: []vmtest.GuestFileRequest{
			{Name: "node-metadata", Path: "/etc/katl/node.json"},
			{Name: "kubeadm-config", Path: "/etc/katl/kubeadm/" + kubeadmRef + "/config.yaml"},
			{Name: "kubelet-kubeconfig", Path: "/etc/kubernetes/kubelet.conf"},
			{Name: "containerd-log", Path: "/var/lib/katl/test-artifacts/bootstrap-cni/containerd.log", MaxBytes: 4 << 20, StoreContent: true},
			{Name: "kubelet-log", Path: "/var/lib/katl/test-artifacts/bootstrap-cni/kubelet.log", MaxBytes: 4 << 20, StoreContent: true},
		},
		Journals: []vmtest.GuestJournalRequest{{
			Name:     "runtime-handoff",
			Units:    []string{"katl-kubeadm-ready.target", "katl-generation-activate.service", "katl-runtime-handoff-status.service", "containerd.service", "kubelet.service"},
			MaxBytes: 1 << 20,
		}, {
			Name:     "container-runtime",
			Units:    []string{"containerd.service", "kubelet.service"},
			MaxBytes: 1 << 20,
		}, {
			Name:     "katlc-agent",
			Units:    []string{"katlc-agent.service"},
			MaxBytes: 1 << 20,
		}},
	}
	if kubeadmRef == "control-plane" {
		plan.Files = append(plan.Files,
			vmtest.GuestFileRequest{Name: "admin-kubeconfig", Path: "/etc/kubernetes/admin.conf"},
			vmtest.GuestFileRequest{Name: "kube-apiserver-manifest", Path: "/etc/kubernetes/manifests/kube-apiserver.yaml"},
			vmtest.GuestFileRequest{Name: "kube-controller-manager-manifest", Path: "/etc/kubernetes/manifests/kube-controller-manager.yaml"},
			vmtest.GuestFileRequest{Name: "kube-scheduler-manifest", Path: "/etc/kubernetes/manifests/kube-scheduler.yaml"},
			vmtest.GuestFileRequest{Name: "etcd-manifest", Path: "/etc/kubernetes/manifests/etcd.yaml"},
		)
	}
	return plan
}

type twoNodeDiagnosticSummary struct {
	Node                 string   `json:"node"`
	BootstrapTranscript  string   `json:"bootstrapTranscript,omitempty"`
	DiagnosticTranscript string   `json:"diagnosticTranscript,omitempty"`
	GuestDiagnostics     string   `json:"guestDiagnostics,omitempty"`
	DiagnosticErrors     string   `json:"diagnosticErrors,omitempty"`
	DialError            string   `json:"dialError,omitempty"`
	CollectionErrors     []string `json:"collectionErrors,omitempty"`
}

func twoNodeDiagnosticSummaryFor(transcriptDir string, node vmtest.RunningInstalledRuntimeNode) twoNodeDiagnosticSummary {
	return twoNodeDiagnosticSummary{
		Node:                 node.Name,
		BootstrapTranscript:  twoNodeBootstrapTranscriptPath(transcriptDir, node.Name),
		DiagnosticTranscript: node.Result.Artifacts.VSockTranscript,
		GuestDiagnostics:     filepath.Join(node.Result.Artifacts.GuestDir, "diagnostics.json"),
	}
}

func twoNodeBootstrapTranscriptPath(transcriptDir, node string) string {
	if transcriptDir == "" {
		return ""
	}
	return filepath.Join(transcriptDir, node+".jsonl")
}

func writeTwoNodeDiagnosticError(node vmtest.RunningInstalledRuntimeNode, name string, err error, summary twoNodeDiagnosticSummary) {
	_ = os.MkdirAll(node.Result.Artifacts.GuestDir, 0o755)
	lines := []string{err.Error()}
	if summary.BootstrapTranscript != "" {
		lines = append(lines, "bootstrapTranscript="+summary.BootstrapTranscript)
	}
	if summary.DiagnosticTranscript != "" {
		lines = append(lines, "diagnosticTranscript="+summary.DiagnosticTranscript)
	}
	if summary.GuestDiagnostics != "" {
		lines = append(lines, "guestDiagnostics="+summary.GuestDiagnostics)
	}
	_ = os.WriteFile(filepath.Join(node.Result.Artifacts.GuestDir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func writeTwoNodeDiagnosticJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

type kubeconfigMetadata struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Modified  string `json:"modified,omitempty"`
	StatError string `json:"statError,omitempty"`
}

func writeKubeconfigMetadata(kubeconfigPath, metadataPath string) error {
	metadata := kubeconfigMetadata{Path: kubeconfigPath}
	info, err := os.Stat(kubeconfigPath)
	switch {
	case err == nil:
		metadata.Exists = true
		metadata.SizeBytes = info.Size()
		metadata.Mode = fmt.Sprintf("%#o", info.Mode().Perm())
		metadata.Modified = info.ModTime().UTC().Format(time.RFC3339Nano)
	case errors.Is(err, os.ErrNotExist):
		metadata.Exists = false
	default:
		metadata.StatError = err.Error()
	}
	return writeTwoNodeDiagnosticJSON(metadataPath, metadata)
}

func kubeadmRefForNode(name string) string {
	if name == "worker-1" {
		return "worker"
	}
	return "control-plane"
}

func finishTwoNodeResult(t *testing.T, runner vmtest.Runner, scenario vmtest.Scenario, result vmtest.Result, status vmtest.Status, failure string) {
	t.Helper()
	result.Status = status
	result.FailureSummary = failure
	result.Finished = time.Now().UTC()
	if !result.Started.IsZero() {
		result.DurationMS = result.Finished.Sub(result.Started).Milliseconds()
	}
	result.Phases = append(result.Phases, vmtest.PhaseResult{
		Name:           "multi-node-smoke",
		Status:         status,
		Started:        result.Started,
		Finished:       result.Finished,
		DurationMS:     result.DurationMS,
		FailureSummary: failure,
	})
	if err := runner.Write(scenario, result); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

func stopNode(t *testing.T, node vmtest.RunningInstalledRuntimeNode) {
	t.Helper()
	if t.Failed() {
		if err := node.StopFailure("parent scenario failed"); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("preserve failed %s: %v", node.Name, err)
		}
		return
	}
	if err := node.Stop(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("stop %s: %v", node.Name, err)
	}
}

func firstString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func compactStringMap(values map[string]string) map[string]string {
	out := make(map[string]string)
	for key, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func uint32String(value uint32) string {
	return strconv.FormatUint(uint64(value), 10)
}

func diagnosticSummaryPaths(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	out := make(map[string]string, len(nodes))
	for _, node := range nodes {
		if node.Name == "" || node.Result.Artifacts.GuestDir == "" {
			continue
		}
		out[node.Name] = filepath.Join(node.Result.Artifacts.GuestDir, "diagnostics-summary.json")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nodeResultPaths(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeArtifactPaths(nodes, func(paths vmtest.ArtifactPaths) string {
		return paths.Result
	})
}

func nodeScenarioPaths(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeArtifactPaths(nodes, func(paths vmtest.ArtifactPaths) string {
		return paths.Scenario
	})
}

func launchCommandPaths(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeArtifactPaths(nodes, func(paths vmtest.ArtifactPaths) string {
		return paths.LaunchCommand
	})
}

func domainXMLPaths(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeArtifactPaths(nodes, func(paths vmtest.ArtifactPaths) string {
		return paths.DomainXML
	})
}

func installedRuntimeInputPaths(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeArtifactPaths(nodes, func(paths vmtest.ArtifactPaths) string {
		return paths.InstalledRuntime
	})
}

func vsockTranscriptPaths(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeArtifactPaths(nodes, func(paths vmtest.ArtifactPaths) string {
		return paths.VSockTranscript
	})
}

func libvirtLeasePaths(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeArtifactPaths(nodes, func(paths vmtest.ArtifactPaths) string {
		return paths.LibvirtLease
	})
}

func serialLogPaths(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeArtifactPaths(nodes, func(paths vmtest.ArtifactPaths) string {
		return paths.RuntimeSerial
	})
}

func nodeDomainNames(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeResultValues(nodes, func(result vmtest.Result) string {
		return result.DomainName
	})
}

func nodeMACAddresses(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeResultValues(nodes, func(result vmtest.Result) string {
		return result.MACAddress
	})
}

func nodeIPAddresses(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	return nodeResultValues(nodes, func(result vmtest.Result) string {
		return result.IPAddress
	})
}

func nodeArtifactPaths(nodes []vmtest.RunningInstalledRuntimeNode, path func(vmtest.ArtifactPaths) string) map[string]string {
	return nodeResultValues(nodes, func(result vmtest.Result) string {
		return path(result.Artifacts)
	})
}

func nodeResultValues(nodes []vmtest.RunningInstalledRuntimeNode, value func(vmtest.Result) string) map[string]string {
	out := make(map[string]string, len(nodes))
	for _, node := range nodes {
		got := value(node.Result)
		if node.Name == "" || got == "" {
			continue
		}
		out[node.Name] = got
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func TestTwoNodeArtifactManifestRecordsWorldInputs(t *testing.T) {
	inputs := twoNodeFixtureInputs("cp.qcow2", "qcow2", "worker.raw", string(vmtest.DiskRaw), "cp-esp", "worker-esp", "cp-fixture.json", "worker-fixture.json", "cp-node.json", "worker-node.json")
	if inputs["cp-1"].DiskFormat != "qcow2" || inputs["worker-1"].DiskFormat != string(vmtest.DiskRaw) {
		t.Fatalf("fixture input formats = %#v", inputs)
	}
	path := filepath.Join(t.TempDir(), "two-node-artifacts.json")
	if err := writeTwoNodeArtifactManifest(path, twoNodeArtifactManifest{
		VMTestRun:          "/tmp/run.json",
		WorldManifest:      "/tmp/world.json",
		HostCapabilities:   "/tmp/host-capabilities.json",
		MkosiArtifactIndex: "/tmp/mkosi-artifacts.json",
		ControlPlaneRunDir: "/tmp/cp-run",
		WorkerRunDir:       "/tmp/worker-run",
		NodeResults: map[string]string{
			"cp-1":     "/tmp/cp-run/result.json",
			"worker-1": "/tmp/worker-run/result.json",
		},
		NodeScenarios: map[string]string{
			"cp-1":     "/tmp/cp-run/scenario.json",
			"worker-1": "/tmp/worker-run/scenario.json",
		},
		LaunchCommands: map[string]string{
			"cp-1":     "/tmp/cp-run/vm/launch-command.txt",
			"worker-1": "/tmp/worker-run/vm/launch-command.txt",
		},
		DomainXMLs: map[string]string{
			"cp-1":     "/tmp/cp-run/vm/domain.xml",
			"worker-1": "/tmp/worker-run/vm/domain.xml",
		},
		InstalledRuntimeInputs: map[string]string{
			"cp-1":     "/tmp/cp-run/manifests/installed-runtime.json",
			"worker-1": "/tmp/worker-run/manifests/installed-runtime.json",
		},
		VSockTranscripts: map[string]string{
			"cp-1":     "/tmp/cp-run/vm/vsock-transcript.jsonl",
			"worker-1": "/tmp/worker-run/vm/vsock-transcript.jsonl",
		},
		FixtureInputs:            inputs,
		FixtureProducerScenarios: map[string]string{"cp-1": "/tmp/fixture-cp/scenario.json", "worker-1": "/tmp/fixture-worker/scenario.json"},
		FixtureProducerResults:   map[string]string{"cp-1": "/tmp/fixture-cp/result.json", "worker-1": "/tmp/fixture-worker/result.json"},
		KubeconfigMetadata:       "/tmp/run/operator-kubeconfig-metadata.json",
		BootstrapFixture:         (&bootstrapFixtureInputs{Manifests: []string{"/tmp/cni.yaml"}, Waits: []string{"nodes-ready"}}).manifestValue(),
		SerialLogs:               map[string]string{"cp-1": "/tmp/cp-run/vm/runtime-serial.log", "worker-1": "/tmp/worker-run/vm/runtime-serial.log"},
		Diagnostics:              map[string]string{"cp-1": "/tmp/cp-guest/diagnostics-summary.json", "worker-1": "/tmp/worker-guest/diagnostics-summary.json"},
		KubectlDiagnostics:       map[string]string{"nodesWide": "/tmp/run/kubectl-get-nodes-wide.txt"},
	}); err != nil {
		t.Fatalf("writeTwoNodeArtifactManifest() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact manifest: %v", err)
	}
	var manifest twoNodeArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode artifact manifest: %v", err)
	}
	if manifest.FixtureInputs["cp-1"].FixtureManifest != "cp-fixture.json" || manifest.FixtureInputs["worker-1"].NodeMetadata != "worker-node.json" {
		t.Fatalf("artifact manifest fixture inputs = %#v", manifest.FixtureInputs)
	}
	if manifest.VMTestRun != "/tmp/run.json" || manifest.WorldManifest != "/tmp/world.json" || manifest.HostCapabilities != "/tmp/host-capabilities.json" || manifest.MkosiArtifactIndex != "/tmp/mkosi-artifacts.json" {
		t.Fatalf("artifact manifest world provenance = %q %q %q %q", manifest.VMTestRun, manifest.WorldManifest, manifest.HostCapabilities, manifest.MkosiArtifactIndex)
	}
	if manifest.FixtureProducerScenarios["cp-1"] != "/tmp/fixture-cp/scenario.json" || manifest.FixtureProducerResults["worker-1"] != "/tmp/fixture-worker/result.json" {
		t.Fatalf("artifact manifest fixture provenance = %#v %#v", manifest.FixtureProducerScenarios, manifest.FixtureProducerResults)
	}
	if manifest.Diagnostics["cp-1"] != "/tmp/cp-guest/diagnostics-summary.json" || manifest.Diagnostics["worker-1"] != "/tmp/worker-guest/diagnostics-summary.json" {
		t.Fatalf("artifact manifest diagnostics = %#v", manifest.Diagnostics)
	}
	if manifest.SerialLogs["cp-1"] != "/tmp/cp-run/vm/runtime-serial.log" || manifest.SerialLogs["worker-1"] != "/tmp/worker-run/vm/runtime-serial.log" {
		t.Fatalf("artifact manifest serial logs = %#v", manifest.SerialLogs)
	}
	if manifest.NodeResults["cp-1"] != "/tmp/cp-run/result.json" || manifest.LaunchCommands["worker-1"] != "/tmp/worker-run/vm/launch-command.txt" {
		t.Fatalf("artifact manifest node artifacts = %#v %#v", manifest.NodeResults, manifest.LaunchCommands)
	}
	if manifest.DomainXMLs["cp-1"] != "/tmp/cp-run/vm/domain.xml" || manifest.DomainXMLs["worker-1"] != "/tmp/worker-run/vm/domain.xml" {
		t.Fatalf("artifact manifest domain XMLs = %#v", manifest.DomainXMLs)
	}
	if manifest.NodeScenarios["cp-1"] != "/tmp/cp-run/scenario.json" || manifest.NodeScenarios["worker-1"] != "/tmp/worker-run/scenario.json" {
		t.Fatalf("artifact manifest node scenarios = %#v", manifest.NodeScenarios)
	}
	if manifest.InstalledRuntimeInputs["cp-1"] != "/tmp/cp-run/manifests/installed-runtime.json" || manifest.VSockTranscripts["worker-1"] != "/tmp/worker-run/vm/vsock-transcript.jsonl" {
		t.Fatalf("artifact manifest runtime artifacts = %#v %#v", manifest.InstalledRuntimeInputs, manifest.VSockTranscripts)
	}
	if manifest.KubeconfigMetadata != "/tmp/run/operator-kubeconfig-metadata.json" {
		t.Fatalf("artifact manifest kubeconfig metadata = %q", manifest.KubeconfigMetadata)
	}
	if manifest.BootstrapFixture == nil || !stringSlicesEqual(manifest.BootstrapFixture.Manifests, []string{"/tmp/cni.yaml"}) || !stringSlicesEqual(manifest.BootstrapFixture.Waits, []string{"nodes-ready"}) {
		t.Fatalf("artifact manifest bootstrap fixture = %#v", manifest.BootstrapFixture)
	}
	if manifest.KubectlDiagnostics["nodesWide"] != "/tmp/run/kubectl-get-nodes-wide.txt" {
		t.Fatalf("artifact manifest kubectl diagnostics = %#v", manifest.KubectlDiagnostics)
	}
}

func TestPlanTwoNodeWorldSmokeRunWritesSetupFailureForMissingPublishedFixture(t *testing.T) {
	world := twoNodeTestWorld(t)
	run, err := planTwoNodeWorldSmokeRun(world, t.TempDir(), "v1.36.1", vmtest.KVMOff)
	if err == nil || !strings.Contains(err.Error(), "published installed runtime fixture is missing") {
		t.Fatalf("planTwoNodeWorldSmokeRun() error = %v, want missing published fixture", err)
	}
	if run.WorldScenario == nil {
		t.Fatal("planTwoNodeWorldSmokeRun() did not return world scenario on setup failure")
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

func TestPlanTwoNodeWorldSmokeRunPrefersWorldPublishedFixtures(t *testing.T) {
	world := twoNodeTestWorld(t)
	world.RunIndex = filepath.Join(world.RunDir, "custom-run.json")
	repo := t.TempDir()
	writeKatlctlPublishedInstalledRuntimeFixture(t, vmtest.DefaultVMTestCacheDir(repo), "repo-cp", "cp-1", vmtest.ControlPlane)
	writeKatlctlPublishedInstalledRuntimeFixture(t, vmtest.DefaultVMTestCacheDir(repo), "repo-worker", "worker-1", vmtest.Worker)
	writeKatlctlPublishedInstalledRuntimeFixture(t, world.CacheDir, "world-cp", "cp-1", vmtest.ControlPlane)
	writeKatlctlPublishedInstalledRuntimeFixture(t, world.CacheDir, "world-worker", "worker-1", vmtest.Worker)
	world.ResourceManifest = filepath.Join(world.RunDir, "resource-test-manifest.json")
	world.ResourceDigest = strings.Repeat("a", 64)
	world.PackageLock = filepath.Join(world.RunDir, "resource-package-lock.json")
	world.PackageLockDigest = strings.Repeat("b", 64)

	run, err := planTwoNodeWorldSmokeRun(world, repo, "v1.36.1", vmtest.KVMOff)
	if err != nil {
		t.Fatalf("planTwoNodeWorldSmokeRun() error = %v", err)
	}
	assertFileContent(t, run.Inputs.ControlPlaneDisk, "disk-world-cp")
	assertFileContent(t, run.Inputs.WorkerDisk, "disk-world-worker")
	if !hasPathPrefix(run.Inputs.ControlPlaneFixture, run.WorldScenario.Dir) || !hasPathPrefix(run.Inputs.WorkerFixture, run.WorldScenario.Dir) {
		t.Fatalf("fixtures were not staged into world scenario: cp=%q worker=%q", run.Inputs.ControlPlaneFixture, run.Inputs.WorkerFixture)
	}
	if run.Inputs.WorldProvenance.VMTestRun != filepath.Join(world.RunDir, "custom-run.json") || run.Inputs.WorldProvenance.WorldManifest != filepath.Join(world.RunDir, "world.json") || run.Inputs.WorldProvenance.HostCapabilities != filepath.Join(world.RunDir, "host-capabilities.json") {
		t.Fatalf("world provenance = %#v", run.Inputs.WorldProvenance)
	}
	if run.Inputs.WorldProvenance.ResourceManifest != world.ResourceManifest || run.Inputs.WorldProvenance.ResourceManifestSHA256 != world.ResourceDigest || run.Inputs.WorldProvenance.PackageLock != world.PackageLock || run.Inputs.WorldProvenance.PackageLockSHA256 != world.PackageLockDigest {
		t.Fatalf("world resource provenance = %#v", run.Inputs.WorldProvenance)
	}
	if run.Inputs.WorldProvenance.FixtureProducerResults["cp-1"] != filepath.Join(world.ScenarioDir, "first-install-installed-runtime-fixture-cp-1-control-plane", "result.json") {
		t.Fatalf("fixture producer results = %#v", run.Inputs.WorldProvenance.FixtureProducerResults)
	}
}

func TestPlanTwoNodeWorldSmokeRunRejectsRepoOnlyPublishedFixtures(t *testing.T) {
	world := twoNodeTestWorld(t)
	repo := t.TempDir()
	writeKatlctlPublishedInstalledRuntimeFixture(t, vmtest.DefaultVMTestCacheDir(repo), "repo-cp", "cp-1", vmtest.ControlPlane)
	writeKatlctlPublishedInstalledRuntimeFixture(t, vmtest.DefaultVMTestCacheDir(repo), "repo-worker", "worker-1", vmtest.Worker)

	run, err := planTwoNodeWorldSmokeRun(world, repo, "v1.36.1", vmtest.KVMOff)
	if err == nil || !strings.Contains(err.Error(), "published installed runtime fixture is missing") {
		t.Fatalf("planTwoNodeWorldSmokeRun() error = %v, want missing world fixture", err)
	}
	if run.WorldScenario == nil {
		t.Fatal("planTwoNodeWorldSmokeRun() did not return world scenario on setup failure")
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

func twoNodeTestWorld(t *testing.T) vmtest.World {
	t.Helper()
	root := t.TempDir()
	leaseFile := filepath.Join(root, "network", "leases.json")
	if err := os.MkdirAll(filepath.Dir(leaseFile), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(leaseFile, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return vmtest.World{
		APIVersion:  vmtest.WorldAPIVersion,
		Kind:        vmtest.WorldKind,
		RunID:       "run-1",
		RunDir:      root,
		CacheDir:    filepath.Join(root, "cache"),
		ArtifactDir: filepath.Join(root, "artifacts"),
		ScenarioDir: filepath.Join(root, "scenarios"),
		Network: vmtest.WorldNetwork{
			Backend:   vmtest.NetworkLibvirt,
			Name:      "katl-default",
			CIDR:      "10.77.0.0/24",
			Gateway:   "10.77.0.1",
			LeaseFile: leaseFile,
		},
		Libvirt: vmtest.WorldLibvirt{
			URI:          "qemu:///system",
			Network:      "katl-default",
			StoragePool:  "default",
			StoragePath:  "/var/lib/libvirt/images",
			DomainPrefix: "katl-run-1",
		},
		Capabilities: map[string]vmtest.WorldStatus{"libvirt": vmtest.WorldStatusPassed},
	}
}

func TestTwoNodeHostToolPrereqsUseSelectedKubectl(t *testing.T) {
	t.Setenv("KATL_VMTEST_KUBECTL", "/tmp/selected-kubectl")
	var checked string
	missing := twoNodeHostToolPrereqs(func(name string) (string, error) {
		checked = name
		return name, nil
	})
	if len(missing) != 0 {
		t.Fatalf("missing prereqs = %#v", missing)
	}
	if checked != "/tmp/selected-kubectl" {
		t.Fatalf("checked kubectl = %q, want selected binary", checked)
	}
}

func TestRequireSmokePrereqsWritesSkippedResult(t *testing.T) {
	stateRoot := t.TempDir()
	runner := vmtest.NewRunner(vmtest.Options{
		Enabled:   true,
		StateRoot: stateRoot,
		RunID:     "skip-run",
		Missing:   vmtest.MissingSkips,
	})
	scenario := vmtest.Scenario{Name: "missing-prereq-smoke"}
	t.Run("skip", func(t *testing.T) {
		result := planStartedVMResult(t, runner, scenario)
		requireSmokePrereqs(t, runner, scenario, result, "missing prereqs", []vmtest.MissingPrerequisite{{
			Name:   "KATL_REQUIRED_INPUT",
			Detail: "set test input",
		}})
		t.Fatal("requireSmokePrereqs returned without skipping")
	})

	data, err := os.ReadFile(filepath.Join(stateRoot, "skip-run", "result.json"))
	if err != nil {
		t.Fatalf("read skipped result: %v", err)
	}
	var result vmtest.Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode skipped result: %v", err)
	}
	if result.Status != vmtest.StatusSkipped || !strings.Contains(result.FailureSummary, "missing prereqs") {
		t.Fatalf("result status = %q summary = %q", result.Status, result.FailureSummary)
	}
	if len(result.Missing) != 1 || result.Missing[0].Name != "KATL_REQUIRED_INPUT" {
		t.Fatalf("result missing prerequisites = %#v", result.Missing)
	}
}

func TestFinishTwoNodeResultWritesSmokePhase(t *testing.T) {
	stateRoot := t.TempDir()
	runner := vmtest.NewRunner(vmtest.Options{
		Enabled:   true,
		StateRoot: stateRoot,
		RunID:     "failed-run",
	})
	scenario := vmtest.Scenario{Name: "two-node"}
	result := planStartedVMResult(t, runner, scenario)
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, "bootstrap failed")

	data, err := os.ReadFile(filepath.Join(stateRoot, "failed-run", "result.json"))
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var written vmtest.Result
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if written.Status != vmtest.StatusFailed || written.FailureSummary != "bootstrap failed" {
		t.Fatalf("result = %#v", written)
	}
	if len(written.Phases) != 1 || written.Phases[0].Name != "multi-node-smoke" || written.Phases[0].Status != vmtest.StatusFailed || written.Phases[0].FailureSummary != "bootstrap failed" {
		t.Fatalf("result phases = %#v", written.Phases)
	}
}

func TestTwoNodeSmokeArtifactManifestUsesPlannedNodeArtifacts(t *testing.T) {
	result, err := vmtest.NewRunner(vmtest.Options{
		StateRoot: t.TempDir(),
		RunID:     "run-1",
	}).Plan(vmtest.Scenario{Name: "two-node"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	cpResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, "cp-1")
	if err != nil {
		t.Fatalf("plan cp-1: %v", err)
	}
	workerResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, "worker-1")
	if err != nil {
		t.Fatalf("plan worker-1: %v", err)
	}
	cpResult.DomainName = "katl-cp-1"
	cpResult.MACAddress = "52:54:00:00:00:01"
	cpResult.IPAddress = "192.0.2.11"
	workerResult.DomainName = "katl-worker-1"
	workerResult.MACAddress = "52:54:00:00:00:02"
	workerResult.IPAddress = "192.0.2.12"
	nodes := []vmtest.RunningInstalledRuntimeNode{
		{Name: "cp-1", Result: cpResult},
		{Name: "worker-1", Result: workerResult},
	}
	if err := writeTwoNodeSmokeArtifactManifest(result, twoNodeSmokeInputs{
		ControlPlaneDisk:     "cp.raw",
		ControlPlaneESP:      "esp",
		ControlPlaneFixture:  "cp-fixture.json",
		ControlPlaneMetadata: "cp-node.json",
		WorkerDisk:           "worker.raw",
		WorkerESP:            "esp",
		WorkerFixture:        "worker-fixture.json",
		WorkerMetadata:       "worker-node.json",
		WorldProvenance: multiNodeWorldProvenancePaths{
			WorldManifest:            "/tmp/world.json",
			HostCapabilities:         "/tmp/host-capabilities.json",
			MkosiArtifactIndex:       "/tmp/mkosi-artifacts.json",
			NetworkLeaseFile:         "/tmp/network-leases.json",
			FixtureProducerScenarios: map[string]string{"cp-1": "/tmp/fixture-cp/scenario.json"},
			FixtureProducerResults:   map[string]string{"worker-1": "/tmp/fixture-worker/result.json"},
		},
	}, filepath.Join(result.RunDir, "agent-transcripts"), nodes, bootstrapFixtureInputs{}, nil, nil); err != nil {
		t.Fatalf("writeTwoNodeSmokeArtifactManifest() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(result.ManifestDir, "two-node-artifacts.json"))
	if err != nil {
		t.Fatalf("read artifact manifest: %v", err)
	}
	var manifest twoNodeArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode artifact manifest: %v", err)
	}
	if manifest.ControlPlaneRunDir != cpResult.RunDir || manifest.WorkerRunDir != workerResult.RunDir {
		t.Fatalf("run dirs = %q %q", manifest.ControlPlaneRunDir, manifest.WorkerRunDir)
	}
	if manifest.SerialLogs["cp-1"] != cpResult.Artifacts.RuntimeSerial || manifest.LaunchCommands["worker-1"] != workerResult.Artifacts.LaunchCommand || manifest.DomainXMLs["cp-1"] != cpResult.Artifacts.DomainXML {
		t.Fatalf("planned artifact indexes = serial %#v launch %#v domain %#v", manifest.SerialLogs, manifest.LaunchCommands, manifest.DomainXMLs)
	}
	if manifest.NodeScenarios["cp-1"] != cpResult.Artifacts.Scenario || manifest.NodeScenarios["worker-1"] != workerResult.Artifacts.Scenario {
		t.Fatalf("planned node scenario indexes = %#v", manifest.NodeScenarios)
	}
	if manifest.NodeDomains["cp-1"] != "katl-cp-1" || manifest.NodeMACs["worker-1"] != "52:54:00:00:00:02" || manifest.NodeIPs["cp-1"] != "192.0.2.11" {
		t.Fatalf("planned node identity = domains %#v macs %#v ips %#v", manifest.NodeDomains, manifest.NodeMACs, manifest.NodeIPs)
	}
	if manifest.InstalledRuntimeInputs["worker-1"] != workerResult.Artifacts.InstalledRuntime || manifest.VSockTranscripts["cp-1"] != cpResult.Artifacts.VSockTranscript {
		t.Fatalf("planned runtime indexes = installed %#v vsock %#v", manifest.InstalledRuntimeInputs, manifest.VSockTranscripts)
	}
	if manifest.LibvirtLeases["cp-1"] != cpResult.Artifacts.LibvirtLease || manifest.LibvirtLeases["worker-1"] != workerResult.Artifacts.LibvirtLease {
		t.Fatalf("planned libvirt lease artifacts = %#v", manifest.LibvirtLeases)
	}
	if manifest.WorldManifest != "/tmp/world.json" || manifest.NetworkLeases != "/tmp/network-leases.json" || manifest.FixtureProducerScenarios["cp-1"] != "/tmp/fixture-cp/scenario.json" {
		t.Fatalf("planned provenance = %#v", manifest)
	}
}

func TestNodeArtifactPaths(t *testing.T) {
	nodes := []vmtest.RunningInstalledRuntimeNode{
		{
			Name: "cp-1",
			Result: vmtest.Result{Artifacts: vmtest.ArtifactPaths{
				Scenario:         "/tmp/cp-1/scenario.json",
				Result:           "/tmp/cp-1/result.json",
				LaunchCommand:    "/tmp/cp-1/vm/launch-command.txt",
				DomainXML:        "/tmp/cp-1/vm/domain.xml",
				InstalledRuntime: "/tmp/cp-1/manifests/installed-runtime.json",
				RuntimeSerial:    "/tmp/cp-1/vm/runtime-serial.log",
				VSockTranscript:  "/tmp/cp-1/vm/vsock-transcript.jsonl",
			}},
		},
		{
			Name: "",
			Result: vmtest.Result{Artifacts: vmtest.ArtifactPaths{
				Scenario: "/tmp/ignored/scenario.json",
				Result:   "/tmp/ignored/result.json",
			}},
		},
		{
			Name: "worker-1",
			Result: vmtest.Result{Artifacts: vmtest.ArtifactPaths{
				Scenario:         "/tmp/worker-1/scenario.json",
				Result:           "/tmp/worker-1/result.json",
				LaunchCommand:    "/tmp/worker-1/vm/launch-command.txt",
				DomainXML:        "/tmp/worker-1/vm/domain.xml",
				InstalledRuntime: "/tmp/worker-1/manifests/installed-runtime.json",
				RuntimeSerial:    "/tmp/worker-1/vm/runtime-serial.log",
				VSockTranscript:  "/tmp/worker-1/vm/vsock-transcript.jsonl",
			}},
		},
	}

	if got := nodeResultPaths(nodes); got["cp-1"] != "/tmp/cp-1/result.json" || got["worker-1"] != "/tmp/worker-1/result.json" || len(got) != 2 {
		t.Fatalf("node result paths = %#v", got)
	}
	if got := nodeScenarioPaths(nodes); got["cp-1"] != "/tmp/cp-1/scenario.json" || got["worker-1"] != "/tmp/worker-1/scenario.json" || len(got) != 2 {
		t.Fatalf("node scenario paths = %#v", got)
	}
	if got := launchCommandPaths(nodes); got["cp-1"] != "/tmp/cp-1/vm/launch-command.txt" || got["worker-1"] != "/tmp/worker-1/vm/launch-command.txt" || len(got) != 2 {
		t.Fatalf("launch command paths = %#v", got)
	}
	if got := domainXMLPaths(nodes); got["cp-1"] != "/tmp/cp-1/vm/domain.xml" || got["worker-1"] != "/tmp/worker-1/vm/domain.xml" || len(got) != 2 {
		t.Fatalf("domain XML paths = %#v", got)
	}
	if got := installedRuntimeInputPaths(nodes); got["cp-1"] != "/tmp/cp-1/manifests/installed-runtime.json" || got["worker-1"] != "/tmp/worker-1/manifests/installed-runtime.json" || len(got) != 2 {
		t.Fatalf("installed runtime input paths = %#v", got)
	}
	if got := serialLogPaths(nodes); got["cp-1"] != "/tmp/cp-1/vm/runtime-serial.log" || got["worker-1"] != "/tmp/worker-1/vm/runtime-serial.log" || len(got) != 2 {
		t.Fatalf("serial log paths = %#v", got)
	}
	if got := vsockTranscriptPaths(nodes); got["cp-1"] != "/tmp/cp-1/vm/vsock-transcript.jsonl" || got["worker-1"] != "/tmp/worker-1/vm/vsock-transcript.jsonl" || len(got) != 2 {
		t.Fatalf("vsock transcript paths = %#v", got)
	}
	if got := launchCommandPaths([]vmtest.RunningInstalledRuntimeNode{{Name: "cp-1"}}); got != nil {
		t.Fatalf("empty launch command paths = %#v", got)
	}
	if got := domainXMLPaths([]vmtest.RunningInstalledRuntimeNode{{Name: "cp-1"}}); got != nil {
		t.Fatalf("empty domain XML paths = %#v", got)
	}
}

func TestWriteKubeconfigMetadata(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "operator-kubeconfig.yaml")
	metadataPath := filepath.Join(dir, "metadata", "operator-kubeconfig-metadata.json")
	if err := os.WriteFile(kubeconfigPath, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	if err := writeKubeconfigMetadata(kubeconfigPath, metadataPath); err != nil {
		t.Fatalf("writeKubeconfigMetadata() error = %v", err)
	}
	var metadata kubeconfigMetadata
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if metadata.Path != kubeconfigPath || !metadata.Exists || metadata.SizeBytes == 0 || metadata.Mode != "0600" || metadata.Modified == "" || metadata.StatError != "" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if strings.Contains(string(data), "apiVersion") || strings.Contains(string(data), "Config") {
		t.Fatalf("metadata leaked kubeconfig content:\n%s", data)
	}

	missingPath := filepath.Join(dir, "missing.yaml")
	missingMetadataPath := filepath.Join(dir, "missing-metadata.json")
	if err := writeKubeconfigMetadata(missingPath, missingMetadataPath); err != nil {
		t.Fatalf("write missing kubeconfig metadata: %v", err)
	}
	data, err = os.ReadFile(missingMetadataPath)
	if err != nil {
		t.Fatalf("read missing metadata: %v", err)
	}
	metadata = kubeconfigMetadata{}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("decode missing metadata: %v", err)
	}
	if metadata.Path != missingPath || metadata.Exists || metadata.SizeBytes != 0 || metadata.Mode != "" || metadata.Modified != "" || metadata.StatError != "" {
		t.Fatalf("missing metadata = %#v", metadata)
	}
}

func TestBootstrapFixtureInputsFromEnv(t *testing.T) {
	t.Setenv("KATL_BOOTSTRAP_MANIFESTS", strings.Join([]string{"/tmp/01-cni.yaml", " /tmp/02-workload.yaml "}, string(os.PathListSeparator)))
	t.Setenv("KATL_BOOTSTRAP_MANIFEST", " /tmp/03-extra.yaml ")
	t.Setenv("KATL_BOOTSTRAP_WAITS", "\napi-ready\npods-ready:kube-system:k8s-app=kube-dns\n")
	t.Setenv("KATL_BOOTSTRAP_WAIT", " nodes-ready ")

	got := bootstrapFixtureInputsFromEnv()
	if !stringSlicesEqual(got.Manifests, []string{"/tmp/01-cni.yaml", "/tmp/02-workload.yaml", "/tmp/03-extra.yaml"}) {
		t.Fatalf("bootstrap manifests = %#v", got.Manifests)
	}
	if !stringSlicesEqual(got.Waits, []string{"api-ready", "pods-ready:kube-system:k8s-app=kube-dns", "nodes-ready"}) {
		t.Fatalf("bootstrap waits = %#v", got.Waits)
	}
	if got.empty() {
		t.Fatalf("bootstrap fixture reported empty: %#v", got)
	}
	if got.manifestValue() == nil {
		t.Fatalf("bootstrap fixture manifest value is nil for non-empty fixture")
	}

	t.Setenv("KATL_BOOTSTRAP_MANIFESTS", "")
	t.Setenv("KATL_BOOTSTRAP_MANIFEST", "")
	t.Setenv("KATL_BOOTSTRAP_WAITS", "")
	t.Setenv("KATL_BOOTSTRAP_WAIT", "")
	if got := bootstrapFixtureInputsFromEnv(); !got.empty() || got.manifestValue() != nil {
		t.Fatalf("empty bootstrap fixture = %#v", got)
	}
}

func TestUsableClusterBootstrapFixtureInputs(t *testing.T) {
	repo := katlRepoRoot(t)
	got := usableClusterBootstrapFixtureInputs(repo)
	if !stringSlicesEqual(got.Manifests, []string{
		filepath.Join(repo, "internal", "vmtest", "scenarios", "testdata", "bootstrap", "cross-node-workload.yaml"),
	}) {
		t.Fatalf("usable fixture manifests = %#v", got.Manifests)
	}
	if !stringSlicesEqual(got.PreWaits, []string{
		"nodes-ready",
	}) {
		t.Fatalf("usable fixture pre-waits = %#v", got.PreWaits)
	}
	if !stringSlicesEqual(got.Waits, []string{
		"rollout-status:katl-vmtest:deployment/net-server",
		"condition:katl-vmtest:job/net-client:Complete",
	}) {
		t.Fatalf("usable fixture waits = %#v", got.Waits)
	}
	for _, path := range got.Manifests {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			t.Fatalf("usable fixture manifest %s stat = %v, info = %#v", path, err, info)
		}
	}
	cniConfig, err := os.ReadFile(filepath.Join(repo, "internal", "vmtest", "scenarios", "testdata", "bootstrap", "bridge-cni.conflist"))
	if err != nil {
		t.Fatalf("read test CNI fixture: %v", err)
	}
	if !strings.Contains(string(cniConfig), `"type": "bridge"`) || !strings.Contains(string(cniConfig), `"gateway": "__POD_GATEWAY__"`) {
		t.Fatalf("test CNI fixture must make the per-node pod gateway explicit:\n%s", cniConfig)
	}
}

func TestBootstrapFixtureInputsForRunStartsWithRepoFixtureAndAppendsEnv(t *testing.T) {
	repo := katlRepoRoot(t)
	t.Setenv("KATL_BOOTSTRAP_MANIFEST", "/tmp/extra.yaml")
	t.Setenv("KATL_BOOTSTRAP_WAIT", "resource-exists:katl-vmtest:job/net-client")

	got := bootstrapFixtureInputsForRun(repo)
	if len(got.Manifests) != 2 || got.Manifests[1] != "/tmp/extra.yaml" {
		t.Fatalf("fixture manifests = %#v", got.Manifests)
	}
	if !stringSlicesEqual(got.PreWaits, []string{"nodes-ready"}) {
		t.Fatalf("fixture pre-waits = %#v", got.PreWaits)
	}
	if len(got.Waits) != 3 || got.Waits[2] != "resource-exists:katl-vmtest:job/net-client" {
		t.Fatalf("fixture waits = %#v", got.Waits)
	}
	if !strings.Contains(got.Manifests[0], "testdata/bootstrap/cross-node-workload.yaml") {
		t.Fatalf("fixture does not start with repo workload manifest: %#v", got.Manifests)
	}
}

func TestStageBootstrapFixtureInputs(t *testing.T) {
	sourceDir := t.TempDir()
	manifestOne := filepath.Join(sourceDir, "01-cni.yaml")
	manifestTwo := filepath.Join(sourceDir, "02-workload.yaml")
	if err := os.WriteFile(manifestOne, []byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestTwo, []byte("kind: Job\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := stageBootstrapFixtureInputs(t.TempDir(), bootstrapFixtureInputs{
		Manifests: []string{manifestOne, manifestTwo},
		PreWaits:  []string{"nodes-ready"},
		Waits:     []string{"condition:default:job/smoke:Complete"},
	})
	if err != nil {
		t.Fatalf("stageBootstrapFixtureInputs() error = %v", err)
	}
	if len(got.Manifests) != 2 || filepath.Base(got.Manifests[0]) != "01-01-cni.yaml" || filepath.Base(got.Manifests[1]) != "02-02-workload.yaml" {
		t.Fatalf("staged manifests = %#v", got.Manifests)
	}
	if !stringSlicesEqual(got.PreWaits, []string{"nodes-ready"}) {
		t.Fatalf("staged pre-waits = %#v", got.PreWaits)
	}
	if !stringSlicesEqual(got.Waits, []string{"condition:default:job/smoke:Complete"}) {
		t.Fatalf("staged waits = %#v", got.Waits)
	}
	data, err := os.ReadFile(got.Manifests[1])
	if err != nil {
		t.Fatalf("read staged fixture: %v", err)
	}
	if string(data) != "kind: Job\n" {
		t.Fatalf("staged fixture content = %q", data)
	}
}

func TestAppendBootstrapFixtureArgs(t *testing.T) {
	got := appendBootstrapFixtureArgs([]string{"cluster", "bootstrap"}, bootstrapFixtureInputs{
		Manifests: []string{"/tmp/01-cni.yaml", "/tmp/02-workload.yaml"},
		PreWaits:  []string{"nodes-ready"},
		Waits:     []string{"pods-ready:kube-system:k8s-app=kube-dns", "nodes-ready"},
	})
	want := []string{
		"cluster", "bootstrap",
		"--bootstrap-manifest", "/tmp/01-cni.yaml",
		"--bootstrap-manifest", "/tmp/02-workload.yaml",
		"--bootstrap-pre-wait", "nodes-ready",
		"--bootstrap-wait", "pods-ready:kube-system:k8s-app=kube-dns",
		"--bootstrap-wait", "nodes-ready",
	}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("bootstrap args = %#v, want %#v", got, want)
	}
}

func stringSlicesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestKubectlDiagnosticPathsAndCommands(t *testing.T) {
	paths := kubectlDiagnosticPaths("/tmp/run")
	for name, want := range map[string]string{
		"clusterInfo":    "/tmp/run/kubectl-cluster-info.txt",
		"events":         "/tmp/run/kubectl-get-events.txt",
		"kubeSystemPods": "/tmp/run/kubectl-get-pods-kube-system.txt",
		"nodesWide":      "/tmp/run/kubectl-get-nodes-wide.txt",
		"workloadJob":    "/tmp/run/kubectl-get-job-net-client.txt",
		"workloadLogs":   "/tmp/run/kubectl-logs-net-client.txt",
		"workloadPods":   "/tmp/run/kubectl-get-pods-katl-vmtest.txt",
	} {
		if paths[name] != want {
			t.Fatalf("kubectl diagnostic path %s = %q, want %q in %#v", name, paths[name], want, paths)
		}
	}
	if got := kubectlDiagnosticPaths(""); got != nil {
		t.Fatalf("kubectlDiagnosticPaths(\"\") = %#v, want nil", got)
	}

	commands := kubectlDiagnosticCommands("/tmp/kubeconfig.yaml")
	if len(commands) != 7 {
		t.Fatalf("kubectl diagnostic command count = %d, want 7: %#v", len(commands), commands)
	}
	for _, command := range commands {
		if len(command.Argv) < 3 || command.Argv[0] != "kubectl" || command.Argv[1] != "--kubeconfig" || command.Argv[2] != "/tmp/kubeconfig.yaml" {
			t.Fatalf("kubectl diagnostic command %s argv = %#v", command.Name, command.Argv)
		}
	}
	if !kubectlDiagnosticCommandHasArgs(commands, "nodesWide", "get", "nodes", "-o", "wide") {
		t.Fatalf("nodesWide diagnostic command missing expected args: %#v", commands)
	}
	if !kubectlDiagnosticCommandHasArgs(commands, "kubeSystemPods", "-n", "kube-system", "get", "pods", "-o", "wide") {
		t.Fatalf("kubeSystemPods diagnostic command missing expected args: %#v", commands)
	}
	if !kubectlDiagnosticCommandHasArgs(commands, "workloadPods", "-n", "katl-vmtest", "get", "pods", "-o", "wide") {
		t.Fatalf("workloadPods diagnostic command missing expected args: %#v", commands)
	}
	if !kubectlDiagnosticCommandHasArgs(commands, "workloadJob", "-n", "katl-vmtest", "get", "job/net-client", "-o", "wide") {
		t.Fatalf("workloadJob diagnostic command missing expected args: %#v", commands)
	}
	if !kubectlDiagnosticCommandHasArgs(commands, "workloadLogs", "-n", "katl-vmtest", "logs", "-l", "app=net-client", "--all-containers=true", "--prefix") {
		t.Fatalf("workloadLogs diagnostic command missing expected args: %#v", commands)
	}
	if !kubectlDiagnosticCommandHasArgs(commands, "events", "get", "events", "-A", "--sort-by=.lastTimestamp") {
		t.Fatalf("events diagnostic command missing expected args: %#v", commands)
	}
	if !kubectlDiagnosticCommandHasArgs(commands, "clusterInfo", "cluster-info") {
		t.Fatalf("clusterInfo diagnostic command missing expected args: %#v", commands)
	}

	t.Setenv("KATL_VMTEST_KUBECTL", "/tmp/selected-kubectl")
	for _, command := range kubectlDiagnosticCommands("/tmp/kubeconfig.yaml") {
		if len(command.Argv) == 0 || command.Argv[0] != "/tmp/selected-kubectl" {
			t.Fatalf("kubectl diagnostic command %s argv = %#v, want selected kubectl", command.Name, command.Argv)
		}
	}
}

func TestCollectKubectlDiagnosticsIfKubeconfigExistsSkipsMissingKubeconfig(t *testing.T) {
	runDir := t.TempDir()
	if collectKubectlDiagnosticsIfKubeconfigExists("", runDir) {
		t.Fatalf("collectKubectlDiagnosticsIfKubeconfigExists() collected for empty kubeconfig")
	}
	if collectKubectlDiagnosticsIfKubeconfigExists(filepath.Join(runDir, "missing.yaml"), runDir) {
		t.Fatalf("collectKubectlDiagnosticsIfKubeconfigExists() collected for missing kubeconfig")
	}
	if _, err := os.Stat(filepath.Join(runDir, "kubectl-get-nodes-wide.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected kubectl diagnostics file for missing kubeconfig: %v", err)
	}
}

func kubectlDiagnosticCommandHasArgs(commands []kubectlDiagnosticCommand, name string, args ...string) bool {
	for _, command := range commands {
		if command.Name != name {
			continue
		}
		for i := 0; i <= len(command.Argv)-len(args); i++ {
			matched := true
			for j, want := range args {
				if command.Argv[i+j] != want {
					matched = false
					break
				}
			}
			if matched {
				return true
			}
		}
	}
	return false
}

func TestDiagnosticSummaryPaths(t *testing.T) {
	got := diagnosticSummaryPaths([]vmtest.RunningInstalledRuntimeNode{
		{Name: "cp-1", Result: vmtest.Result{Artifacts: vmtest.ArtifactPaths{GuestDir: "/tmp/cp-guest"}}},
		{Name: "worker-1", Result: vmtest.Result{Artifacts: vmtest.ArtifactPaths{GuestDir: "/tmp/worker-guest"}}},
		{Name: "ignored"},
	})
	if got["cp-1"] != "/tmp/cp-guest/diagnostics-summary.json" || got["worker-1"] != "/tmp/worker-guest/diagnostics-summary.json" {
		t.Fatalf("diagnostic summary paths = %#v", got)
	}
}

func TestSerialLogPaths(t *testing.T) {
	got := serialLogPaths([]vmtest.RunningInstalledRuntimeNode{
		{Name: "cp-1", Result: vmtest.Result{Artifacts: vmtest.ArtifactPaths{RuntimeSerial: "/tmp/cp-run/vm/runtime-serial.log"}}},
		{Name: "worker-1", Result: vmtest.Result{Artifacts: vmtest.ArtifactPaths{RuntimeSerial: "/tmp/worker-run/vm/runtime-serial.log"}}},
		{Name: "ignored"},
	})
	if got["cp-1"] != "/tmp/cp-run/vm/runtime-serial.log" || got["worker-1"] != "/tmp/worker-run/vm/runtime-serial.log" {
		t.Fatalf("serial log paths = %#v", got)
	}
}

func TestBootstrapDiagnosticsAreNodeAware(t *testing.T) {
	cp := bootstrapDiagnostics("cp-1")
	if !diagnosticCommand(cp, "etc-kubernetes-mount") || !diagnosticCommand(cp, "kubeadm-version") {
		t.Fatalf("control-plane diagnostics commands = %#v", cp.Commands)
	}
	for _, want := range []string{"crictl-pods", "network-addresses", "network-routes", "kube-proxy-helper-conntrack", "kube-proxy-helper-iptables-nft", "kube-proxy-helper-ipvsadm", "kube-proxy-helper-ipset", "kube-proxy-modules-loaded", "kube-proxy-ipvs-module", "kube-proxy-br-netfilter-module", "cni-fixture-config", "kubeadm-journal", "kubeadm-pki", "kubelet-state", "kubernetes-logs", "kubernetes-log-tail"} {
		if !diagnosticCommand(cp, want) {
			t.Fatalf("control-plane diagnostics commands = %#v, want %s", cp.Commands, want)
		}
	}
	if !diagnosticFile(cp, "admin-kubeconfig", "/etc/kubernetes/admin.conf") {
		t.Fatalf("control-plane diagnostics files = %#v, want admin kubeconfig", cp.Files)
	}
	if !diagnosticFile(cp, "kube-apiserver-manifest", "/etc/kubernetes/manifests/kube-apiserver.yaml") || !diagnosticFile(cp, "etcd-manifest", "/etc/kubernetes/manifests/etcd.yaml") {
		t.Fatalf("control-plane diagnostics files = %#v, want static pod manifests", cp.Files)
	}
	if !diagnosticJournalUnit(cp, "runtime-handoff", "katl-runtime-handoff-status.service") || !diagnosticJournalUnit(cp, "container-runtime", "containerd.service") || !diagnosticJournalUnit(cp, "katlc-agent", "katlc-agent.service") {
		t.Fatalf("control-plane diagnostics journals = %#v", cp.Journals)
	}

	joiningCP := bootstrapDiagnostics("cp-2")
	if !diagnosticFile(joiningCP, "kubeadm-config", "/etc/katl/kubeadm/control-plane/config.yaml") {
		t.Fatalf("joining control-plane diagnostics files = %#v, want control-plane kubeadm config", joiningCP.Files)
	}
	if !diagnosticFile(joiningCP, "admin-kubeconfig", "/etc/kubernetes/admin.conf") || !diagnosticFile(joiningCP, "etcd-manifest", "/etc/kubernetes/manifests/etcd.yaml") {
		t.Fatalf("joining control-plane diagnostics files = %#v, want control-plane artifacts", joiningCP.Files)
	}

	worker := bootstrapDiagnostics("worker-1")
	if !diagnosticFile(worker, "kubeadm-config", "/etc/katl/kubeadm/worker/config.yaml") {
		t.Fatalf("worker diagnostics files = %#v, want worker kubeadm config", worker.Files)
	}
	for _, want := range []string{"crictl-pods", "network-addresses", "network-routes", "kube-proxy-helper-conntrack", "kube-proxy-helper-iptables-nft", "kube-proxy-helper-ipvsadm", "kube-proxy-helper-ipset", "kube-proxy-modules-loaded", "kube-proxy-ipvs-module", "kube-proxy-br-netfilter-module", "cni-fixture-config", "kubeadm-journal", "kubeadm-pki", "kubelet-state", "kubernetes-logs", "kubernetes-log-tail"} {
		if !diagnosticCommand(worker, want) {
			t.Fatalf("worker diagnostics commands = %#v, want %s", worker.Commands, want)
		}
	}
	if diagnosticFile(worker, "admin-kubeconfig", "/etc/kubernetes/admin.conf") {
		t.Fatalf("worker diagnostics files = %#v, must not read control-plane admin kubeconfig", worker.Files)
	}
	if diagnosticFile(worker, "kube-apiserver-manifest", "/etc/kubernetes/manifests/kube-apiserver.yaml") {
		t.Fatalf("worker diagnostics files = %#v, must not expect control-plane static pods", worker.Files)
	}
}

func diagnosticCommand(plan vmtest.GuestDiagnostics, name string) bool {
	for _, command := range plan.Commands {
		if command.Name == name {
			return true
		}
	}
	return false
}

func diagnosticFile(plan vmtest.GuestDiagnostics, name, path string) bool {
	for _, file := range plan.Files {
		if file.Name == name && file.Path == path {
			return true
		}
	}
	return false
}

func diagnosticJournalUnit(plan vmtest.GuestDiagnostics, name, unit string) bool {
	for _, journal := range plan.Journals {
		if journal.Name != name {
			continue
		}
		for _, got := range journal.Units {
			if got == unit {
				return true
			}
		}
	}
	return false
}

func TestVerifyTwoNodeBootstrapTranscriptsChecksKubeadmRoles(t *testing.T) {
	dir := t.TempDir()
	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/var/lib/katl/test-artifacts/kubeadm-init-cp-1.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "token", "create", "--print-join-command"}, Redaction: "output", SensitiveOutput: true},
	})
	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "worker-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "WriteFile", Redaction: "sensitive", SensitiveOutput: true, WriteBytes: 256},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "--config", "/var/lib/katl/test-artifacts/kubeadm-join-worker-1.yaml"}, Redaction: "output", SensitiveOutput: true},
	})
	if err := verifyTwoNodeBootstrapTranscripts(dir); err != nil {
		t.Fatalf("verifyTwoNodeBootstrapTranscripts() error = %v", err)
	}

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "worker-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "WriteFile", Redaction: "sensitive", SensitiveOutput: true, WriteBytes: 256},
	})
	err := verifyTwoNodeBootstrapTranscripts(dir)
	if err == nil || !strings.Contains(err.Error(), "unexpected kubeadm init command on worker node") {
		t.Fatalf("verifyTwoNodeBootstrapTranscripts() error = %v, want worker init rejection", err)
	}

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "worker-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "WriteFile", Redaction: "sensitive", SensitiveOutput: true, WriteBytes: 256},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "--config", "/var/lib/katl/test-artifacts/kubeadm-join-worker-1.yaml", "--control-plane"}, Redaction: "output", SensitiveOutput: true},
	})
	err = verifyTwoNodeBootstrapTranscripts(dir)
	if err == nil || !strings.Contains(err.Error(), "worker kubeadm join command must not include --control-plane") {
		t.Fatalf("verifyTwoNodeBootstrapTranscripts() error = %v, want worker control-plane join rejection", err)
	}

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "worker-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "WriteFile", Redaction: "sensitive", SensitiveOutput: true, WriteBytes: 256},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "--config", "/var/lib/katl/test-artifacts/kubeadm-join-cp-1.yaml"}, Redaction: "output", SensitiveOutput: true},
	})
	err = verifyTwoNodeBootstrapTranscripts(dir)
	if err == nil || !strings.Contains(err.Error(), "worker kubeadm join command missing generated worker config path") {
		t.Fatalf("verifyTwoNodeBootstrapTranscripts() error = %v, want generated worker config path rejection", err)
	}
}

func writeTranscriptEntries(t *testing.T, path string, entries []transcriptEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	var data bytes.Buffer
	for _, entry := range entries {
		if err := json.NewEncoder(&data).Encode(entry); err != nil {
			t.Fatalf("encode transcript entry: %v", err)
		}
	}
	if err := os.WriteFile(path, data.Bytes(), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}
