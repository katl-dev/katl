package vmtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/operation"
	agent "github.com/zariel/katl/internal/katlc/agent"
	agentapi "github.com/zariel/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestInstalledRuntimeConfigApplyModesSmoke(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installed runtime config apply smoke")
	}
	runner := NewRunner(options)
	runtime := InstalledRuntimeConfig{}
	var plannedAddress string
	var plannedMAC string
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-config-apply-modes", NodeSpec{Name: "cp-1", Role: ControlPlane}); ok {
		runner = worldRun.Runner
		runtime = worldRun.Config
		plannedAddress = worldRun.Node.Address
		plannedMAC = worldRun.Node.MACAddress
	} else {
		_ = RequireWorld(t)
	}
	scenario := Scenario{Name: "installed-runtime-config-apply-modes"}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result = requirePlannedVMHost(t, runner, scenario, result, HostRequirements{
		Libvirt: true,
		OVMF:    true,
		KVM:     runner.options().KVM,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	katlctl := buildKatlctlForConfigApplySmoke(t, ctx)
	vm := runtime.VM
	vm.KVM = runner.options().KVM
	vm.RAMMiB = 4096
	vm.CPUs = 2
	vm.Timeout = 8 * time.Minute
	vm.Network.MAC = first(vm.Network.MAC, plannedMAC)
	vm.VSock.Enabled = true
	vm.Agent.RequireHealth = true
	vm.Agent.Timeout = 30 * time.Second
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
		Name: "cp-1",
		Runtime: InstalledRuntimeConfig{
			Disk:            runtime.Disk,
			DiskFormat:      runtime.DiskFormat,
			ESPArtifacts:    runtime.ESPArtifacts,
			FixtureManifest: runtime.FixtureManifest,
			NodeMetadata:    runtime.NodeMetadata,
			VM:              vm,
		},
	}, VMRunner{})
	if err != nil {
		t.Fatalf("StartInstalledRuntimeNode() error = %v", err)
	}
	defer func() {
		if err := node.Stop(); err != nil && err != context.Canceled {
			t.Logf("Stop() error = %v", err)
		}
	}()
	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatalf("DialAgent() error = %v", err)
	}
	defer client.Close()
	guest := NewGuestControl(node.Result, client)
	if err := RunKatlcSmoke(ctx, guest); err != nil {
		t.Fatalf("katlc runtime smoke: %v", err)
	}
	defer func() {
		if t.Failed() {
			collectConfigApplyFailureEvidence(ctx, guest)
		}
	}()
	currentGeneration := currentGenerationFromGuest(t, ctx, guest)
	endpoint := katlcEndpoint(t, node, plannedAddress)
	tokenFile, token := writeKatlcAgentTokenFile(t, ctx, guest, result.RunDir)
	runConfigApplyModeSmoke(t, ctx, guest, result, katlctl, endpoint, tokenFile, token, currentGeneration)
	assertBootstrappedKubernetesSysextChangeRejected(t, ctx, guest, endpoint, token)
	node.Result.finish(StatusPassed, "", runner.time())
	if err := runner.Write(scenario, node.Result); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

func requirePlannedVMHost(t testTB, runner Runner, scenario Scenario, result Result, requirements HostRequirements) Result {
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
			t.Fatalf("write vmtest result for %q failed: %v\nvmtest run dir: %s", scenario.Name, writeErr, result.RunDir)
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

func TestRequirePlannedVMHostWritesFailedResult(t *testing.T) {
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
				return "", errors.New("missing VM command")
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

	got := requirePlannedVMHost(tb, runner, scenario, result, HostRequirements{Libvirt: true})
	if !tb.failed || tb.skipped {
		t.Fatalf("failed=%v skipped=%v message=%q", tb.failed, tb.skipped, tb.message)
	}
	if got.Status != StatusFailed || !strings.Contains(got.FailureSummary, "virsh") {
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

func buildKatlctlForConfigApplySmoke(t *testing.T, ctx context.Context) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "katlctl")
	cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", path, "./cmd/katlctl")
	cmd.Dir = repoRoot(t)
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build katlctl: %v\n%s", err, output)
	}
	return path
}

