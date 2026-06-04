package vmtest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	vmtestpb "github.com/zariel/katl/internal/vmtest/proto"
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
		Argv:            []string{"systemctl", "is-active", "containerd.service"},
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
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if strings.Contains(string(data), "secret output") {
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

func TestAgentCommandAllowlist(t *testing.T) {
	server := NewAgentServer("test")
	_, err := server.runCommand(context.Background(), &vmtestpb.RunCommandRequest{
		Argv: []string{"sh", "-c", "true"},
	})
	if err == nil || !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("runCommand() error = %v", err)
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
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
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
