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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/vmtest"
)

func TestInstalledRuntimeTwoNodeKubeadmJoinSmoke(t *testing.T) {
	options := vmtest.DefaultOptions()
	options.Missing = vmtest.MissingSkips
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run two-node kubeadm join smoke")
	}
	cpDisk := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_INSTALLED_DISK")
	workerDisk := vmtest.RequireEnv(t, "KATL_WORKER_INSTALLED_DISK")
	cpESP := requireNodeESP(t, "KATL_CONTROL_PLANE_INSTALLED_ESP_ARTIFACTS")
	workerESP := requireNodeESP(t, "KATL_WORKER_INSTALLED_ESP_ARTIFACTS")
	cpFixture := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_FIXTURE_MANIFEST")
	workerFixture := vmtest.RequireEnv(t, "KATL_WORKER_FIXTURE_MANIFEST")
	cpMetadata := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_NODE_METADATA")
	workerMetadata := vmtest.RequireEnv(t, "KATL_WORKER_NODE_METADATA")
	cpAddress := vmtest.RequireEnv(t, "KATL_CONTROL_PLANE_ADDRESS")
	workerAddress := vmtest.RequireEnv(t, "KATL_WORKER_ADDRESS")
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
	scenario := vmtest.Scenario{Name: "installed-runtime-two-node-kubeadm-join"}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result.Started = time.Now().UTC()
	transcriptDir := filepath.Join(result.RunDir, "agent-transcripts")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cpNode, err := vmtest.StartInstalledRuntimeNode(ctx, result, vmtest.InstalledRuntimeNodeConfig{
		Name: "cp-1",
		Runtime: vmtest.InstalledRuntimeConfig{
			Disk:            cpDisk,
			DiskFormat:      vmtest.DiskFormat(firstString(os.Getenv("KATL_CONTROL_PLANE_INSTALLED_DISK_FORMAT"), string(vmtest.DiskRaw))),
			ESPArtifacts:    cpESP,
			FixtureManifest: cpFixture,
			NodeMetadata:    cpMetadata,
			VM:              twoNodeVMConfig(options.KVM, 43101),
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
			Disk:            workerDisk,
			DiskFormat:      vmtest.DiskFormat(firstString(os.Getenv("KATL_WORKER_INSTALLED_DISK_FORMAT"), string(vmtest.DiskRaw))),
			ESPArtifacts:    workerESP,
			FixtureManifest: workerFixture,
			NodeMetadata:    workerMetadata,
			VM:              twoNodeVMConfig(options.KVM, 43102),
		},
	}, vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics(transcriptDir, cpNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start worker VM: %v", err)
	}
	defer stopNode(t, workerNode)

	inventoryPath := filepath.Join(result.ManifestDir, "bootstrap-inventory.yaml")
	kubeconfigPath := filepath.Join(result.RunDir, "operator-kubeconfig.yaml")
	stdoutPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stdout")
	stderrPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stderr")
	kubectlOut := filepath.Join(result.RunDir, "kubectl-get-nodes.txt")
	if err := writeTwoNodeInventory(inventoryPath, kubernetesVersion, cpNode, workerNode); err != nil {
		t.Fatal(err)
	}
	if err := writeTwoNodeArtifactManifest(filepath.Join(result.ManifestDir, "two-node-artifacts.json"), twoNodeArtifactManifest{
		ControlPlaneRunDir:     cpNode.Result.RunDir,
		WorkerRunDir:           workerNode.Result.RunDir,
		FixtureInputs:          twoNodeFixtureInputs(cpDisk, workerDisk, cpESP, workerESP, cpFixture, workerFixture, cpMetadata, workerMetadata),
		PublishedFixtures:      twoNodePublishedFixtureDirs(),
		Inventory:              inventoryPath,
		Kubeconfig:             kubeconfigPath,
		BootstrapStdout:        stdoutPath,
		BootstrapStderr:        stderrPath,
		KubectlOutput:          kubectlOut,
		ControlPlaneTranscript: twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1"),
		WorkerTranscript:       twoNodeBootstrapTranscriptPath(transcriptDir, "worker-1"),
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--init-node", "cp-1",
		"--control-plane-endpoint", cpAddress + ":6443",
		"--node-address", "cp-1=" + cpAddress,
		"--node-address", "worker-1=" + workerAddress,
		"--kubeconfig-out", kubeconfigPath,
		"--overwrite-kubeconfig",
		"--vmtest-transcript-dir", transcriptDir,
	}, &stdout, &stderr)
	_ = os.WriteFile(stdoutPath, stdout.Bytes(), 0o644)
	_ = os.WriteFile(stderrPath, stderr.Bytes(), 0o644)
	if err != nil {
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

	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfigPath, "get", "nodes", "-o", "name")
	output, err := cmd.CombinedOutput()
	_ = os.WriteFile(kubectlOut, output, 0o644)
	if err != nil {
		collectTwoNodeDiagnostics(transcriptDir, cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("kubectl get nodes failed: %v\n%s", err, output)
	}
	for _, want := range []string{"node/cp-1", "node/worker-1"} {
		if !strings.Contains(string(output), want) {
			collectTwoNodeDiagnostics(transcriptDir, cpNode, workerNode)
			finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, "kubectl output missing "+want)
			t.Fatalf("kubectl output missing %q:\n%s", want, output)
		}
	}
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusPassed, "")
}

