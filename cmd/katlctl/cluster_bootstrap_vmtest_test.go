package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	esp := vmtest.RequireEnv(t, "KATL_INSTALLED_ESP_ARTIFACTS")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cpNode, err := vmtest.StartInstalledRuntimeNode(ctx, result, vmtest.InstalledRuntimeNodeConfig{
		Name: "cp-1",
		Runtime: vmtest.InstalledRuntimeConfig{
			Disk:         cpDisk,
			DiskFormat:   vmtest.DiskFormat(firstString(os.Getenv("KATL_CONTROL_PLANE_INSTALLED_DISK_FORMAT"), string(vmtest.DiskRaw))),
			ESPArtifacts: esp,
			VM:           twoNodeVMConfig(options.KVM, 43101),
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
			Disk:         workerDisk,
			DiskFormat:   vmtest.DiskFormat(firstString(os.Getenv("KATL_WORKER_INSTALLED_DISK_FORMAT"), string(vmtest.DiskRaw))),
			ESPArtifacts: esp,
			VM:           twoNodeVMConfig(options.KVM, 43102),
		},
	}, vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics(cpNode)
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
		ControlPlaneRunDir: cpNode.Result.RunDir,
		WorkerRunDir:       workerNode.Result.RunDir,
		Inventory:          inventoryPath,
		Kubeconfig:         kubeconfigPath,
		BootstrapStdout:    stdoutPath,
		BootstrapStderr:    stderrPath,
		KubectlOutput:      kubectlOut,
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
	}, &stdout, &stderr)
	_ = os.WriteFile(stdoutPath, stdout.Bytes(), 0o644)
	_ = os.WriteFile(stderrPath, stderr.Bytes(), 0o644)
	if err != nil {
		collectTwoNodeDiagnostics(cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("katlctl cluster bootstrap failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	assertBootstrapPhases(t, stdout.String())

	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfigPath, "get", "nodes", "-o", "name")
	output, err := cmd.CombinedOutput()
	_ = os.WriteFile(kubectlOut, output, 0o644)
	if err != nil {
		collectTwoNodeDiagnostics(cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("kubectl get nodes failed: %v\n%s", err, output)
	}
	for _, want := range []string{"node/cp-1", "node/worker-1"} {
		if !strings.Contains(string(output), want) {
			collectTwoNodeDiagnostics(cpNode, workerNode)
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
	ControlPlaneRunDir string `json:"controlPlaneRunDir"`
	WorkerRunDir       string `json:"workerRunDir"`
	Inventory          string `json:"inventory"`
	Kubeconfig         string `json:"kubeconfig"`
	BootstrapStdout    string `json:"bootstrapStdout"`
	BootstrapStderr    string `json:"bootstrapStderr"`
	KubectlOutput      string `json:"kubectlOutput"`
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

func collectTwoNodeDiagnostics(nodes ...vmtest.RunningInstalledRuntimeNode) {
	diagCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, node := range nodes {
		if !node.VSock.Enabled {
			continue
		}
		client, err := vmtest.DialAgent(diagCtx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
		if err != nil {
			writeTwoNodeDiagnosticError(node, "dial-agent-error.txt", err)
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
			_ = writeTwoNodeDiagnosticJSON(filepath.Join(node.Result.Artifacts.GuestDir, "diagnostics-errors.json"), report.Errors)
		}
		_ = client.Close()
	}
}

func writeTwoNodeDiagnosticError(node vmtest.RunningInstalledRuntimeNode, name string, err error) {
	_ = os.MkdirAll(node.Result.Artifacts.GuestDir, 0o755)
	_ = os.WriteFile(filepath.Join(node.Result.Artifacts.GuestDir, name), []byte(err.Error()+"\n"), 0o644)
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

func uint32String(value uint32) string {
	return strconv.FormatUint(uint64(value), 10)
}
