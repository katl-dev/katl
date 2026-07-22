package vmtest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

func TestAgentClientServer(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	server := NewAgentServer("test-version")
	server.Hostname = func() (string, error) { return "katl-test", nil }
	server.BootID = func() string { return "boot-1" }
	server.CommandRunner = fakeAgentRunner{result: &vmtestpb.CommandResult{
		ExitStatus:  0,
		Stdout:      []byte("secret output"),
		StdoutBytes: 13,
	}}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(context.Background(), serverConn)
	}()

	transcript := filepath.Join(t.TempDir(), "vsock-transcript.jsonl")
	client := NewAgentClient(clientConn, transcript)
	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if health.AgentVersion != "test-version" || health.Hostname != "katl-test" || health.BootId != "boot-1" {
		t.Fatalf("health = %#v", health)
	}
	result, err := client.RunCommand(context.Background(), &vmtestpb.RunCommandRequest{
		Argv:            []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--discovery-token-ca-cert-hash=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		SensitiveOutput: true,
	})
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	if string(result.Stdout) != "secret output" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	entries := readTranscript(t, transcript)
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[1].Method != "RunCommand" || entries[1].StdoutBytes != 13 || entries[1].Redaction != "output" {
		t.Fatalf("command transcript = %#v", entries[1])
	}
	wantArgv := []string{"kubeadm", "join", "api.katl.test:6443", "--token", "[REDACTED BOOTSTRAP TOKEN]", "--discovery-token-ca-cert-hash=[REDACTED DISCOVERY TOKEN HASH]"}
	if !reflect.DeepEqual(entries[1].Argv, wantArgv) {
		t.Fatalf("command argv = %#v, want %#v", entries[1].Argv, wantArgv)
	}
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if strings.Contains(string(data), "secret output") || strings.Contains(string(data), "abcdef.0123456789abcdef") || strings.Contains(string(data), "sha256:0123456789abcdef") {
		t.Fatalf("transcript leaked sensitive output: %s", data)
	}
}

func TestAgentReadFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.txt")
	if err := os.WriteFile(path, []byte("abcdef"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	server := NewAgentServer("test")
	server.AllowedFilePaths = []string{root + string(os.PathSeparator)}
	result, err := server.readFile(&vmtestpb.ReadFileRequest{
		Path:     path,
		MaxBytes: 3,
	})
	if err != nil {
		t.Fatalf("readFile() error = %v", err)
	}
	if string(result.Content) != "abc" || !result.Truncated || result.SizeBytes != 3 {
		t.Fatalf("result = %#v", result)
	}
	_, err = server.readFile(&vmtestpb.ReadFileRequest{Path: filepath.Join(t.TempDir(), "blocked")})
	if err == nil || !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("blocked read error = %v", err)
	}
}

func TestAgentWriteFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "requests", "change.yaml")
	server := NewAgentServer("test")
	server.AllowedWritePaths = []string{root + string(os.PathSeparator)}
	result, err := server.writeFile(&vmtestpb.WriteFileRequest{
		Path:    path,
		Content: []byte("apiVersion: katl.dev/v1alpha1\n"),
		Mode:    0o600,
	})
	if err != nil {
		t.Fatalf("writeFile() error = %v", err)
	}
	if result.SizeBytes != 30 || result.Redaction != "none" {
		t.Fatalf("result = %#v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(data) != "apiVersion: katl.dev/v1alpha1\n" {
		t.Fatalf("written data = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	_, err = server.writeFile(&vmtestpb.WriteFileRequest{Path: filepath.Join(t.TempDir(), "blocked"), Content: []byte("x")})
	if err == nil || !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("blocked write error = %v", err)
	}
}

func TestAgentWriteFileChunks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "artifacts", "upgrade.squashfs")
	server := NewAgentServer("test")
	server.AllowedWritePaths = []string{root + string(os.PathSeparator)}
	for _, request := range []*vmtestpb.WriteFileRequest{
		{Path: path, Content: []byte("first"), Mode: 0o600, Truncate: true},
		{Path: path, Content: []byte("-second"), Mode: 0o600, Offset: 5},
	} {
		if _, err := server.writeFile(request); err != nil {
			t.Fatalf("writeFile(%d) error = %v", request.Offset, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chunked file: %v", err)
	}
	if string(data) != "first-second" {
		t.Fatalf("chunked data = %q", data)
	}
	if _, err := server.writeFile(&vmtestpb.WriteFileRequest{Path: path, Content: []byte("gap"), Offset: 99}); err == nil || !strings.Contains(err.Error(), "exceeds current size") {
		t.Fatalf("sparse chunk error = %v", err)
	}
}

func TestAgentWriteFileDefaultAllowlistSupportsKatlMetadata(t *testing.T) {
	for _, path := range []string{
		"/var/lib/katl/boot/selection.json",
		"/var/lib/katl/generations/2026.06.06-001/metadata.json",
		"/var/lib/katl/test-artifacts/kubernetes-bundle-ca.pem",
		"/var/lib/katl/test-artifacts/sysupdate/source/SHA256SUMS",
	} {
		if !pathAllowed(path, defaultAgentWritePaths()) {
			t.Fatalf("%s is not write-allowlisted", path)
		}
	}
	blocked := "/var/lib/katl/config-requests/operator/1.json"
	if pathAllowed(blocked, defaultAgentWritePaths()) {
		t.Fatalf("%s is write-allowlisted", blocked)
	}
}

func TestAgentWriteFileRejectsSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	server := NewAgentServer("test")
	server.AllowedWritePaths = []string{root + string(os.PathSeparator)}
	_, err := server.writeFile(&vmtestpb.WriteFileRequest{Path: link, Content: []byte("new")})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("writeFile() error = %v, want symlink rejection", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("target data = %q", data)
	}
}

