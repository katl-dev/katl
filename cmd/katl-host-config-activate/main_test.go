package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestRunVerifiesHostConfigurationAndPersistsEffects(t *testing.T) {
	root := t.TempDir()
	value := "1"
	config := manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		Node: manifest.NodeConfig{
			Identity:   manifest.NodeIdentity{Hostname: "node-1", SSH: manifest.SSHIdentity{AuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"}}},
			SystemRole: "worker",
			HostConfiguration: manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
				"sysctl": {Files: []manifest.HostConfigurationFile{{Path: "/etc/sysctl.d/80-test.conf", Content: stringPointer("example.value = 1\n")}}},
			}},
		},
		Install: manifest.InstallConfig{TargetDisk: manifest.DiskSelector{ByID: "/dev/disk/by-id/test"}, WipeTarget: true},
		KatlosImage: manifest.KatlosImage{
			URL: "https://example.invalid/katlos.raw", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SizeBytes: 1, Version: "test", Architecture: "x86_64", Role: "install",
		},
	}
	if err := configapply.WriteGenerationManifest(root, "generation-1", config); err != nil {
		t.Fatal(err)
	}
	status, err := generation.NewConfigApplyStatus(generation.ConfigApplyStatusRequest{
		GenerationID: "generation-1", PreviousGeneration: "generation-0",
		RequestedApplyMode: generation.ApplyModeAuto, AcceptedApplyMode: generation.ApplyModeNextBoot,
		ChangedDomains: []string{configapply.DomainHostConfiguration}, HealthState: generation.HealthStateUnknown,
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	status.DomainActions = []generation.ConfigApplyDomainAction{{
		Domain: configapply.DomainHostConfiguration, Action: "native-host-configuration-boot-apply", Status: generation.ConfigApplyActionPlanned,
		Effects: []generation.ConfigApplyEffect{{Action: "apply-and-verify", Target: "sysctl example.value", Status: generation.ConfigApplyActionPlanned}},
	}}
	statusPath, err := generation.ConfigApplyStatusPath(root, "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteConfigApplyStatus(statusPath, status); err != nil {
		t.Fatal(err)
	}
	previousRunner := hostConfigRunner
	defer func() { hostConfigRunner = previousRunner }()
	hostConfigRunner = commandRunnerFunc(func(command configapply.Command) configapply.CommandResult {
		return configapply.CommandResult{Stdout: value + "\n"}
	})
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"--root", root, "--generation", "generation-1", "--phase", "verify"}, &stdout); err != nil {
		t.Fatal(err)
	}
	persisted, err := generation.ReadConfigApplyStatus(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	action := persisted.DomainActions[0]
	if action.Status != generation.ConfigApplyActionPassed || action.Effects[0].Status != generation.ConfigApplyActionPassed {
		t.Fatalf("action = %#v", action)
	}
	if stdout.String() != "katl-host-config-activate generation=generation-1 phase=verify effects=1\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if filepath.Base(statusPath) != "config-apply-status.json" {
		t.Fatalf("status path = %q", statusPath)
	}
}

type commandRunnerFunc func(configapply.Command) configapply.CommandResult

func (f commandRunnerFunc) Run(_ context.Context, command configapply.Command) (configapply.CommandResult, error) {
	return f(command), nil
}

func stringPointer(value string) *string {
	return &value
}