func katlcEndpoint(t *testing.T, node RunningInstalledRuntimeNode, plannedAddress string) string {
	t.Helper()
	address := first(node.Result.IPAddress, plannedAddress)
	if strings.TrimSpace(address) == "" {
		t.Fatalf("installed runtime node %q has no libvirt lease IP address", node.Name)
	}
	return net.JoinHostPort(address, "9443")
}

func writeKatlcAgentTokenFile(t *testing.T, ctx context.Context, guest *GuestControl, runDir string) (string, string) {
	t.Helper()
	tokenArtifact := "/var/lib/katl/test-artifacts/config-apply/agent-token"
	deadline := time.Now().Add(30 * time.Second)
	var last GuestCommandArtifact
	var lastErr error
	for time.Now().Before(deadline) {
		record, err := guest.RunCommand(ctx, GuestCommandRequest{
			Name:         "stage-katlc-agent-token",
			Argv:         []string{"install", "-D", "-m", "0600", "/var/lib/katl/agent/token", tokenArtifact},
			AllowFailure: true,
		})
		last, lastErr = record, err
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("stage katlc agent token for vmtest harness: %v %#v", lastErr, last)
	}
	record, err := guest.ReadFile(ctx, GuestFileRequest{
		Name:         "katlc-agent-token",
		Path:         tokenArtifact,
		MaxBytes:     4 << 10,
		StoreContent: true,
	})
	if err != nil {
		t.Fatalf("read katlc agent token: %v", err)
	}
	token := strings.TrimSpace(readFile(t, record.Artifact))
	if token == "" {
		t.Fatal("katlc agent token is empty")
	}
	dir := filepath.Join(runDir, "katlctl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create katlctl artifact dir: %v", err)
	}
	path := filepath.Join(dir, "agent-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write host katlc agent token file: %v", err)
	}
	return path, token
}

