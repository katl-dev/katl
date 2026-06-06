package vmtest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstalledRuntimeConfigApplyModesSmoke(t *testing.T) {
	options := DefaultOptions()
	if options.StateRoot == "" {
		options.StateRoot = filepath.Join(repoRoot(t), "build", "vmtest")
	}
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installed runtime config apply smoke")
	}
	runner := NewRunner(options)
	runtime := InstalledRuntimeConfig{}
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-config-apply-modes", NodeSpec{Name: "cp-1", Role: ControlPlane}); ok {
		runner = worldRun.Runner
		runtime = worldRun.Config
	} else {
		_ = RequireWorld(t)
	}
	scenario := Scenario{Name: "installed-runtime-config-apply-modes"}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result = requireConfigApplyVMHost(t, runner, scenario, result, HostRequirements{
		QEMU: true,
		OVMF: true,
		KVM:  runner.options().KVM,
	})
	helper := buildConfigApplySmokeHelper(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
		Name: "cp-1",
		Runtime: InstalledRuntimeConfig{
			Disk:            runtime.Disk,
			DiskFormat:      runtime.DiskFormat,
			ESPArtifacts:    runtime.ESPArtifacts,
			FixtureManifest: runtime.FixtureManifest,
			NodeMetadata:    runtime.NodeMetadata,
			VM: VMConfig{
				KVM:     runner.options().KVM,
				RAMMiB:  4096,
				CPUs:    2,
				Timeout: 8 * time.Minute,
				VSock: VSockConfig{
					Enabled: true,
				},
				Agent: AgentControlConfig{
					RequireHealth: true,
					Timeout:       30 * time.Second,
				},
			},
		},
	}, VMRunner{})
	if err != nil {
		t.Fatalf("StartInstalledRuntimeNode() error = %v", err)
	}
	defer func() {
		if err := node.Stop(); err != nil && err != context.Canceled {
			t.Fatalf("Stop() error = %v", err)
		}
	}()
	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatalf("DialAgent() error = %v", err)
	}
	defer client.Close()
	guest := NewGuestControl(node.Result, client)
	currentGeneration := currentGenerationFromGuest(t, ctx, guest)
	uploadConfigApplySmokeInputs(t, ctx, guest, helper)
	runConfigApplyModeSmoke(t, ctx, guest, currentGeneration)
	node.Result.finish(StatusPassed, "", runner.time())
	if err := runner.Write(scenario, node.Result); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

func requireConfigApplyVMHost(t testTB, runner Runner, scenario Scenario, result Result, requirements HostRequirements) Result {
	t.Helper()
	result.start(runner.time())
	if err := runner.CheckHost(requirements); err != nil {
		status := StatusFailed
		if runner.options().Missing == MissingSkips {
			status = StatusSkipped
		}
		var prereq PrereqError
		if errors.As(err, &prereq) {
			result.Missing = prereq.Missing
		}
		result.finish(status, err.Error(), runner.time())
		if writeErr := runner.Write(scenario, result); writeErr != nil {
			t.Fatalf("write config-apply vmtest result for %q failed: %v\nvmtest run dir: %s", scenario.Name, writeErr, result.RunDir)
			return result
		}
		if status == StatusSkipped {
			t.Skipf("%v\nvmtest run dir: %s", err, result.RunDir)
			return result
		}
		t.Fatalf("%v\nvmtest run dir: %s", err, result.RunDir)
	}
	return result
}

func TestRequireConfigApplyVMHostWritesFailedResult(t *testing.T) {
	tb := &fakeTB{}
	runner := Runner{
		Options: Options{
			Enabled:   true,
			StateRoot: t.TempDir(),
			RunID:     "run-1",
			Missing:   MissingFails,
		},
		probe: probe{
			lookPath: func(string) (string, error) {
				return "", errors.New("missing qemu")
			},
		},
		now: func() time.Time {
			return time.Unix(10, 0)
		},
	}
	scenario := Scenario{Name: "installed-runtime-config-apply-modes"}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	got := requireConfigApplyVMHost(tb, runner, scenario, result, HostRequirements{QEMU: true})
	if !tb.failed || tb.skipped {
		t.Fatalf("failed=%v skipped=%v message=%q", tb.failed, tb.skipped, tb.message)
	}
	if got.Status != StatusFailed || !strings.Contains(got.FailureSummary, "qemu-system-x86_64") {
		t.Fatalf("result = %#v", got)
	}
	data, err := os.ReadFile(got.Artifacts.Result)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var stored Result
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if stored.Status != StatusFailed || len(stored.Missing) == 0 || stored.Phases[0].Name != "host-prerequisites" {
		t.Fatalf("stored result = %#v", stored)
	}
}