func TestAgentWriteFileRejectsSymlinkParentWithoutCreatingChild(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	server := NewAgentServer("test")
	server.AllowedWritePaths = []string{root + string(os.PathSeparator)}
	_, err := server.writeFile(&vmtestpb.WriteFileRequest{Path: filepath.Join(link, "child", "request.yaml"), Content: []byte("x")})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("writeFile() error = %v, want symlink parent rejection", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "child")); !os.IsNotExist(err) {
		t.Fatalf("writeFile created child through symlink: %v", err)
	}
}

func TestAgentCommandAllowlist(t *testing.T) {
	server := NewAgentServer("test")
	_, err := server.runCommand(context.Background(), &vmtestpb.RunCommandRequest{
		Argv: []string{"sh", "-c", "true"},
	})
	if err == nil || !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("runCommand() error = %v", err)
	}
}

func TestAgentDefaultAllowlistSupportsBootstrapReadiness(t *testing.T) {
	for _, command := range []string{"bgp-api-vip-smoke", "blkid", "chmod", "crictl", "ctr", "dd", "find", "findmnt", "install", "katlc", "kubeadm", "kubectl", "kubelet", "lsmod", "modprobe", "mount", "partx", "sfdisk", "sha256sum", "sshd", "systemd-sysupdate", "test", "udevadm"} {
		if !commandAllowed(command, defaultAgentCommands()) {
			t.Fatalf("%s is not allowlisted", command)
		}
	}
	for _, path := range []string{
		"/etc/katl/node.json",
		"/etc/katl/kubeadm/control-plane/config.yaml",
		"/etc/machine-id",
		"/etc/kubernetes/admin.conf",
		"/etc/kubernetes/kubelet.conf",
		"/var/lib/katl/boot/selection.json",
		"/var/lib/katl/identity/machine-id",
		"/var/lib/katl/install/status.json",
		"/var/lib/katl/operations/bootstrap-init-1/record.json",
	} {
		if !pathAllowed(path, defaultAgentFilePaths()) {
			t.Fatalf("%s is not allowlisted", path)
		}
	}
}

func TestAgentResponseError(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	server := NewAgentServer("test")
	server.CommandRunner = fakeAgentRunner{err: errors.New("boom")}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(context.Background(), serverConn)
	}()

	client := NewAgentClient(clientConn, "")
	_, err := client.RunCommand(context.Background(), &vmtestpb.RunCommandRequest{Argv: []string{"systemctl"}})
	if err == nil || !strings.Contains(err.Error(), "command_failed") {
		t.Fatalf("RunCommand() error = %v", err)
	}
	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
}

func TestAgentClientHonorsRequestTimeout(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	server := NewAgentServer("test-version")
	server.CommandRunner = delayedAgentRunner{
		delay: 40 * time.Millisecond,
		result: &vmtestpb.CommandResult{
			ExitStatus: 0,
			Stdout:     []byte("ok\n"),
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(context.Background(), serverConn)
	}()

	client := NewAgentClient(clientConn, "")
	client.DefaultTimeout = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result, err := client.RunCommand(ctx, &vmtestpb.RunCommandRequest{Argv: []string{"true"}})
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	if string(result.Stdout) != "ok\n" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
}

func TestVMRunnerChecksAgentAfterSerial(t *testing.T) {
	result, config := vmFixture(t)
	config.Expect = "runtime ready"
	config.VSock.Enabled = true
	config.VSock.GuestCID = 4096
	config.Agent.RequireHealth = true
	config.Agent.Timeout = time.Second
	checked := false
	runner := VMRunner{
		Executor: vmExec{write: "runtime ready"},
		AgentConnector: func(_ context.Context, plan VSockPlan, transcript string) (AgentHealthClient, error) {
			if plan.GuestCID != 4096 || plan.Port != DefaultAgentPort {
				t.Fatalf("plan = %#v", plan)
			}
			if transcript != result.Artifacts.VSockTranscript {
				t.Fatalf("transcript = %q", transcript)
			}
			checked = true
			return fakeHealthClient{}, nil
		},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
			output: func(string, ...string) ([]byte, error) {
				return []byte("vhost-vsock-pci guest-cid=<uint32>"), nil
			},
		},
	}
	result = runner.Run(context.Background(), result, config)
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q failure=%q", result.Status, result.FailureSummary)
	}
	if !checked {
		t.Fatal("agent health was not checked")
	}
}

type fakeAgentRunner struct {
	result *vmtestpb.CommandResult
	err    error
}

func (r fakeAgentRunner) Run(context.Context, *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.result, nil
}

type delayedAgentRunner struct {
	delay  time.Duration
	result *vmtestpb.CommandResult
}

func (r delayedAgentRunner) Run(ctx context.Context, _ *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(r.delay):
		return r.result, nil
	}
}

type fakeHealthClient struct{}

func (fakeHealthClient) Health(context.Context) error { return nil }
func (fakeHealthClient) Close() error                 { return nil }

func readTranscript(t *testing.T, path string) []transcriptEntry {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer file.Close()
	var entries []transcriptEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry transcriptEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("decode transcript: %v", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan transcript: %v", err)
	}
	return entries
}
