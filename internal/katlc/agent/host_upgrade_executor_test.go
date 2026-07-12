package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/operation"
)

func TestExecutorStagesHostUpgradeAndArmsTrial(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"etc", "efi/EFI/Linux", "dev/disk/by-partlabel"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/machine-id"), []byte("0123456789abcdef0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	previous := generation.GenerationSpec{
		APIVersion:     generation.APIVersion,
		Kind:           generation.SpecKind,
		GenerationID:   "gen0",
		RuntimeVersion: "2026.7.0-dev.0",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "aaaaaaaa-1111-2222-3333-444444444444",
			RuntimeVersion:        "2026.7.0-dev.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: strings.Repeat("a", 64),
		},
		Boot: generation.BootSelection{
			UKIPath:         "/efi/EFI/Linux/katl_gen0.efi",
			LoaderEntryPath: "loader/entries/katl-gen0.conf",
		},
		CreatedAt: now.Add(-time.Hour),
	}
	previousStatus, err := generation.NewGenerationStatus(previous, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, previous.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, previous, previousStatus); err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind,
		DefaultGenerationID: "gen0", BootedGenerationID: "gen0", Generation0FallbackID: "gen0",
		DefaultBootEntry: "loader/entries/katl-gen0.conf", BootedBootEntry: "loader/entries/katl-gen0.conf",
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	runtimeBytes := []byte("next runtime root")
	ukiBytes := []byte("next runtime uki")
	payloadRoot := filepath.Join(root, "payload")
	if err := os.MkdirAll(filepath.Join(payloadRoot, "components/runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(payloadRoot, "components/boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payloadRoot, "components/runtime/root.squashfs"), runtimeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payloadRoot, "components/boot/katl.efi"), ukiBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	payload := katlosimage.Payload{
		Root:           payloadRoot,
		ImageSHA256:    strings.Repeat("e", 64),
		ImageSizeBytes: 4096,
		Index:          katlosimage.Index{ImageRole: katlosimage.RoleUpgrade, Version: "2026.7.0-dev.1", Architecture: "x86_64", RuntimeInterface: "katl-runtime-1"},
		Runtime:        katlosimage.Component{Name: "runtime-root", Role: katlosimage.ComponentRuntimeRoot, Path: "components/runtime/root.squashfs", SizeBytes: int64(len(runtimeBytes)), SHA256: testSHA(runtimeBytes), Version: "2026.7.0-dev.1", Architecture: "x86_64"},
		Boot:           katlosimage.Component{Name: "runtime-uki", Role: katlosimage.ComponentRuntimeUKI, Path: "components/boot/katl.efi", SizeBytes: int64(len(ukiBytes)), SHA256: testSHA(ukiBytes), Version: "2026.7.0-dev.1", Architecture: "x86_64"},
	}
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(operation.OperationRecord{
		OperationID: "host-upgrade-1", OperationKind: OperationKindHostUpgrade, Scope: "host-generation", RequestDigest: strings.Repeat("d", 64),
		Phase: "accepted", CandidateGenerationID: "gen1", ActivationMode: operation.ActivationModeNextBoot,
		HostUpgradeRequest: &operation.HostUpgrade{ImageLocalRef: "upgrade.squashfs", ImageSHA256: strings.Repeat("e", 64), CandidateGenerationID: "gen1"},
		ResourceLocks:      []string{"generation-state.lock", "sysupdate.lock"},
	}, "accepted", now)
	if err != nil {
		t.Fatal(err)
	}
	oneshoot := ""
	activeDevice := filepath.Join(root, "dev/vda2")
	inactiveDevice := filepath.Join(root, "dev/vda3")
	if err := os.WriteFile(activeDevice, []byte("current root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inactiveDevice, make([]byte, len(runtimeBytes)+32), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(root, store, "agent-test")
	executor.Async = false
	executor.Now = func() time.Time { return now.Add(time.Minute) }
	executor.ResolveHostUpgrade = func(context.Context, operation.HostUpgrade) (katlosimage.Payload, error) { return payload, nil }
	executor.MountBootRoot = func(_ context.Context, path string) error {
		if path != filepath.Join(root, "efi") {
			t.Fatalf("MountBootRoot path = %q, want %q", path, filepath.Join(root, "efi"))
		}
		return nil
	}
	executor.SetBootOneshot = func(_ context.Context, _ string, entry string) error { oneshoot = entry; return nil }
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch filepath.Base(argv[0]) {
		case "blkid":
			joined := strings.Join(argv, " ")
			switch {
			case strings.Contains(joined, "PARTLABEL=KATL_ROOT_A"):
				return ToolResult{Stdout: []byte(activeDevice + "\n")}
			case strings.Contains(joined, "PARTLABEL=KATL_ROOT_B"):
				return ToolResult{Stdout: []byte(inactiveDevice + "\n")}
			default:
				return ToolResult{Stdout: []byte("bbbbbbbb-1111-2222-3333-444444444444\n")}
			}
		case "lsblk":
			if argv[len(argv)-1] == activeDevice {
				return ToolResult{Stdout: []byte("vda 2\n")}
			}
			return ToolResult{Stdout: []byte("vda 3\n")}
		case "sfdisk", "partx":
			return ToolResult{}
		case "systemd-sysupdate":
			if err := os.WriteFile(inactiveDevice, append(append([]byte(nil), runtimeBytes...), make([]byte, 32)...), 0o600); err != nil {
				return ToolResult{Err: err, ExitStatus: -1}
			}
			if err := os.WriteFile(filepath.Join(root, "efi/EFI/Linux/katl_2026.7.0-dev.1.efi"), ukiBytes, 0o600); err != nil {
				return ToolResult{Err: err, ExitStatus: -1}
			}
			return ToolResult{}
		default:
			return ToolResult{Err: os.ErrNotExist, ExitStatus: -1}
		}
	}
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if oneshoot != "loader/entries/katl-gen1.conf" {
		t.Fatalf("oneshot entry = %q", oneshoot)
	}
	gotRoot, err := os.ReadFile(inactiveDevice)
	if err != nil || !bytes.Equal(gotRoot[:len(runtimeBytes)], runtimeBytes) {
		t.Fatalf("staged root = %q, err = %v", gotRoot, err)
	}
	spec, status, err := generation.ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Root.Slot != "root-b" || spec.Root.PartitionUUID != "bbbbbbbb-1111-2222-3333-444444444444" || status.BootState != generation.BootStateTrying {
		t.Fatalf("candidate generation = spec %+v status %+v", spec, status)
	}
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.TrialGenerationID != "gen1" || selection.DefaultGenerationID != "gen0" || !selection.PendingHealthValidation {
		t.Fatalf("trial selection = %+v", selection)
	}
	completed, err := store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Terminal || completed.Result != operation.ResultSucceeded || !completed.BootHealthPending {
		t.Fatalf("completed operation = %+v", completed)
	}
}

func testSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