func runConfigApplyModeSmoke(t *testing.T, ctx context.Context, guest *GuestControl, result Result, katlctl, endpoint, tokenFile, token, currentGeneration string) {
	t.Helper()
	beforeSysext := readlinkOptional(t, ctx, guest, "/run/extensions/katl-kubernetes.raw")
	beforeConfext := readlink(t, ctx, guest, "/run/confexts/katl-node")
	rejectedGeneration := "2026.06.06-vmtest-rejected"
	liveGeneration := "2026.06.06-vmtest-live"
	stagedGeneration := "2026.06.06-vmtest-staged"

	rejectedOutput := runKatlctl(t, ctx, result, katlctl, "config-apply-validate-rejected",
		"config", "apply", "validate",
		"--endpoint", endpoint,
		"--agent-token-file", tokenFile,
		"--file", configApplyFixture(t, "rejected-live-without-preflight.yaml"),
		"--mode", "live",
		"--candidate-generation", rejectedGeneration,
		"--client-request-id", "vmtest-config-apply-rejected",
		"--actor", "installed-runtime config apply vmtest",
	)
	var rejected agentapi.ConfigValidationResult
	mustUnmarshalProtoJSON(t, rejectedOutput, &rejected)
	if rejected.Accepted || !strings.Contains(strings.Join(rejected.Diagnostics, "\n"), "staged-only") {
		t.Fatalf("rejected validation = %+v, want fail closed staged-only diagnostic", rejected)
	}
	assertGuestMissing(t, ctx, guest, "/var/lib/katl/generations/"+rejectedGeneration)
	assertOptionalReadlink(t, ctx, guest, "/run/extensions/katl-kubernetes.raw", beforeSysext)
	assertReadlink(t, ctx, guest, "/run/confexts/katl-node", beforeConfext)

	rejectedApplyGeneration := "2026.06.06-vmtest-rejected-apply"
	rejectedAccepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, tokenFile, "config-apply-rejected", "live", rejectedApplyGeneration, "vmtest-config-apply-rejected-submit", configApplyFixture(t, "rejected-live-without-preflight.yaml"))
	rejectedStatus := waitKatlcOperationTerminal(t, ctx, endpoint, token, rejectedAccepted.OperationId)
	if rejectedStatus.Result == operation.ResultSucceeded ||
		rejectedStatus.GetExternalMutationStarted() ||
		len(rejectedStatus.GetMutationScopes()) != 0 ||
		!strings.Contains(rejectedStatus.GetFailureReason(), "rejected") {
		t.Fatalf("rejected apply operation status = %+v, want fail closed before mutation", rejectedStatus)
	}
	assertGuestMissing(t, ctx, guest, "/var/lib/katl/generations/"+rejectedApplyGeneration)
	assertOptionalReadlink(t, ctx, guest, "/run/extensions/katl-kubernetes.raw", beforeSysext)
	assertReadlink(t, ctx, guest, "/run/confexts/katl-node", beforeConfext)
	assertGuestFileContains(t, ctx, guest, rejectedAccepted.RecordPath, `"operationKind": "generation-apply"`, `"result": "failed-needs-repair"`, "staged-only")

	liveAccepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, tokenFile, "config-apply-live", "live", liveGeneration, "vmtest-config-apply-live", configApplyFixture(t, "live-sysctl.yaml"))
	liveStatus := waitKatlcOperationTerminal(t, ctx, endpoint, token, liveAccepted.OperationId)
	if liveStatus.Result != operation.ResultSucceeded || liveStatus.ConfigApplyPhase != "active" {
		t.Fatalf("live operation status = %+v, want succeeded active config apply", liveStatus)
	}
	liveGenerationStatus := katlctlGenerationStatus(t, ctx, result, katlctl, endpoint, tokenFile, "status-live", liveGeneration)
	if liveGenerationStatus.GetConfigApply().GetPhase() != "active" || liveGenerationStatus.GetConfigApply().GetAcceptedApplyMode() != "live" {
		t.Fatalf("live katlctl generation status = %+v, want active live config apply", liveGenerationStatus.GetConfigApply())
	}
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+liveGeneration+"/spec.json", `"generationID": "`+liveGeneration+`"`, `"previousGenerationID": "`+currentGeneration+`"`)
	if beforeSysext != "" {
		assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+liveGeneration+"/spec.json", `"name": "kubernetes"`)
	}
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+liveGeneration+"/status.json", `"commitState": "candidate"`)
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+liveGeneration+"/config-apply-status.json", `"phase": "active"`, `"acceptedApplyMode": "live"`)
	assertGuestFileContains(t, ctx, guest, "/run/confexts/katl-node/etc/sysctl.d/90-katl.conf", "net.ipv4.ip_forward = 1")
	assertGuestFileContains(t, ctx, guest, liveAccepted.RecordPath, `"operationKind": "generation-apply"`, `"configApplyPhase": "active"`)

	liveConfext := readlink(t, ctx, guest, "/run/confexts/katl-node")
	stagedAccepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, tokenFile, "config-apply-staged", "next-boot", stagedGeneration, "vmtest-config-apply-staged", configApplyFixture(t, "next-boot-identity.yaml"))
	stagedStatus := waitKatlcOperationTerminal(t, ctx, endpoint, token, stagedAccepted.OperationId)
	if stagedStatus.Result != operation.ResultSucceeded || stagedStatus.ConfigApplyPhase != "next-boot" {
		t.Fatalf("staged operation status = %+v, want succeeded next-boot config apply", stagedStatus)
	}
	stagedGenerationStatus := katlctlGenerationStatus(t, ctx, result, katlctl, endpoint, tokenFile, "status-staged", stagedGeneration)
	if stagedGenerationStatus.GetConfigApply().GetPhase() != "next-boot" || stagedGenerationStatus.GetConfigApply().GetAcceptedApplyMode() != "next-boot" {
		t.Fatalf("staged katlctl generation status = %+v, want next-boot config apply", stagedGenerationStatus.GetConfigApply())
	}
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/spec.json", `"generationID": "`+stagedGeneration+`"`, `"previousGenerationID": "`+currentGeneration+`"`)
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/status.json", `"commitState": "candidate"`)
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/config-apply-status.json", `"phase": "next-boot"`, `"acceptedApplyMode": "next-boot"`, `"domain": "node-identity"`)
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/confext/etc/katl/node.json", `"hostname": "cp-1-staged"`)
	assertGuestExists(t, ctx, guest, "/var/lib/katl/generations/"+currentGeneration+"/metadata.json")
	assertReadlink(t, ctx, guest, "/run/confexts/katl-node", liveConfext)
	assertOptionalReadlink(t, ctx, guest, "/run/extensions/katl-kubernetes.raw", beforeSysext)
	assertGuestFileContains(t, ctx, guest, stagedAccepted.RecordPath, `"operationKind": "generation-stage"`, `"configApplyPhase": "next-boot"`)
}

