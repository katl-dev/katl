package main

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
	options := vmtest.DefaultOptions()
	options.Missing = vmtest.MissingSkips
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run three-control-plane stacked-etcd smoke")
	}
	cp1Disk := requireFirstEnv(t, "KATL_CONTROL_PLANE_1_INSTALLED_DISK", "KATL_CONTROL_PLANE_INSTALLED_DISK")
	cp2Disk := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_2_INSTALLED_DISK")
	cp3Disk := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_3_INSTALLED_DISK")
	cp1ESP := requireNodeESP(t, firstSetEnv("KATL_CONTROL_PLANE_1_INSTALLED_ESP_ARTIFACTS", "KATL_CONTROL_PLANE_INSTALLED_ESP_ARTIFACTS"))
	cp2ESP := requireNodeESP(t, "KATL_CONTROL_PLANE_2_INSTALLED_ESP_ARTIFACTS")
	cp3ESP := requireNodeESP(t, "KATL_CONTROL_PLANE_3_INSTALLED_ESP_ARTIFACTS")
	cp1Fixture := requireFirstEnv(t, "KATL_CONTROL_PLANE_1_FIXTURE_MANIFEST", "KATL_CONTROL_PLANE_FIXTURE_MANIFEST")
	cp2Fixture := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_2_FIXTURE_MANIFEST")
	cp3Fixture := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_3_FIXTURE_MANIFEST")
	cp1Metadata := requireFirstEnv(t, "KATL_CONTROL_PLANE_1_NODE_METADATA", "KATL_CONTROL_PLANE_NODE_METADATA")
	cp2Metadata := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_2_NODE_METADATA")
	cp3Metadata := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_3_NODE_METADATA")
	cp1Address := requireFirstEnv(t, "KATL_CONTROL_PLANE_1_ADDRESS", "KATL_CONTROL_PLANE_ADDRESS")
	cp2Address := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_2_ADDRESS")
	cp3Address := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_3_ADDRESS")
	kubernetesVersion := firstString(os.Getenv("KATL_KUBERNETES_VERSION"), "v1.36.1")
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skipf("kubectl is required for host-side kubeconfig verification: %v", err)
	}

	runner := vmtest.NewRunner(options)
	runner.RequireHost(t, vmtest.HostRequirements{
		QEMU:         true,
		OVMF:         true,
		KVM:          options.KVM,
		SharedBridge: true,
	})
	scenario := vmtest.Scenario{Name: "installed-runtime-three-control-plane-stacked-etcd"}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result.Started = time.Now().UTC()
	transcriptDir := filepath.Join(result.RunDir, "agent-transcripts")
	etcdTranscriptDir := filepath.Join(result.RunDir, "etcd-transcripts")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	cp1Node, err := vmtest.StartInstalledRuntimeNode(ctx, result, threeControlPlaneNodeConfig("cp-1", cp1Disk, cp1ESP, cp1Fixture, cp1Metadata, diskFormatEnv("KATL_CONTROL_PLANE_1_INSTALLED_DISK_FORMAT", "KATL_CONTROL_PLANE_INSTALLED_DISK_FORMAT"), options.KVM, 43201), vmtest.VMRunner{})
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start cp-1 VM: %v", err)
	}
	defer stopNode(t, cp1Node)

	cp2Node, err := vmtest.StartInstalledRuntimeNode(ctx, result, threeControlPlaneNodeConfig("cp-2", cp2Disk, cp2ESP, cp2Fixture, cp2Metadata, diskFormatEnv("KATL_CONTROL_PLANE_2_INSTALLED_DISK_FORMAT"), options.KVM, 43202), vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics(transcriptDir, cp1Node)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start cp-2 VM: %v", err)
	}
	defer stopNode(t, cp2Node)

	cp3Node, err := vmtest.StartInstalledRuntimeNode(ctx, result, threeControlPlaneNodeConfig("cp-3", cp3Disk, cp3ESP, cp3Fixture, cp3Metadata, diskFormatEnv("KATL_CONTROL_PLANE_3_INSTALLED_DISK_FORMAT"), options.KVM, 43203), vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics(transcriptDir, cp1Node, cp2Node)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start cp-3 VM: %v", err)
	}
	defer stopNode(t, cp3Node)

	nodes := []vmtest.RunningInstalledRuntimeNode{cp1Node, cp2Node, cp3Node}
	inventoryPath := filepath.Join(result.ManifestDir, "bootstrap-inventory.yaml")
	kubeconfigPath := filepath.Join(result.RunDir, "operator-kubeconfig.yaml")
	stdoutPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stdout")
	stderrPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stderr")
	kubectlOut := filepath.Join(result.RunDir, "kubectl-get-nodes.txt")
	etcdReportPath := filepath.Join(result.RunDir, "etcd-report.json")
	if err := writeThreeControlPlaneInventory(inventoryPath, kubernetesVersion, nodes); err != nil {
		t.Fatal(err)
	}
	if err := writeThreeControlPlaneArtifactManifest(filepath.Join(result.ManifestDir, "three-control-plane-artifacts.json"), threeControlPlaneArtifactManifest{
		NodeRunDirs:        nodeRunDirs(nodes),
		FixtureInputs:      threeControlPlaneFixtureInputs(cp1Disk, cp2Disk, cp3Disk, cp1ESP, cp2ESP, cp3ESP, cp1Fixture, cp2Fixture, cp3Fixture, cp1Metadata, cp2Metadata, cp3Metadata),
		PublishedFixtures:  threeControlPlanePublishedFixtureDirs(),
		Inventory:          inventoryPath,
		Kubeconfig:         kubeconfigPath,
		BootstrapStdout:    stdoutPath,
		BootstrapStderr:    stderrPath,
		KubectlOutput:      kubectlOut,
		KubectlDiagnostics: kubectlDiagnosticPaths(result.RunDir),
		EtcdReport:         etcdReportPath,
		Transcripts:        transcriptPaths(transcriptDir, nodes),
		EtcdTranscripts:    transcriptPaths(etcdTranscriptDir, nodes),
		Diagnostics:        diagnosticSummaryPaths(nodes),
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--init-node", "cp-1",
		"--control-plane-endpoint", cp1Address + ":6443",
		"--node-address", "cp-1=" + cp1Address,
		"--node-address", "cp-2=" + cp2Address,
		"--node-address", "cp-3=" + cp3Address,
		"--kubeconfig-out", kubeconfigPath,
		"--overwrite-kubeconfig",
		"--vmtest-transcript-dir", transcriptDir,
	}, &stdout, &stderr)
	_ = os.WriteFile(stdoutPath, stdout.Bytes(), 0o644)
	_ = os.WriteFile(stderrPath, stderr.Bytes(), 0o644)
	if err != nil {
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

	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfigPath, "get", "nodes", "-o", "name")
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
	etcdReport, err := verifyThreeControlPlaneEtcd(ctx, etcdTranscriptDir, nodes)
	if err != nil {
		collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics(transcriptDir, nodes...)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("verify stacked etcd: %v", err)
	}
	if err := writeTwoNodeDiagnosticJSON(etcdReportPath, etcdReport); err != nil {
		t.Fatalf("write etcd report: %v", err)
	}
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusPassed, "")
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
	NodeRunDirs        map[string]string           `json:"nodeRunDirs"`
	FixtureInputs      map[string]nodeFixtureInput `json:"fixtureInputs,omitempty"`
	PublishedFixtures  map[string]string           `json:"publishedFixtures,omitempty"`
	Inventory          string                      `json:"inventory"`
	Kubeconfig         string                      `json:"kubeconfig"`
	BootstrapStdout    string                      `json:"bootstrapStdout"`
	BootstrapStderr    string                      `json:"bootstrapStderr"`
	KubectlOutput      string                      `json:"kubectlOutput"`
	KubectlDiagnostics map[string]string           `json:"kubectlDiagnostics,omitempty"`
	EtcdReport         string                      `json:"etcdReport"`
	Transcripts        map[string]string           `json:"transcripts"`
	EtcdTranscripts    map[string]string           `json:"etcdTranscripts"`
	Diagnostics        map[string]string           `json:"diagnostics,omitempty"`
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