func buildConfigApplySmokeHelper(t *testing.T) []byte {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "configapply-smoke")
	cmd := exec.Command("go", "build", "-o", path, "./internal/vmtest/testcmd/configapply-smoke")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build config apply smoke helper: %v\n%s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	return data
}

func uploadConfigApplySmokeInputs(t *testing.T, ctx context.Context, guest *GuestControl, helper []byte) {
	t.Helper()
	for _, file := range []struct {
		name      string
		path      string
		content   []byte
		mode      os.FileMode
		sensitive bool
	}{
		{name: "configapply-helper", path: configApplyGuestHelper, content: helper, mode: 0o755, sensitive: true},
		{name: "current-manifest", path: configApplyGuestManifest, content: []byte(configApplyCurrentManifest), mode: 0o600},
		{name: "rejected-request", path: configApplyGuestRejectedRequest, content: []byte(configApplyVMRejectedRequest), mode: 0o600},
		{name: "live-request", path: configApplyGuestLiveRequest, content: []byte(configApplyVMLiveRequest), mode: 0o600},
		{name: "staged-request", path: configApplyGuestStagedRequest, content: []byte(configApplyVMStagedRequest), mode: 0o600},
	} {
		if _, err := guest.WriteFile(ctx, GuestFileRequest{
			Name:      file.name,
			Path:      file.path,
			Content:   file.content,
			Mode:      file.mode,
			Sensitive: file.sensitive,
		}); err != nil {
			t.Fatalf("upload %s: %v", file.name, err)
		}
	}
}

func runConfigApplyModeSmoke(t *testing.T, ctx context.Context, guest *GuestControl, currentGeneration string) {
	t.Helper()
	beforeSysext := readlink(t, ctx, guest, "/run/extensions/kubernetes")
	beforeConfext := readlink(t, ctx, guest, "/run/confexts/katl-node")
	rejectedGeneration := "2026.06.06-vmtest-rejected"
	liveGeneration := "2026.06.06-vmtest-live"
	stagedGeneration := "2026.06.06-vmtest-staged"

	rejected, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name: "config-apply-rejected",
		Argv: []string{
			configApplyGuestHelper,
			"--root=/",
			"--next-generation=" + rejectedGeneration,
			"--node=cp-1",
			"--manifest=" + configApplyGuestManifest,
			"--request=" + configApplyGuestRejectedRequest,
		},
		Timeout: 30 * time.Second,
	})
	if err == nil {
		t.Fatalf("rejected config apply unexpectedly passed: %#v", rejected)
	}
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/config-requests/operator/20.json", `"decision": "rejected"`, `"decision": "staged-required"`, "live preflight is required")
	assertGuestMissing(t, ctx, guest, "/var/lib/katl/generations/"+rejectedGeneration)
	assertReadlink(t, ctx, guest, "/run/extensions/kubernetes", beforeSysext)
	assertReadlink(t, ctx, guest, "/run/confexts/katl-node", beforeConfext)

	if _, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name: "config-apply-live",
		Argv: []string{
			configApplyGuestHelper,
			"--root=/",
			"--next-generation=" + liveGeneration,
			"--node=cp-1",
			"--manifest=" + configApplyGuestManifest,
			"--request=" + configApplyGuestLiveRequest,
			"--command-log=" + configApplyGuestCommandLog,
		},
		Timeout: 90 * time.Second,
	}); err != nil {
		t.Fatalf("live config apply: %v", err)
	}
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+liveGeneration+"/config-apply-status.json", `"phase": "active"`, `"acceptedApplyMode": "live"`)
	assertGuestExists(t, ctx, guest, "/run/confexts/katl-node/etc/systemd/network/20-vmtest-live.network")
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+liveGeneration+"/metadata.json", `"previousGenerationID": "`+currentGeneration+`"`, `"payloadVersion": "v1.36.0"`)
	commandLog := readGuestFile(t, ctx, guest, configApplyGuestCommandLog)
	for _, forbidden := range []string{"kubeadm", "kubectl"} {
		if strings.Contains(commandLog, forbidden) {
			t.Fatalf("normal config apply command log contains %s: %s", forbidden, commandLog)
		}
	}

	liveConfext := readlink(t, ctx, guest, "/run/confexts/katl-node")
	if _, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name: "config-apply-staged",
		Argv: []string{
			configApplyGuestHelper,
			"--root=/",
			"--next-generation=" + stagedGeneration,
			"--node=cp-1",
			"--manifest=" + configApplyGuestManifest,
			"--request=" + configApplyGuestStagedRequest,
		},
		Timeout: 30 * time.Second,
	}); err != nil {
		t.Fatalf("staged config apply: %v", err)
	}
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/config-apply-status.json", `"phase": "next-boot"`, `"acceptedApplyMode": "next-boot"`, `"domain": "node-identity"`)
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/confext/etc/katl/node.json", `"hostname": "cp-1-staged"`)
	assertGuestExists(t, ctx, guest, "/var/lib/katl/generations/"+currentGeneration+"/metadata.json")
	assertReadlink(t, ctx, guest, "/run/confexts/katl-node", liveConfext)
}

