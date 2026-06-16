package vmtest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	vmtestpb "github.com/zariel/katl/internal/vmtest/proto"
)

func TestKubeadmAPISmokeRunsInitAndReadyz(t *testing.T) {
	result := guestResult(t)
	client := newScriptedGuestClient()
	client.commandResults = map[string][]*vmtestpb.CommandResult{
		commandKey("systemctl", "start", "katl-kubeadm-ready.target"):                              {okCommand()},
		commandKey("systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"):               {okCommand()},
		commandKey("test", "-x", "/usr/bin/katlc"):                                                 {okCommand()},
		commandKey("/usr/bin/katlc", "--help"):                                                     {stdoutCommand("Usage: katlc <command> [args]\nagent serve\n")},
		commandKey("test", "-f", DefaultKubeadmConfigPath):                                         {okCommand()},
		commandKey("findmnt", "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE"): {stdoutCommand(DefaultProjectedKubernetesPath + "\n")},
		commandKey("test", "-x", "/usr/bin/kubeadm"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/kubelet"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/kubectl"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/crictl"):                                                {okCommand()},
		commandKey("systemctl", "is-active", "--quiet", "containerd.service"):                      {okCommand()},
		commandKey("networkctl", "status", "--all"):                                                {failedCommand("Failed to connect to system bus\n")},
		commandKey("resolvectl", "status"):                                                         {stdoutCommand("DNS Servers: 10.0.2.3\n"), stdoutCommand("DNS Servers: 10.0.2.3\n")},
		commandKey("ip", "route"):                                                                  {stdoutCommand("default via 10.0.2.2 dev enp0s2\n"), stdoutCommand("default via 10.0.2.2 dev enp0s2\n")},
		commandKey("crictl", "info"):                                                               {stdoutCommand("{}\n")},
		commandKey("getent", "hosts", "registry.k8s.io"):                                           {stdoutCommand("1.2.3.4 registry.k8s.io\n")},
		commandKey("kubeadm", "init", "--config", DefaultKubeadmConfigPath, "--skip-token-print", "--skip-phases=addon/coredns,addon/kube-proxy"): {stdoutCommand("kubeadm init completed\n")},
		commandKey("test", "-f", "/etc/kubernetes/admin.conf"):                                      {okCommand()},
		commandKey("test", "-f", "/etc/kubernetes/manifests/kube-apiserver.yaml"):                   {okCommand()},
		commandKey("test", "-f", "/etc/kubernetes/manifests/kube-controller-manager.yaml"):          {okCommand()},
		commandKey("test", "-f", "/etc/kubernetes/manifests/kube-scheduler.yaml"):                   {okCommand()},
		commandKey("test", "-f", "/etc/kubernetes/manifests/etcd.yaml"):                             {okCommand()},
		commandKey("crictl", "ps", "--name", "kube-apiserver", "--state", "Running", "-q"):          {stdoutCommand("apiserver-id\n")},
		commandKey("kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "--raw=/readyz"): {stdoutCommand("ok\n")},
	}
	guest := NewGuestControl(result, client)
	guest.Timeout = time.Second

	if err := RunKubeadmAPISmoke(context.Background(), guest, KubeadmAPISmokePlan{APIServerPollInterval: time.Millisecond}); err != nil {
		t.Fatalf("RunKubeadmAPISmoke() error = %v", err)
	}

	if !client.sensitiveCommand(commandKey("crictl", "info")) {
		t.Fatalf("crictl info was not marked sensitive: %#v", client.commandRequests)
	}
	kubeadmRecord := findCommandRecord(t, result, "kubeadm-init")
	data, err := os.ReadFile(filepath.Join(kubeadmRecord, "command.json"))
	if err != nil {
		t.Fatalf("read kubeadm command record: %v", err)
	}
	if strings.Contains(string(data), `"redaction": "output"`) {
		t.Fatalf("kubeadm command record unexpectedly redacted debuggable output: %s", data)
	}
	networkctlRecord := findCommandRecord(t, result, "networkctl-status")
	networkctlData, err := os.ReadFile(filepath.Join(networkctlRecord, "command.json"))
	if err != nil {
		t.Fatalf("read networkctl command record: %v", err)
	}
	if !strings.Contains(string(networkctlData), `"allowFailure": true`) {
		t.Fatalf("network visibility command record missing allowFailure: %s", networkctlData)
	}
}

func TestKubeadmAPISmokeRetriesReadyTargetStart(t *testing.T) {
	result := guestResult(t)
	client := newScriptedGuestClient()
	client.commandResults = map[string][]*vmtestpb.CommandResult{
		commandKey("systemctl", "start", "katl-kubeadm-ready.target"): {
			failedCommand("inactive\n"),
			okCommand(),
		},
		commandKey("systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"):               {okCommand()},
		commandKey("test", "-x", "/usr/bin/katlc"):                                                 {okCommand()},
		commandKey("/usr/bin/katlc", "--help"):                                                     {stdoutCommand("Usage: katlc <command> [args]\nagent serve\n")},
		commandKey("test", "-f", DefaultKubeadmConfigPath):                                         {okCommand()},
		commandKey("findmnt", "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE"): {stdoutCommand(DefaultProjectedKubernetesPath + "\n")},
		commandKey("test", "-x", "/usr/bin/kubeadm"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/kubelet"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/kubectl"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/crictl"):                                                {okCommand()},
		commandKey("systemctl", "is-active", "--quiet", "containerd.service"):                      {okCommand()},
		commandKey("networkctl", "status", "--all"):                                                {stdoutCommand("routable\n")},
		commandKey("resolvectl", "status"):                                                         {stdoutCommand("DNS Servers: 10.0.2.3\n")},
		commandKey("ip", "route"):                                                                  {stdoutCommand("default via 10.0.2.2 dev enp0s2\n")},
		commandKey("crictl", "info"):                                                               {stdoutCommand("{}\n")},
		commandKey("getent", "hosts", "registry.k8s.io"):                                           {stdoutCommand("1.2.3.4 registry.k8s.io\n")},
		commandKey("kubeadm", "init", "--config", DefaultKubeadmConfigPath, "--skip-token-print", "--skip-phases=addon/coredns,addon/kube-proxy"): {okCommand()},
		commandKey("test", "-f", "/etc/kubernetes/admin.conf"):                                      {okCommand()},
		commandKey("test", "-f", "/etc/kubernetes/manifests/kube-apiserver.yaml"):                   {okCommand()},
		commandKey("test", "-f", "/etc/kubernetes/manifests/kube-controller-manager.yaml"):          {okCommand()},
		commandKey("test", "-f", "/etc/kubernetes/manifests/kube-scheduler.yaml"):                   {okCommand()},
		commandKey("test", "-f", "/etc/kubernetes/manifests/etcd.yaml"):                             {okCommand()},
		commandKey("crictl", "ps", "--name", "kube-apiserver", "--state", "Running", "-q"):          {stdoutCommand("apiserver-id\n")},
		commandKey("kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "--raw=/readyz"): {stdoutCommand("ok\n")},
	}
	guest := NewGuestControl(result, client)
	err := RunKubeadmAPISmoke(context.Background(), guest, KubeadmAPISmokePlan{
		ReadyPollInterval:     time.Millisecond,
		APIServerPollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("RunKubeadmAPISmoke() error = %v", err)
	}
	if got := client.commandCount(commandKey("systemctl", "start", "katl-kubeadm-ready.target")); got != 2 {
		t.Fatalf("ready target starts = %d, want 2", got)
	}
}

func TestKubeadmAPISmokeRejectsUnprojectedEtcKubernetes(t *testing.T) {
	result := guestResult(t)
	client := newScriptedGuestClient()
	client.commandResults = map[string][]*vmtestpb.CommandResult{
		commandKey("systemctl", "start", "katl-kubeadm-ready.target"):                              {okCommand()},
		commandKey("systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"):               {okCommand()},
		commandKey("test", "-x", "/usr/bin/katlc"):                                                 {okCommand()},
		commandKey("/usr/bin/katlc", "--help"):                                                     {stdoutCommand("Usage: katlc <command> [args]\nagent serve\n")},
		commandKey("test", "-f", DefaultKubeadmConfigPath):                                         {okCommand()},
		commandKey("findmnt", "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE"): {stdoutCommand("/etc/kubernetes\n")},
	}
	guest := NewGuestControl(result, client)

	err := RunKubeadmAPISmoke(context.Background(), guest, KubeadmAPISmokePlan{})
	if err == nil || !strings.Contains(err.Error(), DefaultProjectedKubernetesPath) {
		t.Fatalf("RunKubeadmAPISmoke() error = %v, want projection failure", err)
	}
}

func TestInstalledKubeadmAPISmokeCollectsDiagnosticsOnFailure(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	result, err := NewRunner(Options{StateRoot: root, RunID: "run-1"}).Plan(Scenario{Name: "kubeadm-api-smoke"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, vmConfig := vmFixture(t)
	vmConfig.Expect = runtimeBootSignal
	vmConfig.VSock.Enabled = true
	client := newScriptedGuestClient()
	client.commandResults = map[string][]*vmtestpb.CommandResult{
		commandKey("systemctl", "start", "katl-kubeadm-ready.target"):                              {okCommand()},
		commandKey("systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"):               {okCommand()},
		commandKey("test", "-x", "/usr/bin/katlc"):                                                 {okCommand()},
		commandKey("/usr/bin/katlc", "--help"):                                                     {stdoutCommand("Usage: katlc <command> [args]\nagent serve\n")},
		commandKey("test", "-f", DefaultKubeadmConfigPath):                                         {okCommand()},
		commandKey("findmnt", "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE"): {stdoutCommand(DefaultProjectedKubernetesPath + "\n")},
		commandKey("test", "-x", "/usr/bin/kubeadm"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/kubelet"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/kubectl"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/crictl"):                                                {okCommand()},
		commandKey("systemctl", "is-active", "--quiet", "containerd.service"):                      {okCommand()},
		commandKey("networkctl", "status", "--all"):                                                {stdoutCommand("routable\n")},
		commandKey("resolvectl", "status"):                                                         {stdoutCommand("DNS Servers: 10.0.2.3\n")},
		commandKey("ip", "route"):                                                                  {stdoutCommand("default via 10.0.2.2 dev enp0s2\n")},
		commandKey("crictl", "info"):                                                               {stdoutCommand("{}\n")},
		commandKey("getent", "hosts", "registry.k8s.io"):                                           {stdoutCommand("1.2.3.4 registry.k8s.io\n")},
		commandKey("kubeadm", "init", "--config", DefaultKubeadmConfigPath, "--skip-token-print", "--skip-phases=addon/coredns,addon/kube-proxy"): {failedCommand("kubeadm init failed\n")},
		commandKey("systemctl", "status", "katl-kubeadm-ready.target"):                                                                            {okCommand()},
		commandKey("systemctl", "status", "containerd.service"):                                                                                   {okCommand()},
		commandKey("systemctl", "status", "kubelet.service"):                                                                                      {okCommand()},
		commandKey("systemctl", "status", "systemd-networkd.service"):                                                                             {okCommand()},
		commandKey("crictl", "ps", "-a"):                                                                                                          {stdoutCommand("containers\n")},
		commandKey("findmnt", "--target", "/etc/kubernetes", "--output", "SOURCE,TARGET,FSTYPE,OPTIONS"):                                          {stdoutCommand(DefaultProjectedKubernetesPath + " /etc/kubernetes none rw\n")},
	}
	client.fileResults = map[string]*vmtestpb.FileResult{
		"/etc/katl/node.json":    {Content: []byte(`{"kind":"NodeMetadata"}`), SizeBytes: 23, Redaction: "sensitive"},
		DefaultKubeadmConfigPath: {Content: []byte("apiVersion: kubeadm.k8s.io/v1beta4\n"), SizeBytes: 38, Redaction: "sensitive"},
	}
	client.journal = &vmtestpb.JournalResult{Text: "diagnostic journal\n", SizeBytes: 19}
	var connects int

	runner := VMRunner{
		Executor: blockingVMExec{write: runtimeBootSignal},
		AgentConnector: func(_ context.Context, _ VSockPlan, _ string) (AgentHealthClient, error) {
			t.Fatal("health connector should not be used by kubeadm smoke")
			return nil, nil
		},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
			output:   func(string, ...string) ([]byte, error) { return []byte("vhost-vsock-pci guest-cid\n"), nil },
		},
	}
	result = RunInstalledKubeadmAPISmoke(context.Background(), result, KubeadmAPISmokeConfig{
		Runtime: InstalledRuntimeConfig{
			Disk:         disk,
			DiskFormat:   DiskRaw,
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Smoke: KubeadmAPISmokePlan{APIServerPollInterval: time.Millisecond},
		AgentConnector: func(_ context.Context, _ VSockPlan, _ string) (KubeadmSmokeAgentSession, error) {
			connects++
			return client, nil
		},
	}, runner)

	if result.Status != StatusFailed || !strings.Contains(result.FailureSummary, "kubeadm-init") {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(result.Artifacts.GuestDir, "diagnostics.json")); err != nil {
		t.Fatalf("diagnostics missing: %v", err)
	}
	if connects < 2 {
		t.Fatalf("agent connects = %d, want reconnect for diagnostics", connects)
	}
	kubeadmRecord := findCommandRecord(t, result, "kubeadm-init")
	data, err := os.ReadFile(filepath.Join(kubeadmRecord, "command.json"))
	if err != nil {
		t.Fatalf("read kubeadm command record: %v", err)
	}
	if !strings.Contains(string(data), `"stderr"`) {
		t.Fatalf("kubeadm failure did not preserve debuggable output artifact: %s", data)
	}
}

func TestInstalledKubeadmAPISmokeFailsWhenDomainExitsAfterReady(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	result, err := NewRunner(Options{StateRoot: root, RunID: "run-1"}).Plan(Scenario{Name: "kubeadm-api-smoke"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, vmConfig := vmFixture(t)
	vmConfig.Expect = runtimeBootSignal
	vmConfig.VSock.Enabled = true
	runner := VMRunner{
		Executor: vmExec{write: runtimeBootSignal},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
			output:   func(string, ...string) ([]byte, error) { return []byte("vhost-vsock-pci guest-cid\n"), nil },
		},
	}
	result = RunInstalledKubeadmAPISmoke(context.Background(), result, KubeadmAPISmokeConfig{
		Runtime: InstalledRuntimeConfig{
			Disk:         disk,
			DiskFormat:   DiskRaw,
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		AgentConnector: func(context.Context, VSockPlan, string) (KubeadmSmokeAgentSession, error) {
			t.Fatal("agent connector should not be called after libvirt domain exits")
			return nil, nil
		},
	}, runner)

	if result.Status != StatusFailed || !strings.Contains(result.FailureSummary, "libvirt domain exited after serial signal") {
		t.Fatalf("result = %#v", result)
	}
	serial, err := os.ReadFile(result.Artifacts.RuntimeSerial)
	if err != nil {
		t.Fatalf("read runtime serial: %v", err)
	}
	if !strings.Contains(string(serial), runtimeBootSignal) {
		t.Fatalf("runtime serial missing runtime boot signal: %s", serial)
	}
}

type scriptedGuestClient struct {
	commandResults  map[string][]*vmtestpb.CommandResult
	commandRequests []*vmtestpb.RunCommandRequest
	fileResults     map[string]*vmtestpb.FileResult
	journal         *vmtestpb.JournalResult
	closed          bool
}

func newScriptedGuestClient() *scriptedGuestClient {
	return &scriptedGuestClient{
		commandResults: map[string][]*vmtestpb.CommandResult{},
		fileResults:    map[string]*vmtestpb.FileResult{},
	}
}

func (c *scriptedGuestClient) RunCommand(_ context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	c.commandRequests = append(c.commandRequests, req)
	key := commandKey(req.Argv...)
	results := c.commandResults[key]
	if len(results) == 0 {
		return &vmtestpb.CommandResult{ExitStatus: 127, Stderr: []byte("unexpected command: " + strings.Join(req.Argv, " "))}, nil
	}
	result := results[0]
	c.commandResults[key] = results[1:]
	return result, nil
}

func (c *scriptedGuestClient) ReadFile(_ context.Context, req *vmtestpb.ReadFileRequest) (*vmtestpb.FileResult, error) {
	result, ok := c.fileResults[req.Path]
	if !ok {
		return nil, errors.New("unexpected file read: " + req.Path)
	}
	return result, nil
}

func (c *scriptedGuestClient) WriteFile(_ context.Context, req *vmtestpb.WriteFileRequest) (*vmtestpb.WriteFileResult, error) {
	return nil, errors.New("unexpected file write: " + req.Path)
}

func (c *scriptedGuestClient) ExportJournal(context.Context, *vmtestpb.ExportJournalRequest) (*vmtestpb.JournalResult, error) {
	if c.journal == nil {
		return &vmtestpb.JournalResult{}, nil
	}
	return c.journal, nil
}

func (c *scriptedGuestClient) Close() error {
	c.closed = true
	return nil
}

func (c *scriptedGuestClient) sensitiveCommand(want string) bool {
	for _, req := range c.commandRequests {
		if commandKey(req.Argv...) == want {
			return req.SensitiveOutput
		}
	}
	return false
}

func (c *scriptedGuestClient) commandCount(want string) int {
	var count int
	for _, req := range c.commandRequests {
		if commandKey(req.Argv...) == want {
			count++
		}
	}
	return count
}

type blockingVMExec struct {
	write string
}

func (e blockingVMExec) Run(ctx context.Context, _ string, _ []string, serial io.Writer) error {
	if e.write != "" {
		_, _ = io.WriteString(serial, e.write)
	}
	<-ctx.Done()
	return ctx.Err()
}

func okCommand() *vmtestpb.CommandResult {
	return &vmtestpb.CommandResult{ExitStatus: 0}
}

func stdoutCommand(stdout string) *vmtestpb.CommandResult {
	return &vmtestpb.CommandResult{ExitStatus: 0, Stdout: []byte(stdout), StdoutBytes: uint32(len(stdout))}
}

func failedCommand(stderr string) *vmtestpb.CommandResult {
	return &vmtestpb.CommandResult{ExitStatus: 1, Stderr: []byte(stderr), StderrBytes: uint32(len(stderr))}
}

func commandKey(argv ...string) string {
	return strings.Join(argv, "\x00")
}

func findCommandRecord(t *testing.T, result Result, name string) string {
	t.Helper()
	root := filepath.Join(result.Artifacts.GuestDir, "commands")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read commands dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), name) {
			return filepath.Join(root, entry.Name())
		}
	}
	t.Fatalf("command record %q not found in %#v", name, entries)
	return ""
}
