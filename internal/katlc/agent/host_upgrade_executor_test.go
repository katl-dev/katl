package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestCleanupManagedHostUpgradeArtifactRemovesOnlyUploadedImage(t *testing.T) {
	root := t.TempDir()
	digest := strings.Repeat("a", 64)
	managedRef := hostUpgradeUploadDirectory + "/" + digest + ".squashfs"
	managedPath := filepath.Join(root, "var/lib/katl/artifacts", filepath.FromSlash(managedRef))
	unmanagedPath := filepath.Join(root, "var/lib/katl/artifacts/operator-image.squashfs")
	for _, path := range []string{managedPath, unmanagedPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	executor := &Executor{Root: root}
	executor.cleanupManagedHostUpgradeArtifact(operation.HostUpgrade{ImageLocalRef: managedRef})
	executor.cleanupManagedHostUpgradeArtifact(operation.HostUpgrade{ImageLocalRef: "operator-image.squashfs"})
	if _, err := os.Stat(managedPath); !os.IsNotExist(err) {
		t.Fatalf("managed artifact stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(unmanagedPath); err != nil {
		t.Fatalf("unmanaged artifact was removed: %v", err)
	}
}

func TestExecutorClassifiesHostUpgradeFailureByMutationBoundary(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutated    bool
		wantResult string
		wantRepair bool
	}{
		{name: "pre-mutation", wantResult: "failed"},
		{name: "post-mutation", mutated: true, wantResult: operation.ResultFailedNeedsRepair, wantRepair: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 24, 6, 0, 0, 0, time.UTC)
			record, err := store.Create(operation.OperationRecord{
				OperationID:             "host-upgrade-failure",
				OperationKind:           OperationKindHostUpgrade,
				Scope:                   "host-generation",
				RequestDigest:           strings.Repeat("d", 64),
				Phase:                   "accepted",
				CandidateGenerationID:   "gen1",
				ActivationMode:          operation.ActivationModeNextBoot,
				ActivationState:         operation.ActivationStatePending,
				GenerationCommitState:   operation.GenerationCommitCandidate,
				ExternalMutationStarted: test.mutated,
				HostUpgradeRequest: &operation.HostUpgrade{
					ImageURL:              "https://updates.example.test/katlos-upgrade.squashfs",
					CandidateGenerationID: "gen1",
				},
			}, "accepted", now)
			if err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(root, store, "agent-test")
			executor.Now = func() time.Time { return now.Add(time.Minute) }
			cause := errors.New("source generation prerequisite failed")
			if err := executor.failHostUpgrade(record, "verify-katlos-image", cause); !errors.Is(err, cause) {
				t.Fatalf("failHostUpgrade() error = %v, want cause", err)
			}
			failed, err := store.Read(record.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if !failed.Terminal || failed.Result != test.wantResult || failed.RecoveryRequired != test.wantRepair {
				t.Fatalf("failed record = %+v, want result %q repair=%t", failed, test.wantResult, test.wantRepair)
			}
			if test.mutated {
				if failed.GenerationCommitState != operation.GenerationCommitCandidate || failed.ActivationState != operation.ActivationStateFailed {
					t.Fatalf("post-mutation lifecycle = %+v", failed)
				}
			} else if failed.GenerationCommitState != operation.GenerationCommitAbandoned || !strings.Contains(failed.NextAction, "pre-mutation") {
				t.Fatalf("pre-mutation lifecycle = %+v", failed)
			}
		})
	}
}

func TestExecutorStagesHostUpgradeAndArmsTrial(t *testing.T) {
	exerciseHostUpgrade(t, OperationKindHostUpgrade, true)
}

func TestExecutorPreservesFallbackWhenBootHealthBecomesUnknown(t *testing.T) {
	for _, kind := range []string{OperationKindHostUpgrade, operationKindHostUpgradeHandoff} {
		t.Run(kind, func(t *testing.T) {
			exerciseHostUpgrade(t, kind, false)
		})
	}
}

