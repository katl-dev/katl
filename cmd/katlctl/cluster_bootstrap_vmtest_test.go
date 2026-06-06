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
	kubeconfigMetadataPath := filepath.Join(result.RunDir, "operator-kubeconfig-metadata.json")
	stdoutPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stdout")
	stderrPath := filepath.Join(result.RunDir, "katlctl-bootstrap.stderr")
	kubectlOut := filepath.Join(result.RunDir, "kubectl-get-nodes.txt")
	bootstrapFixture := bootstrapFixtureInputsFromEnv()
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
		KubeconfigMetadata:     kubeconfigMetadataPath,
		BootstrapStdout:        stdoutPath,
		BootstrapStderr:        stderrPath,
		BootstrapFixture:       bootstrapFixture.manifestValue(),
		KubectlOutput:          kubectlOut,
		KubectlDiagnostics:     kubectlDiagnosticPaths(result.RunDir),
		ControlPlaneTranscript: twoNodeBootstrapTranscriptPath(transcriptDir, "cp-1"),
		WorkerTranscript:       twoNodeBootstrapTranscriptPath(transcriptDir, "worker-1"),
		SerialLogs:             serialLogPaths([]vmtest.RunningInstalledRuntimeNode{cpNode, workerNode}),
		Diagnostics:            diagnosticSummaryPaths([]vmtest.RunningInstalledRuntimeNode{cpNode, workerNode}),
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = run(ctx, appendBootstrapFixtureArgs([]string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--init-node", "cp-1",
		"--control-plane-endpoint", cpAddress + ":6443",
		"--node-address", "cp-1=" + cpAddress,
		"--node-address", "worker-1=" + workerAddress,
		"--kubeconfig-out", kubeconfigPath,
		"--overwrite-kubeconfig",
		"--vmtest-transcript-dir", transcriptDir,
	}, bootstrapFixture), &stdout, &stderr)
	_ = os.WriteFile(stdoutPath, stdout.Bytes(), 0o644)
	_ = os.WriteFile(stderrPath, stderr.Bytes(), 0o644)
	_ = writeKubeconfigMetadata(kubeconfigPath, kubeconfigMetadataPath)
	if err != nil {
		collectKubectlDiagnosticsIfKubeconfigExists(kubeconfigPath, result.RunDir)
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
		collectKubectlDiagnostics(kubeconfigPath, result.RunDir)
		collectTwoNodeDiagnostics(transcriptDir, cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("kubectl get nodes failed: %v\n%s", err, output)
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
	KubeconfigMetadata     string                      `json:"kubeconfigMetadata,omitempty"`
	BootstrapStdout        string                      `json:"bootstrapStdout"`
	BootstrapStderr        string                      `json:"bootstrapStderr"`
	BootstrapFixture       *bootstrapFixtureInputs     `json:"bootstrapFixture,omitempty"`
	KubectlOutput          string                      `json:"kubectlOutput"`
	KubectlDiagnostics     map[string]string           `json:"kubectlDiagnostics,omitempty"`
	ControlPlaneTranscript string                      `json:"controlPlaneTranscript"`
	WorkerTranscript       string                      `json:"workerTranscript"`
	SerialLogs             map[string]string           `json:"serialLogs,omitempty"`
	Diagnostics            map[string]string           `json:"diagnostics,omitempty"`
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

type bootstrapFixtureInputs struct {
	Manifests []string `json:"manifests,omitempty"`
	Waits     []string `json:"waits,omitempty"`
}

func (i bootstrapFixtureInputs) empty() bool {
	return len(i.Manifests) == 0 && len(i.Waits) == 0
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
		if !transcriptHasCommandFlagValue(entries, "kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml") {
			return errors.New("kubeadm init command missing control-plane config path")
		}
	case "worker-1":
		if transcriptHasCommand(entries, "kubeadm", "init") {
			return errors.New("unexpected kubeadm init command on worker node")
		}
		if !transcriptHasCommand(entries, "kubeadm", "join") {
			return errors.New("missing kubeadm join command")
		}
		if !transcriptHasCommandFlagValue(entries, "kubeadm", "join", "--config", "/etc/katl/kubeadm/worker/config.yaml") {
			return errors.New("worker kubeadm join command missing worker config path")
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

func kubectlDiagnosticPaths(runDir string) map[string]string {
	if strings.TrimSpace(runDir) == "" {
		return nil
	}
	return map[string]string{
		"clusterInfo":    filepath.Join(runDir, "kubectl-cluster-info.txt"),
		"events":         filepath.Join(runDir, "kubectl-get-events.txt"),
		"kubeSystemPods": filepath.Join(runDir, "kubectl-get-pods-kube-system.txt"),
		"nodesWide":      filepath.Join(runDir, "kubectl-get-nodes-wide.txt"),
	}
}

func kubectlDiagnosticCommands(kubeconfigPath string) []kubectlDiagnosticCommand {
	return []kubectlDiagnosticCommand{
		{Name: "nodesWide", Argv: []string{"kubectl", "--kubeconfig", kubeconfigPath, "get", "nodes", "-o", "wide"}},
		{Name: "kubeSystemPods", Argv: []string{"kubectl", "--kubeconfig", kubeconfigPath, "-n", "kube-system", "get", "pods", "-o", "wide"}},
		{Name: "events", Argv: []string{"kubectl", "--kubeconfig", kubeconfigPath, "get", "events", "-A", "--sort-by=.lastTimestamp"}},
		{Name: "clusterInfo", Argv: []string{"kubectl", "--kubeconfig", kubeconfigPath, "cluster-info"}},
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
			{Name: "etc-kubernetes-mount", Argv: []string{"findmnt", "--target", "/etc/kubernetes", "--output", "SOURCE,TARGET,FSTYPE,OPTIONS"}},
			{Name: "kubeadm-version", Argv: []string{"kubeadm", "version", "-o", "short"}},
		},
		Files: []vmtest.GuestFileRequest{
			{Name: "node-metadata", Path: "/etc/katl/node.json"},
			{Name: "kubeadm-config", Path: "/etc/katl/kubeadm/" + kubeadmRef + "/config.yaml"},
			{Name: "kubelet-kubeconfig", Path: "/etc/kubernetes/kubelet.conf"},
		},
		Journals: []vmtest.GuestJournalRequest{{
			Name:  "runtime-handoff",
			Units: []string{"katl-kubeadm-ready.target", "katl-generation-activate.service", "katl-runtime-handoff-status.service", "containerd.service", "kubelet.service"},
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

func serialLogPaths(nodes []vmtest.RunningInstalledRuntimeNode) map[string]string {
	out := make(map[string]string, len(nodes))
	for _, node := range nodes {
		if node.Name == "" || node.Result.Artifacts.RuntimeSerial == "" {
			continue
		}
		out[node.Name] = node.Result.Artifacts.RuntimeSerial
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
		KubeconfigMetadata: "/tmp/run/operator-kubeconfig-metadata.json",
		BootstrapFixture:   (&bootstrapFixtureInputs{Manifests: []string{"/tmp/cni.yaml"}, Waits: []string{"nodes-ready"}}).manifestValue(),
		SerialLogs:         map[string]string{"cp-1": "/tmp/cp-run/qemu/runtime-serial.log", "worker-1": "/tmp/worker-run/qemu/runtime-serial.log"},
		Diagnostics:        map[string]string{"cp-1": "/tmp/cp-guest/diagnostics-summary.json", "worker-1": "/tmp/worker-guest/diagnostics-summary.json"},
		KubectlDiagnostics: map[string]string{"nodesWide": "/tmp/run/kubectl-get-nodes-wide.txt"},
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
	if manifest.Diagnostics["cp-1"] != "/tmp/cp-guest/diagnostics-summary.json" || manifest.Diagnostics["worker-1"] != "/tmp/worker-guest/diagnostics-summary.json" {
		t.Fatalf("artifact manifest diagnostics = %#v", manifest.Diagnostics)
	}
	if manifest.SerialLogs["cp-1"] != "/tmp/cp-run/qemu/runtime-serial.log" || manifest.SerialLogs["worker-1"] != "/tmp/worker-run/qemu/runtime-serial.log" {
		t.Fatalf("artifact manifest serial logs = %#v", manifest.SerialLogs)
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

func TestAppendBootstrapFixtureArgs(t *testing.T) {
	got := appendBootstrapFixtureArgs([]string{"cluster", "bootstrap"}, bootstrapFixtureInputs{
		Manifests: []string{"/tmp/01-cni.yaml", "/tmp/02-workload.yaml"},
		Waits:     []string{"pods-ready:kube-system:k8s-app=kube-dns", "nodes-ready"},
	})
	want := []string{
		"cluster", "bootstrap",
		"--bootstrap-manifest", "/tmp/01-cni.yaml",
		"--bootstrap-manifest", "/tmp/02-workload.yaml",
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
	} {
		if paths[name] != want {
			t.Fatalf("kubectl diagnostic path %s = %q, want %q in %#v", name, paths[name], want, paths)
		}
	}
	if got := kubectlDiagnosticPaths(""); got != nil {
		t.Fatalf("kubectlDiagnosticPaths(\"\") = %#v, want nil", got)
	}

	commands := kubectlDiagnosticCommands("/tmp/kubeconfig.yaml")
	if len(commands) != 4 {
		t.Fatalf("kubectl diagnostic command count = %d, want 4: %#v", len(commands), commands)
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
	if !kubectlDiagnosticCommandHasArgs(commands, "events", "get", "events", "-A", "--sort-by=.lastTimestamp") {
		t.Fatalf("events diagnostic command missing expected args: %#v", commands)
	}
	if !kubectlDiagnosticCommandHasArgs(commands, "clusterInfo", "cluster-info") {
		t.Fatalf("clusterInfo diagnostic command missing expected args: %#v", commands)
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
		{Name: "cp-1", Result: vmtest.Result{Artifacts: vmtest.ArtifactPaths{RuntimeSerial: "/tmp/cp-run/qemu/runtime-serial.log"}}},
		{Name: "worker-1", Result: vmtest.Result{Artifacts: vmtest.ArtifactPaths{RuntimeSerial: "/tmp/worker-run/qemu/runtime-serial.log"}}},
		{Name: "ignored"},
	})
	if got["cp-1"] != "/tmp/cp-run/qemu/runtime-serial.log" || got["worker-1"] != "/tmp/worker-run/qemu/runtime-serial.log" {
		t.Fatalf("serial log paths = %#v", got)
	}
}

func TestBootstrapDiagnosticsAreNodeAware(t *testing.T) {
	cp := bootstrapDiagnostics("cp-1")
	if !diagnosticCommand(cp, "etc-kubernetes-mount") || !diagnosticCommand(cp, "kubeadm-version") {
		t.Fatalf("control-plane diagnostics commands = %#v", cp.Commands)
	}
	if !diagnosticFile(cp, "admin-kubeconfig", "/etc/kubernetes/admin.conf") {
		t.Fatalf("control-plane diagnostics files = %#v, want admin kubeconfig", cp.Files)
	}
	if !diagnosticFile(cp, "kube-apiserver-manifest", "/etc/kubernetes/manifests/kube-apiserver.yaml") || !diagnosticFile(cp, "etcd-manifest", "/etc/kubernetes/manifests/etcd.yaml") {
		t.Fatalf("control-plane diagnostics files = %#v, want static pod manifests", cp.Files)
	}
	if len(cp.Journals) != 1 || cp.Journals[0].Name != "runtime-handoff" || !diagnosticJournalUnit(cp, "runtime-handoff", "katl-runtime-handoff-status.service") {
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
		{Method: "RunCommand", Argv: []string{"kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "token", "create", "--print-join-command"}, Redaction: "output", SensitiveOutput: true},
	})
	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "worker-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "[REDACTED BOOTSTRAP TOKEN]", "--config", "/etc/katl/kubeadm/worker/config.yaml"}, Redaction: "output", SensitiveOutput: true},
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
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "[REDACTED BOOTSTRAP TOKEN]", "--control-plane", "--config", "/etc/katl/kubeadm/worker/config.yaml"}, Redaction: "output", SensitiveOutput: true},
	})
	err = verifyTwoNodeBootstrapTranscripts(dir)
	if err == nil || !strings.Contains(err.Error(), "worker kubeadm join command must not include --control-plane") {
		t.Fatalf("verifyTwoNodeBootstrapTranscripts() error = %v, want worker control-plane join rejection", err)
	}

	writeTranscriptEntries(t, twoNodeBootstrapTranscriptPath(dir, "worker-1"), []transcriptEntry{
		{Method: "RunCommand", Argv: []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}},
		{Method: "ReadFile", Redaction: "sensitive", SensitiveOutput: true},
		{Method: "RunCommand", Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "[REDACTED BOOTSTRAP TOKEN]", "--config", "/etc/katl/kubeadm/control-plane/config.yaml"}, Redaction: "output", SensitiveOutput: true},
	})
	err = verifyTwoNodeBootstrapTranscripts(dir)
	if err == nil || !strings.Contains(err.Error(), "worker kubeadm join command missing worker config path") {
		t.Fatalf("verifyTwoNodeBootstrapTranscripts() error = %v, want worker config path rejection", err)
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