func submitKatlctlConfigApply(t *testing.T, ctx context.Context, result Result, katlctl, endpoint, tokenFile, name, mode, generationID, clientRequestID, fixture string) agentapi.OperationAccepted {
	t.Helper()
	output := runKatlctl(t, ctx, result, katlctl, name,
		"config", "apply",
		"--endpoint", endpoint,
		"--agent-token-file", tokenFile,
		"--file", fixture,
		"--mode", mode,
		"--candidate-generation", generationID,
		"--client-request-id", clientRequestID,
		"--actor", "installed-runtime config apply vmtest",
	)
	var accepted agentapi.OperationAccepted
	mustUnmarshalProtoJSON(t, output, &accepted)
	if strings.TrimSpace(accepted.OperationId) == "" || strings.TrimSpace(accepted.RecordPath) == "" {
		t.Fatalf("%s accepted operation = %+v, want operation ID and record path", name, accepted)
	}
	return accepted
}

func katlctlGenerationStatus(t *testing.T, ctx context.Context, result Result, katlctl, endpoint, tokenFile, name, generationID string) agentapi.Generation {
	t.Helper()
	output := runKatlctl(t, ctx, result, katlctl, name,
		"config", "apply", "status",
		"--endpoint", endpoint,
		"--agent-token-file", tokenFile,
		"--generation", generationID,
	)
	var gen agentapi.Generation
	mustUnmarshalProtoJSON(t, output, &gen)
	return gen
}

func runKatlctl(t *testing.T, ctx context.Context, result Result, katlctl, name string, args ...string) []byte {
	t.Helper()
	dir := filepath.Join(result.RunDir, "katlctl", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create katlctl artifact dir: %v", err)
	}
	record := struct {
		Name  string   `json:"name"`
		Argv  []string `json:"argv"`
		Error string   `json:"error,omitempty"`
	}{
		Name: name,
		Argv: append([]string{katlctl}, args...),
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, katlctl, args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = os.Environ()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		record.Error = err.Error()
	}
	if data, marshalErr := json.MarshalIndent(record, "", "  "); marshalErr == nil {
		_ = os.WriteFile(filepath.Join(dir, "command.json"), append(data, '\n'), 0o644)
	}
	_ = os.WriteFile(filepath.Join(dir, "stdout"), stdout.Bytes(), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "stderr"), stderr.Bytes(), 0o644)
	if err != nil {
		t.Fatalf("%s: katlctl failed: %v\nstdout:\n%s\nstderr:\n%s", name, err, stdout.String(), stderr.String())
	}
	return stdout.Bytes()
}

func configApplyFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "internal", "vmtest", "testdata", "config-apply", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config apply fixture %s: %v", name, err)
	}
	return path
}

func mustUnmarshalProtoJSON(t *testing.T, data []byte, msg proto.Message) {
	t.Helper()
	if err := protojson.Unmarshal(data, msg); err != nil {
		t.Fatalf("decode proto json into %T: %v\n%s", msg, err, data)
	}
}

func waitKatlcOperationTerminal(t *testing.T, ctx context.Context, endpoint, token, operationID string) *agentapi.OperationStatus {
	t.Helper()
	conn, client := dialKatlcAgentForVMTest(t, ctx, endpoint)
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Minute)
	var last *agentapi.OperationStatus
	for time.Now().Before(deadline) {
		status, err := client.GetOperation(authContext(ctx, token), &agentapi.GetOperationRequest{
			OperationId:        operationID,
			IncludeDiagnostics: "verbose",
		})
		if err != nil {
			t.Fatalf("get operation %s: %v", operationID, err)
		}
		last = status
		if status.Terminal {
			return status
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("operation %s did not become terminal; last status = %+v", operationID, last)
	return nil
}

func assertBootstrappedKubernetesSysextChangeRejected(t *testing.T, ctx context.Context, guest *GuestControl, endpoint, token string) {
	t.Helper()
	const kubeletConf = "/etc/kubernetes/kubelet.conf"
	const kubeletMarker = `apiVersion: v1
kind: Config
clusters: []
contexts: []
current-context: ""
users: []
`
	beforeSysext := readlinkOptional(t, ctx, guest, "/run/extensions/katl-kubernetes.raw")
	beforeConfext := readlink(t, ctx, guest, "/run/confexts/katl-node")
	markerSource := "/var/lib/katl/test-artifacts/config-apply/kubelet.conf"
	if _, err := guest.WriteFile(ctx, GuestFileRequest{
		Name:    "bootstrapped-kubelet-conf-source",
		Path:    markerSource,
		Content: []byte(kubeletMarker),
		Mode:    0o600,
	}); err != nil {
		t.Fatalf("write bootstrapped kubelet marker source: %v", err)
	}
	if _, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name: "install-bootstrapped-kubelet-conf",
		Argv: []string{"install", "-D", "-m", "0600", markerSource, kubeletConf},
	}); err != nil {
		t.Fatalf("install bootstrapped kubelet marker: %v", err)
	}
	assertGuestFileContains(t, ctx, guest, kubeletConf, "kind: Config")
	assertGuestMissing(t, ctx, guest, "/etc/kubernetes/admin.conf")
	assertGuestMissing(t, ctx, guest, "/etc/kubernetes/manifests/kube-apiserver.yaml")
	assertGuestMissing(t, ctx, guest, "/var/lib/kubelet/config.yaml")

	conn, client := dialKatlcAgentForVMTest(t, ctx, endpoint)
	defer conn.Close()
	accepted, err := client.SubmitOperation(authContext(ctx, token), &agentapi.SubmitOperationRequest{
		ApiVersion:      operation.APIVersion,
		Kind:            agent.RequestKind,
		ClientRequestId: "vmtest-kubeadm-upgrade-refused",
		OperationKind:   agent.OperationKindKubeadmUpgrade,
		Actor:           "installed-runtime config apply vmtest",
		KubernetesSysextUpdate: &agentapi.KubernetesSysextUpdateOperationRequest{
			TargetPayloadVersion: "v9.99.0",
			TargetSysextPath:     "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw",
			TargetSysextSha256:   strings.Repeat("e", 64),
		},
	})
	if err != nil {
		t.Fatalf("submit Kubernetes sysext update rejection request: %v", err)
	}
	status := accepted.GetInitialStatus()
	if !status.GetTerminal() {
		status = waitKatlcOperationTerminal(t, ctx, endpoint, token, accepted.OperationId)
	}
	if status.GetOperationKind() != agent.OperationKindKubeadmUpgrade ||
		status.GetPhase() != "execution-refused-unsupported" ||
		status.GetResult() != "execution-refused-unsupported" ||
		!status.GetTerminal() ||
		status.GetExternalMutationStarted() ||
		len(status.GetMutationScopes()) != 0 ||
		len(status.GetInvocations()) != 0 ||
		strings.TrimSpace(status.GetCandidateGenerationId()) != "" {
		t.Fatalf("Kubernetes sysext rejection status = %+v", status)
	}
	if !strings.Contains(status.GetFailureReason(), "target kubeadm access mode is not selected") ||
		!strings.Contains(status.GetNextAction(), "kubelet activation gate") {
		t.Fatalf("Kubernetes sysext rejection diagnostics = failure %q next %q", status.GetFailureReason(), status.GetNextAction())
	}
	assertGuestFileContains(t, ctx, guest, accepted.RecordPath, `"operationKind": "kubeadm-upgrade"`, `"phase": "execution-refused-unsupported"`)
	assertGuestFileContains(t, ctx, guest, kubeletConf, "kind: Config")
	assertGuestMissing(t, ctx, guest, "/etc/kubernetes/admin.conf")
	assertGuestMissing(t, ctx, guest, "/etc/kubernetes/manifests/kube-apiserver.yaml")
	assertGuestMissing(t, ctx, guest, "/var/lib/kubelet/config.yaml")
	assertOptionalReadlink(t, ctx, guest, "/run/extensions/katl-kubernetes.raw", beforeSysext)
	assertReadlink(t, ctx, guest, "/run/confexts/katl-node", beforeConfext)
}

