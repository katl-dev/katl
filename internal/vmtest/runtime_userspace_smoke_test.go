package vmtest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/generation"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestRuntimeUserspaceSmoke(t *testing.T) {
	fixture := runtimeUserspaceFixture(t)
	work := t.TempDir()
	if err := copyOptionalDir(fixture.Root, work); err != nil {
		t.Fatalf("copy runtime fixture: %v", err)
	}

	metadataPath, err := generation.MetadataPath(work, fixture.GenerationID)
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	record, err := generation.ReadRecord(metadataPath)
	if err != nil {
		t.Fatalf("ReadRecord() error = %v", err)
	}
	if record.GenerationID != fixture.GenerationID {
		t.Fatalf("generationID = %q, want %q", record.GenerationID, fixture.GenerationID)
	}
	if len(record.Sysexts) != 1 || record.Sysexts[0].PayloadVersion != "v1.36.0" {
		t.Fatalf("sysext refs = %#v", record.Sysexts)
	}
	if len(record.Sysexts[0].Compatibility.RuntimeInterfaces) != 1 || record.Sysexts[0].Compatibility.RuntimeInterfaces[0] != "katl-runtime-1" {
		t.Fatalf("runtime interfaces = %#v", record.Sysexts[0].Compatibility.RuntimeInterfaces)
	}

	plan, err := generation.ApplyActivation(work, record)
	if err != nil {
		t.Fatalf("ApplyActivation() error = %v", err)
	}
	if len(plan.Sysexts) != 1 || len(plan.Confexts) != 1 {
		t.Fatalf("activation plan = %#v", plan)
	}
	assertSymlinkTarget(t, filepath.Join(work, "run/extensions/kubernetes"), "/var/lib/katl/generations/"+fixture.GenerationID+"/sysext/kubernetes.raw")
	assertSymlinkTarget(t, filepath.Join(work, "run/confexts/katl-node"), "/var/lib/katl/generations/"+fixture.GenerationID+"/confext")

	status := installstatus.New(installstatus.StateKubeadmReady, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC))
	status.InputMode = installstatus.InputModeTest
	status.InputSource = "runtime-userspace-smoke"
	status.RequestDigest = strings.Repeat("a", 64)
	status.KatlosImage = installstatus.Image{
		SHA256:           strings.Repeat("b", 64),
		Version:          "0.1.0",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	}
	status.TargetDiskStableID = "disk/by-id/test"
	status.SelectedRootSlot = "root-a"
	status.InstalledGeneration = fixture.GenerationID
	if err := installstatus.WriteRuntimeHandoff(work, status); err != nil {
		t.Fatalf("WriteRuntimeHandoff() error = %v", err)
	}
	updated, err := installstatus.ReadFile(filepath.Join(work, "var/lib/katl/install/status.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if updated.State != installstatus.StateWaitingForClusterBootstrap || updated.FinalHandoff != installstatus.StateWaitingForClusterBootstrap {
		t.Fatalf("handoff status = %#v", updated)
	}

	networkPath := filepath.Join(work, "var/lib/katl/generations", fixture.GenerationID, "confext/etc/systemd/network/10-smoke.network")
	data, err := os.ReadFile(networkPath)
	if err != nil {
		t.Fatalf("read generated network file: %v", err)
	}
	if !strings.Contains(string(data), "DHCP=yes") {
		t.Fatalf("network file = %q, want DHCP", string(data))
	}
	releasePath := filepath.Join(work, "var/lib/katl/generations", fixture.GenerationID, "confext/etc/extension-release.d/extension-release.katl-node")
	release, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatalf("read confext release: %v", err)
	}
	if !strings.Contains(string(release), "CONFEXT_LEVEL=1") {
		t.Fatalf("confext release = %q, want CONFEXT_LEVEL=1", string(release))
	}
}

type runtimeFixture struct {
	Root         string
	GenerationID string
}

func runtimeUserspaceFixture(t *testing.T) runtimeFixture {
	t.Helper()
	root := t.TempDir()
	generationID := "2026.06.06-001"
	generationsRoot := filepath.Join(root, "var/lib/katl/generations")
	tree, err := confext.RenderGenerationTree(confext.GenerationTreeRequest{
		GenerationsRoot: generationsRoot,
		GenerationID:    generationID,
		Files: []confext.NativeEtcFile{{
			Path:    "/etc/systemd/network/10-smoke.network",
			Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
		}},
		Extension: confext.ExtensionRelease{
			Name:         "katl-node",
			ID:           "katlos",
			VersionID:    "0.1.0",
			ConfextLevel: 1,
		},
		Chown: func(string, int, int) error { return nil },
	})
	if err != nil {
		t.Fatalf("RenderGenerationTree() error = %v", err)
	}
	confextSHA, err := generation.DigestDirectory(tree.ConfextDir)
	if err != nil {
		t.Fatalf("DigestDirectory() error = %v", err)
	}
	sysextPath := filepath.Join(generationsRoot, generationID, "sysext", "kubernetes.raw")
	sysextContent := []byte("katl test Kubernetes sysext placeholder\n")
	if err := os.MkdirAll(filepath.Dir(sysextPath), 0o755); err != nil {
		t.Fatalf("create sysext dir: %v", err)
	}
	if err := os.WriteFile(sysextPath, sysextContent, 0o644); err != nil {
		t.Fatalf("write sysext fixture: %v", err)
	}
	sysextSHA := sha256.Sum256(sysextContent)
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		GenerationID:          generationID,
		RuntimeVersion:        "0.1.0",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/EFI/Linux/katl.efi",
		Sysexts: []generation.ExtensionRef{{
			Name:            "kubernetes",
			Path:            "/var/lib/katl/generations/" + generationID + "/sysext/kubernetes.raw",
			ActivationPath:  "/run/extensions/kubernetes",
			SHA256:          hex.EncodeToString(sysextSHA[:]),
			ArtifactVersion: "0.1.0",
			PayloadVersion:  "v1.36.0",
			Architecture:    "x86_64",
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: []string{"katl-runtime-1"},
			},
		}},
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/" + generationID + "/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         confextSHA,
			Compatibility: generation.ConfextCompatibility{
				ID:           "katlos",
				VersionID:    "0.1.0",
				ConfextLevel: 1,
			},
		},
		KernelCommandLine: []string{"katl.generation=" + generationID},
		CreatedAt:         time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewFirstInstallRecord() error = %v", err)
	}
	metadataPath, err := generation.MetadataPath(root, generationID)
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	if err := generation.WriteRecord(metadataPath, record); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}
	return runtimeFixture{Root: root, GenerationID: generationID}
}

func assertSymlinkTarget(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Readlink(%s) error = %v", path, err)
	}
	if got != want {
		t.Fatalf("Readlink(%s) = %q, want %q", path, got, want)
	}
}
