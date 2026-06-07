package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/bootstrap/cluster"
	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/bootstrap/readiness"
	"github.com/zariel/katl/internal/vmtest"
	vmtestpb "github.com/zariel/katl/internal/vmtest/proto"
)

func TestInstalledRuntimeThreeControlPlaneStackedEtcdSmoke(t *testing.T) {
	if run, ok := threeControlPlaneWorldSmokeRun(t); ok {
		runThreeControlPlaneStackedEtcdSmoke(t, run)
		return
	}

	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run three-control-plane stacked-etcd smoke")
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
	Bridge        string
}

func threeControlPlaneWorldSmokeRun(t *testing.T) (threeControlPlaneSmokeRun, bool) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(vmtest.WorldManifestEnv)) == "" {
		return threeControlPlaneSmokeRun{}, false
	}
	world := vmtest.RequireWorld(t)
	repo := katlRepoRoot(t)
	kvm := vmtest.DefaultOptions().KVM
	if err := ensurePublishedRuntimeFixturesForWorld(world, repo, threeControlPlaneWorldRuntimeSpecs(), kvm); err != nil {
		failWorldFixtureSetup(t, world, "installed-runtime-three-control-plane-stacked-etcd", err)
	}
	run, err := planThreeControlPlaneWorldSmokeRun(world, repo, firstString(os.Getenv("KATL_KUBERNETES_VERSION"), "v1.36.1"), kvm)
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