func dialKatlcAgentForVMTest(t *testing.T, ctx context.Context, endpoint string) (*grpc.ClientConn, agentapi.KatlcAgentClient) {
	t.Helper()
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("dial katlc agent %s: %v", endpoint, err)
	}
	return conn, agentapi.NewKatlcAgentClient(conn)
}

func authContext(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.TrimSpace(token))
}

func collectConfigApplyFailureEvidence(ctx context.Context, guest *GuestControl) {
	_, _ = guest.ExportJournal(ctx, GuestJournalRequest{
		Name:     "katlc-agent-journal",
		Units:    []string{"katlc-agent.service"},
		MaxBytes: 512 << 10,
	})
	for _, req := range []GuestCommandRequest{
		{Name: "katlc-agent-status", Argv: []string{"systemctl", "status", "--no-pager", "katlc-agent.service"}, AllowFailure: true},
		{Name: "generation-json-files", Argv: []string{"find", "/var/lib/katl/generations", "-maxdepth", "3", "-type", "f", "-name", "*.json", "-print"}, AllowFailure: true},
		{Name: "operation-json-files", Argv: []string{"find", "/var/lib/katl/operations", "-maxdepth", "3", "-type", "f", "-name", "*.json", "-print"}, AllowFailure: true},
	} {
		_, _ = guest.RunCommand(ctx, req)
	}
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

func readlinkOptional(t *testing.T, ctx context.Context, guest *GuestControl, path string) string {
	t.Helper()
	record, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name:         "readlink-" + filepath.Base(path),
		Argv:         []string{"readlink", path},
		AllowFailure: true,
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(readFile(t, record.Stdout))
}

func assertReadlink(t *testing.T, ctx context.Context, guest *GuestControl, path, want string) {
	t.Helper()
	if got := readlink(t, ctx, guest, path); got != want {
		t.Fatalf("readlink %s = %q, want %q", path, got, want)
	}
}

func assertOptionalReadlink(t *testing.T, ctx context.Context, guest *GuestControl, path, want string) {
	t.Helper()
	if want == "" {
		assertGuestMissing(t, ctx, guest, path)
		return
	}
	assertReadlink(t, ctx, guest, path, want)
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