func twoNodeVMConfig(kvm vmtest.KVMPolicy, cid uint32) vmtest.VMConfig {
	return vmtest.VMConfig{
		KVM:     kvm,
		RAMMiB:  4096,
		CPUs:    2,
		Timeout: 25 * time.Minute,
		Network: vmtest.VMNetworkConfig{
			Mode: vmtest.VMNetworkBridge,
		},
		VSock: vmtest.VSockConfig{
			Enabled:  true,
			GuestCID: cid,
		},
	}
}

func requireNodeESP(t *testing.T, env string) string {
	t.Helper()
	if value := os.Getenv(env); value != "" {
		return value
	}
	return vmtest.RequireEnv(t, "KATL_INSTALLED_ESP_ARTIFACTS")
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

type twoNodeArtifactManifest struct {
	ControlPlaneRunDir     string                      `json:"controlPlaneRunDir"`
	WorkerRunDir           string                      `json:"workerRunDir"`
	FixtureInputs          map[string]nodeFixtureInput `json:"fixtureInputs,omitempty"`
	PublishedFixtures      map[string]string           `json:"publishedFixtures,omitempty"`
	Inventory              string                      `json:"inventory"`
	Kubeconfig             string                      `json:"kubeconfig"`
	BootstrapStdout        string                      `json:"bootstrapStdout"`
	BootstrapStderr        string                      `json:"bootstrapStderr"`
	KubectlOutput          string                      `json:"kubectlOutput"`
	ControlPlaneTranscript string                      `json:"controlPlaneTranscript"`
	WorkerTranscript       string                      `json:"workerTranscript"`
}

type nodeFixtureInput struct {
	Disk                  string `json:"disk"`
	DiskFormat            string `json:"diskFormat"`
	ESPArtifacts          string `json:"espArtifacts"`
	FixtureManifest       string `json:"fixtureManifest"`
	NodeMetadata          string `json:"nodeMetadata"`
	PublishedFixtureDir   string `json:"publishedFixtureDir,omitempty"`
	KatlOSFixtureManifest string `json:"katlosFixtureManifest,omitempty"`
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

func twoNodePublishedFixtureDirs() map[string]string {
	return compactStringMap(map[string]string{
		"cp-1":     os.Getenv("KATL_CONTROL_PLANE_PUBLISHED_FIXTURE_DIR"),
		"worker-1": os.Getenv("KATL_WORKER_PUBLISHED_FIXTURE_DIR"),
	})
}

func twoNodeFixtureInputs(cpDisk, workerDisk, cpESP, workerESP, cpFixture, workerFixture, cpMetadata, workerMetadata string) map[string]nodeFixtureInput {
	return map[string]nodeFixtureInput{
		"cp-1":     fixtureInput(cpDisk, firstString(os.Getenv("KATL_CONTROL_PLANE_INSTALLED_DISK_FORMAT"), string(vmtest.DiskRaw)), cpESP, cpFixture, cpMetadata, os.Getenv("KATL_CONTROL_PLANE_PUBLISHED_FIXTURE_DIR"), os.Getenv("KATL_CONTROL_PLANE_KATLOS_FIXTURE_MANIFEST")),
		"worker-1": fixtureInput(workerDisk, firstString(os.Getenv("KATL_WORKER_INSTALLED_DISK_FORMAT"), string(vmtest.DiskRaw)), workerESP, workerFixture, workerMetadata, os.Getenv("KATL_WORKER_PUBLISHED_FIXTURE_DIR"), os.Getenv("KATL_WORKER_KATLOS_FIXTURE_MANIFEST")),
	}
}

func fixtureInput(disk, format, esp, fixture, metadata, published, katlos string) nodeFixtureInput {
	return nodeFixtureInput{
		Disk:                  disk,
		DiskFormat:            format,
		ESPArtifacts:          esp,
		FixtureManifest:       fixture,
		NodeMetadata:          metadata,
		PublishedFixtureDir:   strings.TrimSpace(published),
		KatlOSFixtureManifest: strings.TrimSpace(katlos),
	}
}

func assertBootstrapPhases(t *testing.T, output string) {
	t.Helper()
	for _, want := range []string{
		"katlctl cluster bootstrap init-node=cp-1",
		"phase=kubeadm-init node=cp-1 status=passed",
		"phase=worker-join node=worker-1 status=passed",
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

func verifyTwoNodeBootstrapTranscripts(transcriptDir string) error {
	for _, node := range []string{"cp-1", "worker-1"} {
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
	case "worker-1":
		if transcriptHasCommand(entries, "kubeadm", "init") {
			return errors.New("unexpected kubeadm init command on worker node")
		}
		if !transcriptHasCommand(entries, "kubeadm", "join") {
			return errors.New("missing kubeadm join command")
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
		report := guest.CollectDiagnostics(diagCtx, vmtest.GuestDiagnostics{
			Timeout: 20 * time.Second,
			Commands: []vmtest.GuestCommandRequest{
				{Name: "kubeadm-ready", Argv: []string{"systemctl", "status", "katl-kubeadm-ready.target"}},
				{Name: "containerd", Argv: []string{"systemctl", "status", "containerd.service"}},
				{Name: "kubelet", Argv: []string{"systemctl", "status", "kubelet.service"}},
				{Name: "crictl-ps", Argv: []string{"crictl", "ps", "-a"}},
			},
			Files: []vmtest.GuestFileRequest{
				{Name: "node-metadata", Path: "/etc/katl/node.json"},
				{Name: "kubeadm-config", Path: "/etc/katl/kubeadm/" + kubeadmRefForNode(node.Name) + "/config.yaml"},
				{Name: "admin-kubeconfig", Path: "/etc/kubernetes/admin.conf"},
			},
			Journals: []vmtest.GuestJournalRequest{{
				Name:  "kubelet-journal",
				Units: []string{"kubelet.service", "containerd.service"},
			}},
		})
		if len(report.Errors) > 0 {
			summary.DiagnosticErrors = filepath.Join(node.Result.Artifacts.GuestDir, "diagnostics-errors.json")
			summary.CollectionErrors = append(summary.CollectionErrors, report.Errors...)
			_ = writeTwoNodeDiagnosticJSON(summary.DiagnosticErrors, report.Errors)
		}
		_ = writeTwoNodeDiagnosticJSON(filepath.Join(node.Result.Artifacts.GuestDir, "diagnostics-summary.json"), summary)
		_ = client.Close()
	}
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
	if err := runner.Write(scenario, result); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

func stopNode(t *testing.T, node vmtest.RunningInstalledRuntimeNode) {
	t.Helper()
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

func TestTwoNodePublishedFixtureDirs(t *testing.T) {
	t.Setenv("KATL_CONTROL_PLANE_PUBLISHED_FIXTURE_DIR", "/tmp/cp")
	t.Setenv("KATL_WORKER_PUBLISHED_FIXTURE_DIR", "/tmp/worker")
	t.Setenv("KATL_CONTROL_PLANE_KATLOS_FIXTURE_MANIFEST", "/tmp/cp-katlos.json")
	t.Setenv("KATL_WORKER_KATLOS_FIXTURE_MANIFEST", "/tmp/worker-katlos.json")
	t.Setenv("KATL_CONTROL_PLANE_INSTALLED_DISK_FORMAT", "qcow2")
	got := twoNodePublishedFixtureDirs()
	if got["cp-1"] != "/tmp/cp" || got["worker-1"] != "/tmp/worker" {
		t.Fatalf("published fixtures = %#v", got)
	}
	inputs := twoNodeFixtureInputs("cp.qcow2", "worker.raw", "cp-esp", "worker-esp", "cp-fixture.json", "worker-fixture.json", "cp-node.json", "worker-node.json")
	if inputs["cp-1"].DiskFormat != "qcow2" || inputs["worker-1"].DiskFormat != string(vmtest.DiskRaw) {
		t.Fatalf("fixture input formats = %#v", inputs)
	}
	path := filepath.Join(t.TempDir(), "two-node-artifacts.json")
	if err := writeTwoNodeArtifactManifest(path, twoNodeArtifactManifest{
		ControlPlaneRunDir: "/tmp/cp-run",
		WorkerRunDir:       "/tmp/worker-run",
		FixtureInputs:      inputs,
		PublishedFixtures:  got,
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
	if manifest.PublishedFixtures["cp-1"] != "/tmp/cp" || manifest.PublishedFixtures["worker-1"] != "/tmp/worker" {
		t.Fatalf("artifact manifest published fixtures = %#v", manifest.PublishedFixtures)
	}
	if manifest.FixtureInputs["cp-1"].FixtureManifest != "cp-fixture.json" || manifest.FixtureInputs["worker-1"].PublishedFixtureDir != "/tmp/worker" {
		t.Fatalf("artifact manifest fixture inputs = %#v", manifest.FixtureInputs)
	}
	if manifest.FixtureInputs["cp-1"].KatlOSFixtureManifest != "/tmp/cp-katlos.json" || manifest.FixtureInputs["worker-1"].KatlOSFixtureManifest != "/tmp/worker-katlos.json" {
		t.Fatalf("artifact manifest KatlOS fixture inputs = %#v", manifest.FixtureInputs)
	}
}

func TestVerifyTwoNodeBootstrapTranscriptsChecksKubeadmRoles(t *testing.T) {
	dir := t.TempDir()
	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "cp-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "token", "create", "--print-join-command"}, Redaction: "output", SensitiveOutput: true},
	})
	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "worker-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "[REDACTED BOOTSTRAP TOKEN]"}, Redaction: "output", SensitiveOutput: true},
	})
	if err := verifyTwoNodeBootstrapTranscripts(dir); err != nil {
		t.Fatalf("verifyTwoNodeBootstrapTranscripts() error = %v", err)
	}

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "worker-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
	})
	err := verifyTwoNodeBootstrapTranscripts(dir)
	if err == nil || !strings.Contains(err.Error(), "unexpected kubeadm init command on worker node") {
		t.Fatalf("verifyTwoNodeBootstrapTranscripts() error = %v, want worker init rejection", err)
	}

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "worker-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "[REDACTED BOOTSTRAP TOKEN]", "--control-plane"}, Redaction: "output", SensitiveOutput: true},
	})
	err = verifyTwoNodeBootstrapTranscripts(dir)
	if err == nil || !strings.Contains(err.Error(), "worker kubeadm join command must not include --control-plane") {
		t.Fatalf("verifyTwoNodeBootstrapTranscripts() error = %v, want worker control-plane join rejection", err)
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
