package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestUpgradeFlavour(t *testing.T) {
	for _, tc := range []struct{ name, installed, flag, asset, generation string }{
		{"existing standard", "", "", "katlos-upgrade-2026.9.0-x86_64.squashfs", "katlos-2026.9.0"},
		{"default from lts", "lts", "", "katlos-upgrade-2026.9.0-x86_64.squashfs", "katlos-2026.9.0"},
		{"explicit lts", "lts", "lts", "katlos-lts-upgrade-2026.9.0-x86_64.squashfs", "katlos-lts-2026.9.0"},
		{"switch to lts", "standard", "lts", "katlos-lts-upgrade-2026.9.0-x86_64.squashfs", "katlos-lts-2026.9.0"},
		{"switch to standard", "lts", "standard", "katlos-upgrade-2026.9.0-x86_64.squashfs", "katlos-2026.9.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := readyHostUpgradeClient()
			fake.generation.RuntimeFlavour = tc.installed
			installKatlcDial(t, func(string) {}, fake)
			args := []string{"node", "upgrade", "cp-1", "--config", writeClusterConfig(t), "--version", "2026.9.0", "--plan"}
			if tc.flag != "" {
				args = append(args, "--flavour", tc.flag)
			}
			var out, errs bytes.Buffer
			if err := run(context.Background(), args, &out, &errs); err != nil {
				t.Fatal(err)
			}
			request := fake.submitRequest.GetHostUpgrade()
			if got := request.GetImageUrl(); got != "https://github.com/katl-dev/katl/releases/download/v2026.9.0/"+tc.asset {
				t.Fatalf("image = %s", got)
			}
			if got := request.GetCandidateGenerationId(); !strings.HasPrefix(got, tc.generation+"-") {
				t.Fatalf("generation = %s", got)
			}
		})
	}
}

func TestUpgradeFlavourConflict(t *testing.T) {
	artifact, _, _ := writeHostUpgradeArtifact(t, "2026.9.0", "x86_64", 10)
	var out, errs bytes.Buffer
	err := run(context.Background(), []string{"node", "upgrade", "cp-1", "--artifact", artifact, "--flavour", "lts"}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "conflicts with local image flavour standard") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpgradeLocalLTSRequiresFlavour(t *testing.T) {
	artifact, _, _ := writeHostUpgradeArtifact(t, "2026.9.0", "x86_64", 10)
	data, err := os.ReadFile(artifact + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["flavour"] = "lts"
	data, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact+".json", data, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errs bytes.Buffer
	err = run(context.Background(), []string{"node", "upgrade", "cp-1", "--artifact", artifact}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "conflicts with local image flavour lts") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpgradeFlavourRequiresCapableAgent(t *testing.T) {
	fake := readyHostUpgradeClient()
	fake.generation.RuntimeFlavour = ""
	installKatlcDial(t, func(string) {}, fake)
	var out, errs bytes.Buffer
	err := run(context.Background(), []string{"node", "upgrade", "cp-1", "--config", writeClusterConfig(t), "--version", "2026.9.0", "--flavour", "lts"}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "first upgrade it to the standard flavour") {
		t.Fatalf("error = %v", err)
	}
	if fake.submitRequest != nil || len(fake.stageArtifact) != 0 {
		t.Fatal("unsupported agent received an upgrade mutation")
	}
}

func TestUpgradeGenerationDistinguishesReturnTransition(t *testing.T) {
	fake := readyHostUpgradeClient()
	installKatlcDial(t, func(string) {}, fake)
	config := writeClusterConfig(t)
	plan := func(from string) string {
		t.Helper()
		fake.generation.GenerationId = from
		fake.generation.RuntimeFlavour = "lts"
		fake.nodeStatus.CurrentGenerationId = from
		var out, errs bytes.Buffer
		if err := run(context.Background(), []string{"node", "upgrade", "cp-1", "--config", config, "--version", "2026.9.0", "--flavour", "standard", "--plan"}, &out, &errs); err != nil {
			t.Fatal(err)
		}
		return fake.submitRequest.GetHostUpgrade().GetCandidateGenerationId()
	}
	first := plan("install-0")
	if again := plan("install-0"); again != first {
		t.Fatal("repeating the same plan changed generation identity")
	}
	if returning := plan("lts-after-config"); returning == first {
		t.Fatal("returning to a previously used version reuses its old generation")
	}
}

func TestUpgradeAlreadyInstalledAfterConfigApply(t *testing.T) {
	fake := readyHostUpgradeClient()
	fake.generation.RuntimeVersion = "2026.9.0"
	fake.generation.RuntimeFlavour = "lts"
	fake.generation.CommitState, fake.generation.BootState, fake.generation.HealthState = "committed", "good", "healthy"
	installKatlcDial(t, func(string) {}, fake)
	var out, errs bytes.Buffer
	if err := run(context.Background(), []string{"node", "upgrade", "cp-1", "--config", writeClusterConfig(t), "--version", "2026.9.0", "--flavour", "lts"}, &out, &errs); err != nil {
		t.Fatal(err)
	}
	if fake.submitRequest != nil || len(fake.rebootRequests) != 0 {
		t.Fatal("already-installed version and flavour caused a mutation")
	}
}
