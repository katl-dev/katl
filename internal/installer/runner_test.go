package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/kubeadmconfig"
)

func TestDefaultPlanOrder(t *testing.T) {
	want := []StepID{
		DiscoverInstallerInput,
		WaitForLocalConfig,
		LoadManifest,
		SelectNode,
		CollectHardwareFacts,
		VerifyTrust,
		PlanInstall,
		PrepareDisk,
		CreatePartitions,
		FormatFilesystems,
		MountTarget,
		InstallRootSlot,
		InstallBootArtifacts,
		InstallExtensions,
		InstallSeed,
		InstallMountUnits,
		WriteInstallRecord,
		VerifyTarget,
		Reboot,
	}

	if got := DefaultPlan().IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultPlan IDs = %#v, want %#v", got, want)
	}
}

func TestPreseededManifestPlanSkipsLocalConfigWait(t *testing.T) {
	want := []StepID{
		DiscoverInstallerInput,
		LoadManifest,
		SelectNode,
		CollectHardwareFacts,
		VerifyTrust,
		PlanInstall,
		PrepareDisk,
		CreatePartitions,
		FormatFilesystems,
		MountTarget,
		InstallRootSlot,
		InstallBootArtifacts,
		InstallExtensions,
		InstallSeed,
		InstallMountUnits,
		WriteInstallRecord,
		VerifyTarget,
		Reboot,
	}

	if got := PreseededManifestPlan().IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PreseededManifestPlan IDs = %#v, want %#v", got, want)
	}
}