func currentGenerationFromGuest(t *testing.T, ctx context.Context, guest *GuestControl) string {
	t.Helper()
	cmdline := readGuestFile(t, ctx, guest, "/proc/cmdline")
	for _, field := range strings.Fields(cmdline) {
		if value, ok := strings.CutPrefix(field, "katl.generation="); ok && value != "" {
			return value
		}
	}
	t.Fatalf("guest /proc/cmdline has no katl.generation: %s", cmdline)
	return ""
}

func readlink(t *testing.T, ctx context.Context, guest *GuestControl, path string) string {
	t.Helper()
	record, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name: "readlink",
		Argv: []string{"readlink", path},
	})
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	return strings.TrimSpace(readFile(t, record.Stdout))
}

func assertReadlink(t *testing.T, ctx context.Context, guest *GuestControl, path, want string) {
	t.Helper()
	if got := readlink(t, ctx, guest, path); got != want {
		t.Fatalf("readlink %s = %q, want %q", path, got, want)
	}
}

func readGuestFile(t *testing.T, ctx context.Context, guest *GuestControl, path string) string {
	t.Helper()
	record, err := guest.ReadFile(ctx, GuestFileRequest{
		Name:         filepath.Base(path),
		Path:         path,
		StoreContent: true,
	})
	if err != nil {
		t.Fatalf("read guest file %s: %v", path, err)
	}
	return readFile(t, record.Artifact)
}

func assertGuestFileContains(t *testing.T, ctx context.Context, guest *GuestControl, path string, wants ...string) {
	t.Helper()
	content := readGuestFile(t, ctx, guest, path)
	for _, want := range wants {
		if !strings.Contains(content, want) {
			t.Fatalf("%s missing %q:\n%s", path, want, content)
		}
	}
}

func assertGuestExists(t *testing.T, ctx context.Context, guest *GuestControl, path string) {
	t.Helper()
	if _, err := guest.RunCommand(ctx, GuestCommandRequest{Name: filepath.Base(path), Argv: []string{"test", "-e", path}}); err != nil {
		t.Fatalf("guest path %s missing: %v", path, err)
	}
}

func assertGuestMissing(t *testing.T, ctx context.Context, guest *GuestControl, path string) {
	t.Helper()
	if record, err := guest.RunCommand(ctx, GuestCommandRequest{Name: filepath.Base(path), Argv: []string{"test", "!", "-e", path}}); err != nil {
		t.Fatalf("guest path %s exists or could not be checked: %v %#v", path, err, record)
	}
}

const (
	configApplyGuestDir             = "/var/lib/katl/test-artifacts/config-apply"
	configApplyGuestHelper          = configApplyGuestDir + "/configapply-smoke"
	configApplyGuestManifest        = configApplyGuestDir + "/current-manifest.json"
	configApplyGuestRejectedRequest = configApplyGuestDir + "/rejected-request.yaml"
	configApplyGuestLiveRequest     = configApplyGuestDir + "/live-request.yaml"
	configApplyGuestStagedRequest   = configApplyGuestDir + "/staged-request.yaml"
	configApplyGuestCommandLog      = configApplyGuestDir + "/live-command-log.json"
)

const configApplyVMRejectedRequest = `apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "20"
apply:
  mode: live
spec:
  clusterDefaults:
    networkd:
      files:
        - name: 20-vmtest-rejected.network
          content: |
            [Match]
            Name=*
            [Network]
            DHCP=yes
`

const configApplyVMLiveRequest = `apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "21"
apply:
  mode: live
spec:
  clusterDefaults:
    livePreflight:
      networkd: true
    networkd:
      files:
        - name: 20-vmtest-live.network
          content: |
            [Match]
            Name=*
            [Network]
            DHCP=yes
`

const configApplyVMStagedRequest = `apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "22"
apply:
  mode: next-boot
spec:
  clusterDefaults:
    identity:
      hostname: cp-1-staged
`
