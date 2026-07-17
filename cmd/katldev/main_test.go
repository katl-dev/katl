package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRootAndInstallerCommandsShowHelpWithoutArguments(t *testing.T) {
	for _, test := range []struct {
		args []string
		want []string
	}{
		{want: []string{"installer"}},
		{args: []string{"installer"}, want: []string{"start", "reset", "status", "console", "stop"}},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), test.args, &stdout, &stderr); err != nil {
			t.Fatalf("run(%v) error = %v, stderr=%s", test.args, err, stderr.String())
		}
		help := stdout.String()
		for _, want := range test.want {
			if !strings.Contains(help, want) {
				t.Fatalf("run(%v) help missing %q:\n%s", test.args, want, help)
			}
		}
	}
}

func TestInstallerIdentityIsStableAndCheckoutScoped(t *testing.T) {
	runA, macA := installerIdentity("/work/katl-a")
	runA2, macA2 := installerIdentity("/work/katl-a")
	runB, macB := installerIdentity("/work/katl-b")
	if runA != runA2 || macA != macA2 {
		t.Fatalf("identity is not stable: %q %q, %q %q", runA, macA, runA2, macA2)
	}
	if runA == runB || macA == macB {
		t.Fatalf("checkout identities collide: %q %q", runA, macA)
	}
	if !strings.HasPrefix(runA, "dev-installer-") || !strings.HasPrefix(macA, "52:54:00:") {
		t.Fatalf("identity = %q %q", runA, macA)
	}
}

func TestInstallerStateRoundTripAndReadyGuidance(t *testing.T) {
	repo := t.TempDir()
	var stdout bytes.Buffer
	manager := installerManager{repoRoot: repo, stdout: &stdout, stderr: &bytes.Buffer{}}
	state := installerState{
		APIVersion: installerStateAPIVersion,
		Kind:       installerStateKind,
		RepoRoot:   repo,
		DomainName: "katl-dev-installer-test",
		Endpoint:   "http://192.0.2.42:8080",
	}
	if err := manager.writeState(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := manager.readState()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DomainName != state.DomainName || loaded.Endpoint != state.Endpoint {
		t.Fatalf("loaded state = %#v", loaded)
	}
	if err := manager.printReady(loaded); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(repo, "_build", "katldev", "cluster.yaml")
	for _, want := range []string{
		"KatlOS installer VM is ready.",
		"katlctl config init " + configPath + " --installer http://192.0.2.42:8080",
		"katlctl install apply --config " + configPath + " --endpoint http://192.0.2.42:8080",
		"katldev installer reset",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("ready output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestInstallerOptionsRejectUnsafeOrUnusableVMs(t *testing.T) {
	for _, test := range []struct {
		name string
		opts installerOptions
		want string
	}{
		{name: "memory", opts: installerOptions{MemoryMiB: 512, CPUs: 2, DiskSize: "32G", Timeout: time.Second}, want: "--memory"},
		{name: "CPUs", opts: installerOptions{MemoryMiB: 4096, DiskSize: "32G", Timeout: time.Second}, want: "--cpus"},
		{name: "disk", opts: installerOptions{MemoryMiB: 4096, CPUs: 2, Timeout: time.Second}, want: "--disk-size"},
		{name: "timeout", opts: installerOptions{MemoryMiB: 4096, CPUs: 2, DiskSize: "32G"}, want: "--timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateInstallerOptions(test.opts); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateInstallerOptions() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestInstallerAcceptingConfigStates(t *testing.T) {
	for _, state := range []string{"waiting", "waiting-for-config"} {
		if !installerAcceptingConfig(state) {
			t.Fatalf("installerAcceptingConfig(%q) = false", state)
		}
	}
	for _, state := range []string{"", "install-starting", "reboot-requested"} {
		if installerAcceptingConfig(state) {
			t.Fatalf("installerAcceptingConfig(%q) = true", state)
		}
	}
}

func TestDomainOwner(t *testing.T) {
	owner, err := domainOwner([]byte(`<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">katl/katldev-installer</vmtest>`))
	if err != nil || owner != installerDomainMetadata {
		t.Fatalf("domainOwner() = %q, %v", owner, err)
	}
	if _, err := domainOwner([]byte(`<vmtest>`)); err == nil {
		t.Fatal("domainOwner() accepted malformed metadata")
	}
}