func TestRunnerRecordsCheckpointsWithoutCommands(t *testing.T) {
	store := &MemoryStateStore{}
	commands := &NoopCommandRunner{}
	install := &Context{
		ManifestPath:   writeManifest(t),
		StateDir:       t.TempDir(),
		TargetRoot:     t.TempDir(),
		LoaderRecord:   minimalRecord("2026.06.04-000"),
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		Commands:       commands,
		Store:          store,
		Chown:          func(string, int, int) error { return nil },
	}

	if err := NewRunner(DefaultPlan(), install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := DefaultPlan().IDs()
	if !reflect.DeepEqual(install.Completed, want) {
		t.Fatalf("completed steps = %#v, want %#v", install.Completed, want)
	}
	if len(store.Checkpoints) != len(want) {
		t.Fatalf("checkpoint count = %d, want %d", len(store.Checkpoints), len(want))
	}
	if got := store.Checkpoints[len(store.Checkpoints)-1].CompletedSteps; !reflect.DeepEqual(got, want) {
		t.Fatalf("final checkpoint completed steps = %#v, want %#v", got, want)
	}
	if len(commands.Calls) != 0 {
		t.Fatalf("command runner was called during scaffold run: %#v", commands.Calls)
	}
}

func TestRunnerInstallsIdentity(t *testing.T) {
	store := &MemoryStateStore{}
	targetRoot := t.TempDir()
	bootRoot := t.TempDir()
	record := generation.Record{
		GenerationID:   "2026.06.01-005",
		RuntimeVersion: "0.1.0",
		Root: generation.RootSelection{
			Slot:          "root-a",
			PartitionUUID: "11111111-2222-3333-4444-555555555555",
		},
		Boot: generation.BootSelection{
			UKIPath: "/efi/EFI/Linux/katl.efi",
		},
	}
	install := &Context{
		ManifestPath:   writeManifest(t),
		StateDir:       t.TempDir(),
		TargetRoot:     targetRoot,
		BootRoot:       bootRoot,
		LoaderRecord:   &record,
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		Commands:       &NoopCommandRunner{},
		Store:          store,
		Chown:          func(string, int, int) error { return nil },
	}

	if err := NewRunner(PreseededManifestPlan(), install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	machineID := "30313233343536373839616263646566"
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/identity/machine-id"), machineID+"\n")
	assertText(t, filepath.Join(targetRoot, "etc/ssh/authorized_keys/katl"), sshKey+"\n")
	assertContains(t, filepath.Join(targetRoot, "etc/ssh/sshd_config.d/10-katl.conf"), "AllowUsers katl")
	assertContains(t, filepath.Join(bootRoot, "loader/entries/katl-2026.06.01-005.conf"), "systemd.machine_id="+machineID)
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.01-005/confext/etc/extension-release.d/extension-release.katl-node"), "ID=katl\nVERSION_ID=0.1.0\nCONFEXT_LEVEL=1\n")
}

func TestRunnerMaterializesInstallRecord(t *testing.T) {
	store := &MemoryStateStore{}
	targetRoot := t.TempDir()
	bootRoot := t.TempDir()
	record := generation.Record{
		GenerationID:   "2026.06.04-001",
		RuntimeVersion: "0.1.0",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeVersion:        "0.1.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: strings.Repeat("a", 64),
		},
		Boot: generation.BootSelection{
			UKIPath: "/efi/EFI/Linux/katl-2026.06.04-001.efi",
		},
		Sysexts: []generation.ExtensionRef{
			{
				Name:            "kubernetes",
				Path:            "/var/lib/katl/generations/2026.06.04-001/sysext/kubernetes.raw",
				ActivationPath:  "/run/extensions/kubernetes.raw",
				SHA256:          strings.Repeat("b", 64),
				ArtifactVersion: "k8s-v1.34.8",
				PayloadVersion:  "v1.34.8",
				Architecture:    "x86_64",
				Compatibility: generation.ExtensionCompatibility{
					RuntimeInterfaces: []string{"katl-runtime-1"},
				},
			},
		},
		Confexts: []generation.GeneratedConfext{
			{
				Name: "stale-node",
				Compatibility: generation.ConfextCompatibility{
					ID:           "stale",
					VersionID:    "9.9.9",
					ConfextLevel: 9,
				},
			},
		},
	}
	install := &Context{
		ManifestPath: writeManifestWithNode(t, `,
			"networkd": {
				"files": [
					{"name": "10-lan.network", "content": "[Match]\nName=enp1s0\n"}
				]
			},
			"kubernetes": {
				"kubeadm": {"configRef": "control-plane"}
			}`),
		StateDir:       t.TempDir(),
		TargetRoot:     targetRoot,
		BootRoot:       bootRoot,
		LoaderRecord:   &record,
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		Commands:       &NoopCommandRunner{},
		Store:          store,
		KubeadmConfigs: kubeadmPlans(),
		Chown:          func(string, int, int) error { return nil },
	}

	if err := NewRunner(PreseededManifestPlan(), install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	confextDir := filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.04-001/confext")
	assertText(t, filepath.Join(confextDir, "etc/systemd/network/10-lan.network"), "[Match]\nName=enp1s0\n")
	assertText(t, filepath.Join(confextDir, "etc/katl/kubeadm/control-plane/config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n")
	assertText(t, filepath.Join(confextDir, "etc/extension-release.d/extension-release.katl-node"), "ID=katl\nVERSION_ID=0.1.0\nCONFEXT_LEVEL=1\n")

	digest, err := generation.DigestDirectory(confextDir)
	if err != nil {
		t.Fatalf("DigestDirectory() error = %v", err)
	}
	metadataPath := filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.04-001/metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var decoded generation.Record
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if decoded.Root.Slot != "root-a" || len(decoded.Sysexts) != 1 || decoded.Sysexts[0].Name != "kubernetes" {
		t.Fatalf("metadata did not preserve root/sysext selection: %#v", decoded)
	}
	if len(decoded.Confexts) != 1 || decoded.Confexts[0].Path != "/var/lib/katl/generations/2026.06.04-001/confext" {
		t.Fatalf("confext metadata = %#v", decoded.Confexts)
	}
	if decoded.Confexts[0].ActivationPath != "/run/confexts/katl-node" || decoded.Confexts[0].SHA256 != digest {
		t.Fatalf("confext activation/digest = %#v, digest %s", decoded.Confexts[0], digest)
	}
	if decoded.Confexts[0].Compatibility.ID != "katl" || decoded.Confexts[0].Compatibility.ConfextLevel != 1 {
		t.Fatalf("confext compatibility = %#v", decoded.Confexts[0].Compatibility)
	}
	if decoded.Confexts[0].Name != "katl-node" || decoded.Confexts[0].Compatibility.VersionID != "0.1.0" {
		t.Fatalf("stale confext metadata was reused: %#v", decoded.Confexts[0])
	}
}

func TestRunnerRejectsMissingGenerationRecord(t *testing.T) {
	install := &Context{
		ManifestPath:   writeManifest(t),
		StateDir:       t.TempDir(),
		TargetRoot:     t.TempDir(),
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		Commands:       &NoopCommandRunner{},
		Store:          &MemoryStateStore{},
	}

	err := NewRunner(PreseededManifestPlan(), install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "loader generation record is required") {
		t.Fatalf("Run() error = %v, want generation record failure", err)
	}
}

func TestRunnerRejectsConfigDomainsWithoutGenerationRecord(t *testing.T) {
	install := &Context{
		ManifestPath: writeManifestWithNode(t, `,
			"networkd": {
				"files": [
					{"name": "10-lan.network", "content": "[Match]\nName=enp1s0\n"}
				]
			}`),
		StateDir:       t.TempDir(),
		TargetRoot:     t.TempDir(),
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		Commands:       &NoopCommandRunner{},
		Store:          &MemoryStateStore{},
	}

	err := NewRunner(PreseededManifestPlan(), install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "loader generation record is required") {
		t.Fatalf("Run() error = %v, want generation record failure", err)
	}
}

const sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKatlExampleRuntimeKeyReplaceMe katl@example"

func writeManifest(t *testing.T) string {
	t.Helper()
	return writeManifestWithNode(t, "")
}

func writeManifestWithNode(t *testing.T, nodeExtra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "install.json")
	data := `{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"node": {
			"identity": {
				"hostname": "lab-node-01",
				"ssh": {
					"authorizedKeys": [
						"` + sshKey + `"
					]
				}
			}` + nodeExtra + `
		},
		"install": {
			"allowDestructiveInstall": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}
		},
		"artifacts": {
			"runtimeRoot": {
				"url": "https://example.invalid/root.squashfs",
				"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			"uki": {
				"url": "https://example.invalid/katl.efi",
				"sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			},
			"sysexts": [
				{
					"name": "kubelet",
					"url": "https://example.invalid/kubelet.sysext.raw",
					"sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
				}
			]
		}
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func minimalRecord(id string) *generation.Record {
	return &generation.Record{
		GenerationID:   id,
		RuntimeVersion: "0.1.0",
		Root: generation.RootSelection{
			Slot:          "root-a",
			PartitionUUID: "11111111-2222-3333-4444-555555555555",
		},
		Boot: generation.BootSelection{
			UKIPath: "/efi/EFI/Linux/katl-" + strings.TrimSpace(id) + ".efi",
		},
	}
}

func kubeadmPlans() map[string]kubeadmconfig.Plan {
	return map[string]kubeadmconfig.Plan{
		"control-plane": {
			Name: "control-plane",
			Config: kubeadmconfig.File{
				RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml",
				Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"),
				Mode:       0o644,
			},
		},
	}
}

func assertText(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func assertContains(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s missing %q:\n%s", path, want, data)
	}
}
