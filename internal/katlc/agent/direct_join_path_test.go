package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDirectControlPlaneJoinPathMapsCanonicalNameToCoordinator(t *testing.T) {
	root := t.TempDir()
	hostsPath := filepath.Join(root, "etc/hosts")
	writeTestFile(t, hostsPath, "127.0.0.1 localhost\n")
	discoveryPath := writeDirectJoinDiscovery(t, root, "https://192.0.2.11:6443")
	var commands [][]string
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, slices.Clone(argv))
		return ToolResult{}
	}

	path, err := installDirectControlPlaneJoinPath(context.Background(), root, "api.unpublished.katl.test:6443", discoveryPath, run)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path.hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "192.0.2.11 api.unpublished.katl.test "+directJoinHostsMarker) {
		t.Fatalf("hosts = %s", data)
	}
	if !slices.Equal(commands[0], []string{directJoinMount, "--bind", path.hostsPath, hostsPath}) {
		t.Fatalf("mount command = %v", commands[0])
	}
	if err := path.cleanup(context.Background(), root, run); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "127.0.0.1 localhost\n" {
		t.Fatalf("hosts after cleanup = %q", data)
	}
	if len(commands) != 2 || !slices.Equal(commands[1], []string{directJoinUnmount, hostsPath}) {
		t.Fatalf("commands = %v", commands)
	}
	if _, err := os.Stat(filepath.Join(root, "run/katl/bootstrap-join/test/hosts")); !os.IsNotExist(err) {
		t.Fatalf("temporary hosts file still exists: %v", err)
	}
}

func TestDirectControlPlaneJoinPathRedirectsCanonicalAddress(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc/hosts"), "127.0.0.1 localhost\n")
	discoveryPath := writeDirectJoinDiscovery(t, root, "https://192.0.2.11:6443")
	var commands [][]string
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, slices.Clone(argv))
		return ToolResult{}
	}

	path, err := installDirectControlPlaneJoinPath(context.Background(), root, "192.0.2.100:7443", discoveryPath, run)
	if err != nil {
		t.Fatal(err)
	}
	if err := path.cleanup(context.Background(), root, run); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 5 {
		t.Fatalf("commands = %v", commands)
	}
	wantRule := []string{directJoinNFT, "add", "rule", "inet", directJoinNFTTable, "output", "ip", "daddr", "192.0.2.100", "tcp", "dport", "7443", "dnat", "to", "192.0.2.11:6443"}
	if !slices.Equal(commands[3], wantRule) {
		t.Fatalf("rule = %v, want %v", commands[3], wantRule)
	}
	if !slices.Equal(commands[0], []string{directJoinNFT, "destroy", "table", "inet", directJoinNFTTable}) || !slices.Equal(commands[4], commands[0]) {
		t.Fatalf("cleanup commands = %v", commands)
	}
}

func TestDirectControlPlaneJoinPathCleansHostsWhenRedirectFails(t *testing.T) {
	root := t.TempDir()
	hostsPath := filepath.Join(root, "etc/hosts")
	writeTestFile(t, hostsPath, "127.0.0.1 localhost\n")
	discoveryPath := writeDirectJoinDiscovery(t, root, "https://192.0.2.11:6443")
	var commands [][]string
	_, err := installDirectControlPlaneJoinPath(context.Background(), root, "api.unpublished.katl.test:7443", discoveryPath, func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, slices.Clone(argv))
		if slices.Contains(argv, "chain") {
			return ToolResult{ExitStatus: 1, Stderr: []byte("injected failure")}
		}
		return ToolResult{}
	})
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("error = %v", err)
	}
	data, readErr := os.ReadFile(hostsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "127.0.0.1 localhost\n" {
		t.Fatalf("hosts after failed setup = %q", data)
	}
	if len(commands) != 6 || !slices.Equal(commands[len(commands)-2], []string{directJoinNFT, "destroy", "table", "inet", directJoinNFTTable}) || !slices.Equal(commands[len(commands)-1], []string{directJoinUnmount, hostsPath}) {
		t.Fatalf("commands = %v", commands)
	}
}

func writeDirectJoinDiscovery(t *testing.T, root, server string) string {
	t.Helper()
	path := "/run/katl/bootstrap-join/test/discovery.conf"
	writeTestFile(t, filepath.Join(root, path), "apiVersion: v1\nkind: Config\nclusters:\n  - name: katl-discovery\n    cluster:\n      server: "+server+"\n")
	return path
}