func threeControlPlanePublishedFixtureDirs() map[string]string {
	return compactStringMap(map[string]string{
		"cp-1": firstString(os.Getenv("KATL_CONTROL_PLANE_1_PUBLISHED_FIXTURE_DIR"), os.Getenv("KATL_CONTROL_PLANE_PUBLISHED_FIXTURE_DIR")),
		"cp-2": os.Getenv("KATL_CONTROL_PLANE_2_PUBLISHED_FIXTURE_DIR"),
		"cp-3": os.Getenv("KATL_CONTROL_PLANE_3_PUBLISHED_FIXTURE_DIR"),
	})
}

func threeControlPlaneFixtureInputs(cp1Disk, cp2Disk, cp3Disk, cp1ESP, cp2ESP, cp3ESP, cp1Fixture, cp2Fixture, cp3Fixture, cp1Metadata, cp2Metadata, cp3Metadata string) map[string]nodeFixtureInput {
	return map[string]nodeFixtureInput{
		"cp-1": fixtureInput(cp1Disk, firstString(os.Getenv("KATL_CONTROL_PLANE_1_INSTALLED_DISK_FORMAT"), os.Getenv("KATL_CONTROL_PLANE_INSTALLED_DISK_FORMAT"), string(vmtest.DiskRaw)), cp1ESP, cp1Fixture, cp1Metadata, firstString(os.Getenv("KATL_CONTROL_PLANE_1_PUBLISHED_FIXTURE_DIR"), os.Getenv("KATL_CONTROL_PLANE_PUBLISHED_FIXTURE_DIR")), firstString(os.Getenv("KATL_CONTROL_PLANE_1_KATLOS_FIXTURE_MANIFEST"), os.Getenv("KATL_CONTROL_PLANE_KATLOS_FIXTURE_MANIFEST"))),
		"cp-2": fixtureInput(cp2Disk, firstString(os.Getenv("KATL_CONTROL_PLANE_2_INSTALLED_DISK_FORMAT"), string(vmtest.DiskRaw)), cp2ESP, cp2Fixture, cp2Metadata, os.Getenv("KATL_CONTROL_PLANE_2_PUBLISHED_FIXTURE_DIR"), os.Getenv("KATL_CONTROL_PLANE_2_KATLOS_FIXTURE_MANIFEST")),
		"cp-3": fixtureInput(cp3Disk, firstString(os.Getenv("KATL_CONTROL_PLANE_3_INSTALLED_DISK_FORMAT"), string(vmtest.DiskRaw)), cp3ESP, cp3Fixture, cp3Metadata, os.Getenv("KATL_CONTROL_PLANE_3_PUBLISHED_FIXTURE_DIR"), os.Getenv("KATL_CONTROL_PLANE_3_KATLOS_FIXTURE_MANIFEST")),
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

type threeControlPlaneEtcdReport struct {
	StaticPods []controlPlaneStaticPodReport `json:"staticPods"`
	Health     cluster.EtcdReport            `json:"health"`
	Snapshot   cluster.EtcdSnapshotReport    `json:"snapshot"`
	Transcript string                        `json:"transcript"`
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
		return threeControlPlaneEtcdReport{}, err
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

func requireFirstEnv(t *testing.T, names ...string) string {
	t.Helper()
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	t.Skipf("set one of %s to run this VM scenario", strings.Join(names, ", "))
	return ""
}

func firstSetEnv(names ...string) string {
	for _, name := range names {
		if os.Getenv(name) != "" {
			return name
		}
	}
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func diskFormatEnv(names ...string) vmtest.DiskFormat {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return vmtest.DiskFormat(value)
		}
	}
	return vmtest.DiskRaw
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

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-2"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
	})
	err := verifyBootstrapTranscripts(dir, []string{"cp-1", "cp-2", "cp-3"})
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

func TestThreeControlPlanePublishedFixtureDirs(t *testing.T) {
	t.Setenv("KATL_CONTROL_PLANE_1_PUBLISHED_FIXTURE_DIR", "/tmp/cp-1")
	t.Setenv("KATL_CONTROL_PLANE_2_PUBLISHED_FIXTURE_DIR", "/tmp/cp-2")
	t.Setenv("KATL_CONTROL_PLANE_3_PUBLISHED_FIXTURE_DIR", "/tmp/cp-3")
	t.Setenv("KATL_CONTROL_PLANE_1_KATLOS_FIXTURE_MANIFEST", "/tmp/cp-1-katlos.json")
	t.Setenv("KATL_CONTROL_PLANE_2_KATLOS_FIXTURE_MANIFEST", "/tmp/cp-2-katlos.json")
	t.Setenv("KATL_CONTROL_PLANE_3_KATLOS_FIXTURE_MANIFEST", "/tmp/cp-3-katlos.json")
	t.Setenv("KATL_CONTROL_PLANE_2_INSTALLED_DISK_FORMAT", "qcow2")
	got := threeControlPlanePublishedFixtureDirs()
	if got["cp-1"] != "/tmp/cp-1" || got["cp-2"] != "/tmp/cp-2" || got["cp-3"] != "/tmp/cp-3" {
		t.Fatalf("published fixtures = %#v", got)
	}
	inputs := threeControlPlaneFixtureInputs("cp1.raw", "cp2.qcow2", "cp3.raw", "cp1-esp", "cp2-esp", "cp3-esp", "cp1-fixture.json", "cp2-fixture.json", "cp3-fixture.json", "cp1-node.json", "cp2-node.json", "cp3-node.json")
	if inputs["cp-2"].DiskFormat != "qcow2" || inputs["cp-3"].DiskFormat != string(vmtest.DiskRaw) {
		t.Fatalf("fixture input formats = %#v", inputs)
	}
	path := filepath.Join(t.TempDir(), "three-control-plane-artifacts.json")
	if err := writeThreeControlPlaneArtifactManifest(path, threeControlPlaneArtifactManifest{
		NodeRunDirs:        map[string]string{"cp-1": "/tmp/cp-1-run"},
		FixtureInputs:      inputs,
		PublishedFixtures:  got,
		Diagnostics:        map[string]string{"cp-1": "/tmp/cp-1-guest/diagnostics-summary.json", "cp-2": "/tmp/cp-2-guest/diagnostics-summary.json", "cp-3": "/tmp/cp-3-guest/diagnostics-summary.json"},
		KubectlDiagnostics: map[string]string{"kubeSystemPods": "/tmp/run/kubectl-get-pods-kube-system.txt"},
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
	if manifest.PublishedFixtures["cp-1"] != "/tmp/cp-1" || manifest.PublishedFixtures["cp-2"] != "/tmp/cp-2" || manifest.PublishedFixtures["cp-3"] != "/tmp/cp-3" {
		t.Fatalf("artifact manifest published fixtures = %#v", manifest.PublishedFixtures)
	}
	if manifest.FixtureInputs["cp-1"].FixtureManifest != "cp1-fixture.json" || manifest.FixtureInputs["cp-3"].PublishedFixtureDir != "/tmp/cp-3" {
		t.Fatalf("artifact manifest fixture inputs = %#v", manifest.FixtureInputs)
	}
	if manifest.FixtureInputs["cp-1"].KatlOSFixtureManifest != "/tmp/cp-1-katlos.json" || manifest.FixtureInputs["cp-3"].KatlOSFixtureManifest != "/tmp/cp-3-katlos.json" {
		t.Fatalf("artifact manifest KatlOS fixture inputs = %#v", manifest.FixtureInputs)
	}
	if manifest.Diagnostics["cp-1"] != "/tmp/cp-1-guest/diagnostics-summary.json" || manifest.Diagnostics["cp-3"] != "/tmp/cp-3-guest/diagnostics-summary.json" {
		t.Fatalf("artifact manifest diagnostics = %#v", manifest.Diagnostics)
	}
	if manifest.KubectlDiagnostics["kubeSystemPods"] != "/tmp/run/kubectl-get-pods-kube-system.txt" {
		t.Fatalf("artifact manifest kubectl diagnostics = %#v", manifest.KubectlDiagnostics)
	}
}