func planThreeControlPlaneWorldSmokeRun(world vmtest.World, repo, kubernetesVersion string, kvm vmtest.KVMPolicy) (threeControlPlaneSmokeRun, error) {
	scenario, err := world.PlanScenario("installed-runtime-three-control-plane-stacked-etcd")
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
	vmScenario := vmtest.Scenario{Name: "installed-runtime-three-control-plane-stacked-etcd"}
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
		Bridge:        world.Network.Bridge,
		Inputs: threeControlPlaneSmokeInputs{
			CP1Disk:           nodes["cp-1"].Config.Disk,
			CP1DiskFormat:     string(nodes["cp-1"].Config.DiskFormat),
			CP1ESP:            nodes["cp-1"].Config.ESPArtifacts,
			CP1Fixture:        nodes["cp-1"].Config.FixtureManifest,
			CP1Metadata:       nodes["cp-1"].Config.NodeMetadata,
			CP1Address:        nodes["cp-1"].Node.Address,
			CP2Disk:           nodes["cp-2"].Config.Disk,
			CP2DiskFormat:     string(nodes["cp-2"].Config.DiskFormat),
			CP2ESP:            nodes["cp-2"].Config.ESPArtifacts,
			CP2Fixture:        nodes["cp-2"].Config.FixtureManifest,
			CP2Metadata:       nodes["cp-2"].Config.NodeMetadata,
			CP2Address:        nodes["cp-2"].Node.Address,
			CP3Disk:           nodes["cp-3"].Config.Disk,
			CP3DiskFormat:     string(nodes["cp-3"].Config.DiskFormat),
			CP3ESP:            nodes["cp-3"].Config.ESPArtifacts,
			CP3Fixture:        nodes["cp-3"].Config.FixtureManifest,
			CP3Metadata:       nodes["cp-3"].Config.NodeMetadata,
			CP3Address:        nodes["cp-3"].Node.Address,
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
		QEMU:         true,
		OVMF:         true,
		KVM:          options.KVM,
		SharedBridge: true,
		Bridge:       smoke.Bridge,
	})
	transcriptDir := filepath.Join(result.RunDir, "agent-transcripts")
	etcdTranscriptDir := filepath.Join(result.RunDir, "etcd-transcripts")
	inventoryPath := filepath.Join(result.ManifestDir, "bootstrap-inventory.yaml")
	kubeconfigPath := filepath.Join(result.RunDir, "operator-kubeconfig.yaml")
	kubeconfigMetadataPath := filepath.Join(result.RunDir, "operator-kubeconfig-metadata.json")
	stdoutPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stdout")
	stderrPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stderr")
	kubectlOut := filepath.Join(result.RunDir, "kubectl-get-nodes.txt")
	etcdReportPath := filepath.Join(result.RunDir, "etcd-report.json")
	bootstrapFixture := bootstrapFixtureInputsFromEnv()
	plannedNodes := make([]vmtest.RunningInstalledRuntimeNode, 0, 3)
	for _, name := range []string{"cp-1", "cp-2", "cp-3"} {
		nodeResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, name)
		if err != nil {
			t.Fatal(err)
		}
		plannedNodes = append(plannedNodes, vmtest.RunningInstalledRuntimeNode{Name: name, Result: nodeResult})
	}
	if err := writeThreeControlPlaneSmokeArtifactManifest(result, inputs, transcriptDir, etcdTranscriptDir, plannedNodes, bootstrapFixture); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	cp1Node, err := vmtest.StartInstalledRuntimeNode(ctx, result, threeControlPlaneNodeConfigForRun(smoke, "cp-1", inputs.CP1Disk, inputs.CP1ESP, inputs.CP1Fixture, inputs.CP1Metadata, vmtest.DiskFormat(inputs.CP1DiskFormat), 43201), vmtest.VMRunner{})
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start cp-1 VM: %v", err)
	}
	defer stopNode(t, cp1Node)

	cp2Node, err := vmtest.StartInstalledRuntimeNode(ctx, result, threeControlPlaneNodeConfigForRun(smoke, "cp-2", inputs.CP2Disk, inputs.CP2ESP, inputs.CP2Fixture, inputs.CP2Metadata, vmtest.DiskFormat(inputs.CP2DiskFormat), 43202), vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics(transcriptDir, cp1Node)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start cp-2 VM: %v", err)
	}
	defer stopNode(t, cp2Node)

	cp3Node, err := vmtest.StartInstalledRuntimeNode(ctx, result, threeControlPlaneNodeConfigForRun(smoke, "cp-3", inputs.CP3Disk, inputs.CP3ESP, inputs.CP3Fixture, inputs.CP3Metadata, vmtest.DiskFormat(inputs.CP3DiskFormat), 43203), vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics(transcriptDir, cp1Node, cp2Node)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start cp-3 VM: %v", err)
	}
	defer stopNode(t, cp3Node)

	nodes := []vmtest.RunningInstalledRuntimeNode{cp1Node, cp2Node, cp3Node}
	if err := writeThreeControlPlaneInventory(inventoryPath, inputs.KubernetesVersion, nodes); err != nil {
		t.Fatal(err)
	}
	if err := writeThreeControlPlaneSmokeArtifactManifest(result, inputs, transcriptDir, etcdTranscriptDir, nodes, bootstrapFixture); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err = runKatlctlCommand(t, ctx, katlRepoRoot(t), appendBootstrapFixtureArgs([]string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--init-node", "cp-1",
		"--control-plane-endpoint", inputs.CP1Address + ":6443",
		"--node-address", "cp-1=" + inputs.CP1Address,
		"--node-address", "cp-2=" + inputs.CP2Address,
		"--node-address", "cp-3=" + inputs.CP3Address,
		"--kubeconfig-out", kubeconfigPath,
		"--overwrite-kubeconfig",
		"--vmtest-transcript-dir", transcriptDir,
	}, bootstrapFixture), &stdout, &stderr)
	_ = os.WriteFile(stdoutPath, stdout.Bytes(), 0o644)
	_ = os.WriteFile(stderrPath, stderr.Bytes(), 0o644)
	_ = writeKubeconfigMetadata(kubeconfigPath, kubeconfigMetadataPath)
	if err != nil {
		collectKubectlDiagnosticsIfKubeconfigExists(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics(transcriptDir, nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("katlctl cluster bootstrap failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	assertThreeControlPlaneBootstrapPhases(t, stdout.String())
	if err := verifyBootstrapTranscripts(transcriptDir, []string{"cp-1", "cp-2", "cp-3"}); err != nil {
		collectTwoNodeDiagnostics(transcriptDir, nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("bootstrap transcripts: %v", err)
	}

	cmd := exec.CommandContext(ctx, selectedKubectl(), "--kubeconfig", kubeconfigPath, "get", "nodes", "-o", "name")
	output, err := cmd.CombinedOutput()
	_ = os.WriteFile(kubectlOut, output, 0o644)
	if err != nil {
		collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics(transcriptDir, nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("kubectl get nodes failed: %v\n%s", err, output)
	}
	for _, want := range []string{"node/cp-1", "node/cp-2", "node/cp-3"} {
		if !strings.Contains(string(output), want) {
			collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
			collectTwoNodeDiagnostics(transcriptDir, nodes...)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, "kubectl output missing "+want)
			t.Fatalf("kubectl output missing %q:\n%s", want, output)
		}
	}
	collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
	etcdReport, err := verifyThreeControlPlaneEtcd(ctx, etcdTranscriptDir, nodes)
	if err != nil {
		etcdReport.FailureSummary = err.Error()
		if writeErr := writeThreeControlPlaneEtcdReport(etcdReportPath, etcdReport); writeErr != nil {
			t.Fatalf("write failed etcd report: %v; original error: %v", writeErr, err)
		}
		collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics(transcriptDir, nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("verify stacked etcd: %v", err)
	}
	if err := writeThreeControlPlaneEtcdReport(etcdReportPath, etcdReport); err != nil {
		t.Fatalf("write etcd report: %v", err)
	}
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusPassed, "")
}

type threeControlPlaneSmokeInputs struct {
	CP1Disk           string
	CP1DiskFormat     string
	CP1ESP            string
	CP1Fixture        string
	CP1Metadata       string
	CP1Address        string
	CP2Disk           string
	CP2DiskFormat     string
	CP2ESP            string
	CP2Fixture        string
	CP2Metadata       string
	CP2Address        string
	CP3Disk           string
	CP3DiskFormat     string
	CP3ESP            string
	CP3Fixture        string
	CP3Metadata       string
	CP3Address        string
	KubernetesVersion string
	WorldProvenance   multiNodeWorldProvenancePaths
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

func threeControlPlaneNodeConfigForRun(run threeControlPlaneSmokeRun, name, disk, esp, fixtureManifest, nodeMetadata string, format vmtest.DiskFormat, cid uint32) vmtest.InstalledRuntimeNodeConfig {
	config := threeControlPlaneNodeConfig(name, disk, esp, fixtureManifest, nodeMetadata, format, run.Options.KVM, cid)
	config.Runtime.VM.Network.Bridge = run.Bridge
	return config
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

type threeControlPlaneArtifactManifest struct {
	VMTestRun                string                      `json:"vmtestRun,omitempty"`
	WorldManifest            string                      `json:"worldManifest,omitempty"`
	HostCapabilities         string                      `json:"hostCapabilities,omitempty"`
	MkosiArtifactIndex       string                      `json:"mkosiArtifactIndex,omitempty"`
	NodeRunDirs              map[string]string           `json:"nodeRunDirs"`
	NodeScenarios            map[string]string           `json:"nodeScenarios,omitempty"`
	NodeResults              map[string]string           `json:"nodeResults,omitempty"`
	LaunchCommands           map[string]string           `json:"launchCommands,omitempty"`
	DomainXMLs               map[string]string           `json:"domainXMLs,omitempty"`
	InstalledRuntimeInputs   map[string]string           `json:"installedRuntimeInputs,omitempty"`
	VSockTranscripts         map[string]string           `json:"vsockTranscripts,omitempty"`
	FixtureInputs            map[string]nodeFixtureInput `json:"fixtureInputs,omitempty"`
	FixtureProducerScenarios map[string]string           `json:"fixtureProducerScenarios,omitempty"`
	FixtureProducerResults   map[string]string           `json:"fixtureProducerResults,omitempty"`
	Inventory                string                      `json:"inventory"`
	Kubeconfig               string                      `json:"kubeconfig"`
	KubeconfigMetadata       string                      `json:"kubeconfigMetadata,omitempty"`
	BootstrapStdout          string                      `json:"bootstrapStdout"`
	BootstrapStderr          string                      `json:"bootstrapStderr"`
	BootstrapFixture         *bootstrapFixtureInputs     `json:"bootstrapFixture,omitempty"`
	KubectlOutput            string                      `json:"kubectlOutput"`
	KubectlDiagnostics       map[string]string           `json:"kubectlDiagnostics,omitempty"`
	EtcdReport               string                      `json:"etcdReport"`
	Transcripts              map[string]string           `json:"transcripts"`
	EtcdTranscripts          map[string]string           `json:"etcdTranscripts"`
	SerialLogs               map[string]string           `json:"serialLogs,omitempty"`
	Diagnostics              map[string]string           `json:"diagnostics,omitempty"`
}

func writeThreeControlPlaneSmokeArtifactManifest(result vmtest.Result, inputs threeControlPlaneSmokeInputs, transcriptDir, etcdTranscriptDir string, nodes []vmtest.RunningInstalledRuntimeNode, bootstrapFixture bootstrapFixtureInputs) error {
	return writeThreeControlPlaneArtifactManifest(filepath.Join(result.ManifestDir, "three-control-plane-artifacts.json"), threeControlPlaneArtifactManifest{
		VMTestRun:                inputs.WorldProvenance.VMTestRun,
		WorldManifest:            inputs.WorldProvenance.WorldManifest,
		HostCapabilities:         inputs.WorldProvenance.HostCapabilities,
		MkosiArtifactIndex:       inputs.WorldProvenance.MkosiArtifactIndex,
		NodeRunDirs:              nodeRunDirs(nodes),
		NodeScenarios:            nodeScenarioPaths(nodes),
		NodeResults:              nodeResultPaths(nodes),
		LaunchCommands:           launchCommandPaths(nodes),
		DomainXMLs:               domainXMLPaths(nodes),
		InstalledRuntimeInputs:   installedRuntimeInputPaths(nodes),
		VSockTranscripts:         vsockTranscriptPaths(nodes),
		FixtureInputs:            threeControlPlaneFixtureInputs(inputs.CP1Disk, inputs.CP1DiskFormat, inputs.CP2Disk, inputs.CP2DiskFormat, inputs.CP3Disk, inputs.CP3DiskFormat, inputs.CP1ESP, inputs.CP2ESP, inputs.CP3ESP, inputs.CP1Fixture, inputs.CP2Fixture, inputs.CP3Fixture, inputs.CP1Metadata, inputs.CP2Metadata, inputs.CP3Metadata),
		FixtureProducerScenarios: inputs.WorldProvenance.FixtureProducerScenarios,
		FixtureProducerResults:   inputs.WorldProvenance.FixtureProducerResults,
		Inventory:                filepath.Join(result.ManifestDir, "bootstrap-inventory.yaml"),
		Kubeconfig:               filepath.Join(result.RunDir, "operator-kubeconfig.yaml"),
		KubeconfigMetadata:       filepath.Join(result.RunDir, "operator-kubeconfig-metadata.json"),
		BootstrapStdout:          filepath.Join(result.RunDir, "katlctl-bootstrap.stdout"),
		BootstrapStderr:          filepath.Join(result.RunDir, "katlctl-bootstrap.stderr"),
		BootstrapFixture:         bootstrapFixture.manifestValue(),
		KubectlOutput:            filepath.Join(result.RunDir, "kubectl-get-nodes.txt"),
		KubectlDiagnostics:       kubectlDiagnosticPaths(result.RunDir),
		EtcdReport:               filepath.Join(result.RunDir, "etcd-report.json"),
		Transcripts:              transcriptPaths(transcriptDir, nodes),
		EtcdTranscripts:          transcriptPaths(etcdTranscriptDir, nodes),
		SerialLogs:               serialLogPaths(nodes),
		Diagnostics:              diagnosticSummaryPaths(nodes),
	})
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
		"phase=kubeadm-init node=cp-1 status=passed",
		"phase=control-plane-join-material node=cp-1 status=passed",
		"phase=control-plane-join node=cp-2 status=passed",
		"phase=control-plane-ready node=cp-2 status=passed",
		"phase=control-plane-join node=cp-3 status=passed",
		"phase=control-plane-ready node=cp-3 status=passed",
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
		var runCommand, readFile, sensitiveCommand, sensitiveFile bool
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
		if !transcriptHasCommandFlagValue(entries, "kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml") {
			return errors.New("kubeadm init command missing control-plane config path")
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
		if !transcriptHasCommandFlagValue(entries, "kubeadm", "join", "--config", "/etc/katl/kubeadm/control-plane/config.yaml") {
			return errors.New("kubeadm control-plane join command missing control-plane config path")
		}
		if !transcriptHasCommandArg(entries, "kubeadm", "join", "--control-plane") {
			return errors.New("kubeadm join command missing --control-plane")
		}
		if !transcriptHasCommandArg(entries, "kubeadm", "join", "--certificate-key") {
			return errors.New("kubeadm control-plane join command missing --certificate-key")
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
	snapshot, err := checker.CreateSnapshot(ctx, planned["cp-1"], "/var/lib/etcd/katl-snapshots/three-control-plane.db")
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
	client, err := t.client(ctx, node)
	if err != nil {
		return readiness.CommandResult{}, err
	}
	defer client.Close()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	result, err := client.RunCommand(ctx, &vmtestpb.RunCommandRequest{
		Argv:             req.Argv,
		StdoutLimit:      req.StdoutLimit,
		StderrLimit:      req.StderrLimit,
		SensitiveOutput:  req.SensitiveOutput,
		WorkingDirectory: "",
	})
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
	client, err := t.client(ctx, node)
	if err != nil {
		return readiness.FileResult{}, err
	}
	defer client.Close()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	result, err := client.ReadFile(ctx, &vmtestpb.ReadFileRequest{
		Path:      req.Path,
		MaxBytes:  req.MaxBytes,
		Sensitive: req.Sensitive,
	})
	if err != nil {
		return readiness.FileResult{}, err
	}
	return readiness.FileResult{Content: result.Content, Truncated: result.Truncated, Redaction: result.Redaction}, nil
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
		nodes = append(nodes, vmtest.RunningInstalledRuntimeNode{Name: name, Result: nodeResult})
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
			FixtureProducerScenarios: map[string]string{"cp-2": "/tmp/fixture-cp-2/scenario.json"},
			FixtureProducerResults:   map[string]string{"cp-3": "/tmp/fixture-cp-3/result.json"},
		},
	}, filepath.Join(result.RunDir, "agent-transcripts"), filepath.Join(result.RunDir, "etcd-transcripts"), nodes, bootstrapFixtureInputs{}); err != nil {
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
	if manifest.EtcdTranscripts["cp-2"] != twoNodeBootstrapTranscriptPath(filepath.Join(result.RunDir, "etcd-transcripts"), "cp-2") {
		t.Fatalf("etcd transcripts = %#v", manifest.EtcdTranscripts)
	}
	if manifest.WorldManifest != "/tmp/world.json" || manifest.FixtureProducerResults["cp-3"] != "/tmp/fixture-cp-3/result.json" {
		t.Fatalf("planned provenance = %#v", manifest)
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
	run, err := planThreeControlPlaneWorldSmokeRun(world, t.TempDir(), "v1.36.1", vmtest.KVMOff)
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
		writeKatlctlPublishedInstalledRuntimeFixture(t, repo, "repo-"+name, name, vmtest.ControlPlane)
		writeKatlctlPublishedInstalledRuntimeFixture(t, world.RunDir, "world-"+name, name, vmtest.ControlPlane)
	}

	run, err := planThreeControlPlaneWorldSmokeRun(world, repo, "v1.36.1", vmtest.KVMOff)
	if err != nil {
		t.Fatalf("planThreeControlPlaneWorldSmokeRun() error = %v", err)
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
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "phase", "upload-certs", "--upload-certs"}, Redaction: "output", SensitiveOutput: true},
	})
	for _, node := range []string{"cp-2", "cp-3"} {
		writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, node), []transcriptEntry{
			{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
			{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
			{Method: "RunCommand", Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "[REDACTED BOOTSTRAP TOKEN]", "--control-plane", "--certificate-key", "[REDACTED CERTIFICATE KEY]", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
		})
	}
	if err := verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"}); err != nil {
		t.Fatalf("verifyBootstrapTranscripts() error = %v", err)
	}

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
	})
	err := verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"})
	if err == nil || !strings.Contains(err.Error(), "missing kubeadm certificate upload command") {
		t.Fatalf("verifyBootstrapTranscripts() error = %v, want certificate upload rejection", err)
	}
	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "phase", "upload-certs", "--upload-certs"}, Redaction: "output", SensitiveOutput: true},
	})

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-2"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "[REDACTED BOOTSTRAP TOKEN]", "--control-plane", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
	})
	err = verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"})
	if err == nil || !strings.Contains(err.Error(), "kubeadm control-plane join command missing --certificate-key") {
		t.Fatalf("verifyBootstrapTranscripts() error = %v, want certificate-key rejection", err)
	}
	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-2"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "[REDACTED BOOTSTRAP TOKEN]", "--control-plane", "--certificate-key", "[REDACTED CERTIFICATE KEY]", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
	})

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-2"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
	})
	err = verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"})
	if err == nil || !strings.Contains(err.Error(), "unexpected kubeadm init command on joining control-plane") {
		t.Fatalf("verifyBootstrapTranscripts() error = %v, want cp-2 init rejection", err)
	}

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-2"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "[REDACTED BOOTSTRAP TOKEN]", "--control-plane", "--certificate-key", "[REDACTED CERTIFICATE KEY]", "--config", "/etc/katl/kubeadm/worker/config.yaml"}, Redaction: "output", SensitiveOutput: true},
	})
	err = verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"})
	if err == nil || !strings.Contains(err.Error(), "kubeadm control-plane join command missing control-plane config path") {
		t.Fatalf("verifyBootstrapTranscripts() error = %v, want cp-2 config path rejection", err)
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