func exerciseHostUpgrade(t *testing.T, kind string, staysHealthy bool) {
	t.Helper()
	const nextVersion = "2026.7.0-local.version-longer-than-a-gpt-partition-label"
	const commandLine = "root=PARTUUID=aaaaaaaa-1111-2222-3333-444444444444 rootfstype=squashfs ro katl.generation=gen0 katl.root-slot=root-a"
	root := t.TempDir()
	for _, dir := range []string{"etc", "efi/EFI/Linux", "efi/EFI/katl", "dev/disk/by-partlabel"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/machine-id"), []byte("0123456789abcdef0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/cmdline"), []byte(commandLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	endpointAdvertiserBytes := []byte("endpoint advertiser sysext")
	previous := generation.GenerationSpec{
		APIVersion:     generation.APIVersion,
		Kind:           generation.SpecKind,
		GenerationID:   "gen0",
		RuntimeVersion: "2026.7.0-local.previous-version-longer-than-a-gpt-partition-label",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "aaaaaaaa-1111-2222-3333-444444444444",
			RuntimeVersion:        "2026.7.0-local.previous-version-longer-than-a-gpt-partition-label",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: testSHA(testBootRootImage()),
		},
		Boot: generation.BootSelection{
			UKIPath:         "/efi/EFI/Linux/katl_gen0.efi",
			LoaderEntryPath: "loader/entries/katl-gen0.conf",
		},
		Sysexts: []generation.ExtensionRef{{
			Name:            katlosimage.EndpointAdvertiserName,
			Path:            "/var/lib/katl/generations/gen0/sysext/katl-endpoint-advertiser.raw",
			ActivationPath:  "/run/extensions/katl-endpoint-advertiser.raw",
			SHA256:          testSHA(endpointAdvertiserBytes),
			ArtifactVersion: "2026.7.0-dev.0",
			PayloadVersion:  "2026.7.0-dev.0",
			Architecture:    "x86_64",
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: []string{"katl-runtime-1"},
			},
		}},
		CreatedAt: now.Add(-time.Hour),
	}
	previousStatus, err := generation.NewGenerationStatus(previous, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, previous.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, previous, previousStatus); err != nil {
		t.Fatal(err)
	}
	previousManifest := bootstrapRuntimeManifest("control-plane")
	previousManifest.Node.Identity.SSH.AuthorizedKeys = []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"}
	previousManifest.Install.TargetDisk.ByID = "/dev/disk/by-id/test-root"
	previousManifest.Install.WipeTarget = true
	previousManifest.KatlosImage.URL = "https://example.test/katlos-install.squashfs"
	previousManifest.KatlosImage.SizeBytes = 1
	if err := configapply.WriteGenerationManifest(root, previous.GenerationID, previousManifest); err != nil {
		t.Fatal(err)
	}
	if err := configapply.WriteGenerationKubeadmConfig(root, previous.GenerationID, testNodeInput(t, bootstrapRuntimeKubeadmConfigs("control-plane")["default"])); err != nil {
		t.Fatal(err)
	}
	endpointAdvertiserPath := filepath.Join(root, "var/lib/katl/generations/gen0/sysext/katl-endpoint-advertiser.raw")
	if err := os.MkdirAll(filepath.Dir(endpointAdvertiserPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(endpointAdvertiserPath, endpointAdvertiserBytes, 0o644); err != nil {
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
	nextEndpointAdvertiserBytes := []byte("next endpoint advertiser sysext")
	payloadRoot := filepath.Join(root, "payload")
	if err := os.MkdirAll(filepath.Join(payloadRoot, "components/runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(payloadRoot, "components/boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(payloadRoot, "components/sysext"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payloadRoot, "components/runtime/root.squashfs"), runtimeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payloadRoot, "components/boot/katl.efi"), ukiBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payloadRoot, "components/sysext/endpoint-advertiser.raw"), nextEndpointAdvertiserBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	payload := katlosimage.Payload{
		Root:           payloadRoot,
		ImagePath:      filepath.Join(payloadRoot, "upgrade.squashfs"),
		ImageSHA256:    strings.Repeat("e", 64),
		ImageSizeBytes: 4096,
		Index:          katlosimage.Index{ImageRole: katlosimage.RoleUpgrade, Version: nextVersion, Architecture: "x86_64", RuntimeInterface: "katl-runtime-1"},
		Runtime:        katlosimage.Component{Name: "runtime-root", Role: katlosimage.ComponentRuntimeRoot, Path: "components/runtime/root.squashfs", SizeBytes: int64(len(runtimeBytes)), SHA256: testSHA(runtimeBytes), Version: "2026.7.0-dev.1", Architecture: "x86_64"},
		Boot:           katlosimage.Component{Name: "runtime-uki", Role: katlosimage.ComponentRuntimeUKI, Path: "components/boot/katl.efi", SizeBytes: int64(len(ukiBytes)), SHA256: testSHA(ukiBytes), Version: "2026.7.0-dev.1", Architecture: "x86_64"},
		EndpointAdvertiser: katlosimage.Component{
			Name: "endpoint-advertiser", Role: katlosimage.ComponentEndpointAdvertiser, Path: "components/sysext/endpoint-advertiser.raw",
			SizeBytes: int64(len(nextEndpointAdvertiserBytes)), SHA256: testSHA(nextEndpointAdvertiserBytes), Version: "2026.7.0-dev.1",
			PayloadVersion: "2026.7.0-dev.1", Architecture: "x86_64",
			Compatibility: katlosimage.Compatibility{RuntimeInterface: "katl-runtime-1"},
		},
	}
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(operation.OperationRecord{
		OperationID: "host-upgrade-1", OperationKind: kind, Scope: "host-generation", RequestDigest: strings.Repeat("d", 64),
		Phase: "accepted", CandidateGenerationID: "gen1", ActivationMode: operation.ActivationModeNextBoot,
		ExpectedCurrentGenerationID: previous.GenerationID,
		HostUpgradeRequest:          &operation.HostUpgrade{ImageLocalRef: "upgrade.squashfs", ImageSHA256: strings.Repeat("e", 64), CandidateGenerationID: "gen1"},
		ResourceLocks:               []string{"generation-state.lock", "sysupdate.lock"},
	}, "accepted", now)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(root, store)
	unit := &healthUnit{}
	unit.set(healthSucceeded)
	server.RunBootHealthStatus = unit.run
	if err := server.validateHostUpgradePlan(t.Context(), &agentapi.HostUpgradeOperationRequest{
		ImageLocalRef: "upgrade.squashfs", CandidateGenerationId: "gen1",
	}); err != nil {
		t.Fatalf("admit upgrade while healthy: %v", err)
	}
	oneshoot := ""
	host := newHostDisks(t, hostDisk{name: "vda", seq: 1, partitions: []hostPartition{
		{1, "KATL_ESP", "esp-partuuid", espType},
		{2, "KATL_ROOT_A", "aaaaaaaa-1111-2222-3333-444444444444", rootType},
		{3, "KATL_ROOT_B", "bbbbbbbb-1111-2222-3333-444444444444", rootType},
		{4, "KATL_STATE", "state-partuuid", stateType},
	}})
	host.mountRoot(t, root, 2)
	writeTestFile(t, filepath.Join(root, "dev/vda2"), string(testBootRootImage()))
	writeTestFile(t, filepath.Join(root, "efi", previous.Boot.LoaderEntryPath), "known-good loader entry")
	// udev has not yet created the inactive partition's by-diskseq link: only
	// the kernel node exists.
	inactiveDevice := filepath.Join(root, "dev/vda3")
	host.devRoot = root
	writeTestFile(t, inactiveDevice, string(make([]byte, len(runtimeBytes)+32)))
	executor := NewExecutor(root, store, "agent-test")
	executor.nodeBootHealth = server.nodeBootHealth
	executor.volumeDevices.bootedDisk = host.booted(0, 2)
	executor.Async = false
	executor.Now = func() time.Time { return now.Add(time.Minute) }
	executor.ResolveHostUpgrade = func(context.Context, operation.HostUpgrade) (katlosimage.Payload, error) {
		if !staysHealthy {
			unit.set("")
		}
		return payload, nil
	}
	executor.MountBootRoot = func(_ context.Context, path string) error {
		if path != filepath.Join(root, "efi") {
			t.Fatalf("MountBootRoot path = %q, want %q", path, filepath.Join(root, "efi"))
		}
		return nil
	}
	executor.SetBootOneshot = func(_ context.Context, _ string, entry string) error { oneshoot = entry; return nil }
	var toolCalls []string
	var stagedRoot []byte
	activeUKI := filepath.Join(root, "efi/EFI/Linux/katl_gen0.efi")
	if err := os.WriteFile(activeUKI, []byte("active rollback kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	inactiveUKI := filepath.Join(root, "efi/EFI/katl/katl-root-b-1.efi")
	if err := os.WriteFile(inactiveUKI, []byte("stale inactive kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		toolCalls = append(toolCalls, strings.Join(argv, " "))
		switch filepath.Base(argv[0]) {
		case "lsblk":
			return host.run(context.Background(), argv, nil)
		case "sfdisk":
			if len(argv[len(argv)-1]) > 36 {
				return ToolResult{Err: errors.New("GPT label exceeds 36 characters"), ExitStatus: 1}
			}
			return ToolResult{}
		case "systemd-sysupdate":
			if _, err := os.Stat(inactiveUKI); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("inactive UKI still looks installed to sysupdate: %v", err)
			}
			if argv[len(argv)-1] == "vacuum" {
				return ToolResult{}
			}
			stagedRoot = append(append([]byte(nil), runtimeBytes...), make([]byte, 32)...)
			if argv[len(argv)-1] != "1" {
				t.Fatalf("sysupdate selected release version instead of local candidate: %v", argv)
			}
			if err := os.WriteFile(filepath.Join(root, "efi/EFI/katl/katl-root-b-1.efi"), ukiBytes, 0o600); err != nil {
				return ToolResult{Err: err, ExitStatus: -1}
			}
			return ToolResult{}
		case "blockdev":
			if len(argv) != 3 || argv[1] != "--flushbufs" {
				return ToolResult{Err: errors.New("unexpected blockdev arguments"), ExitStatus: 1}
			}
			if _, err := os.Stat(argv[2]); err != nil {
				return ToolResult{Stderr: []byte("blockdev: cannot open " + argv[2] + ": No such file or directory"), ExitStatus: 1}
			}
			if argv[2] != inactiveDevice {
				return ToolResult{Err: errors.New("flushed " + argv[2] + ", not the inactive slot"), ExitStatus: 1}
			}
			if err := os.WriteFile(inactiveDevice, stagedRoot, 0o600); err != nil {
				return ToolResult{Err: err, ExitStatus: -1}
			}
			return ToolResult{}
		default:
			return ToolResult{Err: os.ErrNotExist, ExitStatus: -1}
		}
	}
	if kind == operationKindHostUpgradeHandoff {
		err = executor.executeHostUpgradeHandoff(t.Context(), record, executor.ResolveHostUpgrade,
			func(_ context.Context, _ katlosimage.Payload, handoff generation.UpgradeHandoff, _ string) (preparedUpgrade, error) {
				// The target planner's output lives in a private state view.
				prepared := preparedUpgrade{root: t.TempDir()}
				dir, err := generation.GenerationDir(prepared.root, handoff.CandidateGenerationID)
				if err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(dir, "spec.json"), "prepared generation")
				return prepared, nil
			})
	} else {
		err = executor.Execute(t.Context(), record)
	}
	if !staysHealthy {
		failed, readErr := store.Read(record.OperationID)
		if readErr != nil || !failed.Terminal || failed.Result != "failed" || failed.ExternalMutationStarted {
			t.Fatalf("refused upgrade record = %+v, read error = %v", failed, readErr)
		}
		for _, want := range []string{"unknown", "cannot read boot health for this boot from systemd", "katlctl node status", "katlctl cluster apply", "katlctl node reboot"} {
			if err == nil || !strings.Contains(failed.FailureReason, want) {
				t.Fatalf("upgrade error = %v, failure reason = %q, want %q", err, failed.FailureReason, want)
			}
		}
		if got, err := os.ReadFile(inactiveDevice); err != nil || !bytes.Equal(got, make([]byte, len(runtimeBytes)+32)) {
			t.Fatalf("inactive root changed before mutation: %q, %v", got, err)
		}
		if got, err := os.ReadFile(inactiveUKI); err != nil || string(got) != "stale inactive kernel" {
			t.Fatalf("inactive UKI changed before mutation: %q, %v", got, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := strings.Join(toolCalls, "\n"); strings.Contains(calls, "PARTLABEL=") {
		t.Fatalf("host upgrade discovered mutable partition labels:\n%s", calls)
	}
	if calls := strings.Join(toolCalls, "\n"); strings.Contains(calls, "--sync=no") {
		t.Fatalf("host upgrade disabled sysupdate durability:\n%s", calls)
	}
	vacuumCall, sysupdateCall, flushCall := -1, -1, -1
	for index, call := range toolCalls {
		switch {
		case strings.Contains(call, "systemd-sysupdate") && strings.HasSuffix(call, " vacuum"):
			vacuumCall = index
		case strings.Contains(call, "systemd-sysupdate"):
			sysupdateCall = index
		case strings.HasPrefix(call, "blockdev --flushbufs "):
			flushCall = index
		}
	}
	if vacuumCall < 0 || vacuumCall > sysupdateCall {
		t.Fatalf("host upgrade calls = %#v, want sysupdate vacuum before update", toolCalls)
	}
	if sysupdateCall < 0 || flushCall != sysupdateCall+1 {
		t.Fatalf("host upgrade calls = %#v, want block cache flush immediately after sysupdate", toolCalls)
	}
	if oneshoot != "loader/entries/katl-gen1.conf" {
		t.Fatalf("oneshot entry = %q", oneshoot)
	}
	if got, err := os.ReadFile(activeUKI); err != nil || string(got) != "active rollback kernel" {
		t.Fatalf("active rollback kernel changed: %q, %v", got, err)
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
	if spec.RuntimeVersion != nextVersion || spec.Root.RuntimeVersion != nextVersion {
		t.Fatalf("full release version was not preserved: %+v", spec)
	}
	for _, call := range toolCalls {
		if strings.Contains(call, "sfdisk --part-label") && (strings.Contains(call, nextVersion) || strings.Contains(call, previous.RuntimeVersion)) {
			t.Fatalf("release version leaked into GPT label: %s", call)
		}
	}
	if len(spec.Sysexts) != 1 || spec.Sysexts[0].Name != katlosimage.EndpointAdvertiserName || spec.Sysexts[0].Path != "/var/lib/katl/generations/gen1/sysext/endpoint-advertiser.raw" || spec.Sysexts[0].SHA256 != testSHA(nextEndpointAdvertiserBytes) {
		t.Fatalf("candidate sysexts = %+v, want bundled endpoint advertiser replacement", spec.Sysexts)
	}
	candidateManifest, err := configapply.ReadGenerationManifest(root, "gen1")
	if err != nil {
		t.Fatal(err)
	}
	if candidateManifest.Node.Identity.Hostname != previousManifest.Node.Identity.Hostname ||
		candidateManifest.Node.SystemRole != previousManifest.Node.SystemRole {
		t.Fatalf("candidate manifest node = %+v, want current configuration %+v", candidateManifest.Node, previousManifest.Node)
	}
	stagedEndpointAdvertiser, err := os.ReadFile(filepath.Join(root, strings.TrimPrefix(spec.Sysexts[0].Path, "/")))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stagedEndpointAdvertiser, nextEndpointAdvertiserBytes) {
		t.Fatalf("staged endpoint advertiser = %q", stagedEndpointAdvertiser)
	}
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.TargetBootGenerationID != "gen1" || selection.TrialGenerationID != "gen1" || selection.DefaultGenerationID != "gen0" || !selection.PendingHealthValidation {
		t.Fatalf("trial selection = %+v", selection)
	}
	completed, err := store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Terminal || completed.Result != operation.ResultSucceeded || !completed.BootHealthPending {
		t.Fatalf("completed operation = %+v", completed)
	}

	t.Run("running boot fails while trial is queued", func(t *testing.T) {
		health, err := generation.RecordBootHealth(generation.BootHealthRequest{
			Root: root, CommandLine: commandLine, Result: generation.BootHealthFailure, RequestReboot: true,
		})
		if err != nil || !health.KeptKnownGood || health.DefaultGeneration != "gen0" || health.NextBootGeneration != "gen1" || health.RebootRequested {
			t.Fatalf("boot health = %#v, %v, want kept gen0 with gen1 queued", health, err)
		}
		after, err := generation.ReadBootSelection(root)
		if err != nil || after != selection {
			t.Fatalf("boot selection after recovery = %#v, %v, want original trial %#v", after, err, selection)
		}
	})
}

func testSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
