package vmtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
	"github.com/katl-dev/katl/internal/katlc/agent"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestInstalledRuntimeSysupdateRootUKITransfer(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installed runtime sysupdate root+UKI smoke")
	}
	runner := NewRunner(options)
	runtime := InstalledRuntimeConfig{}
	var plannedMAC string
	var worldScenario *WorldScenario
	spec := NodeSpec{Name: "sysupdate-partx-1", Role: ControlPlane}
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-sysupdate-root-uki", spec); ok {
		runner = worldRun.Runner
		runtime = worldRun.Config
		plannedMAC = worldRun.Node.MACAddress
		worldScenario = worldRun.Scenario
	} else {
		_ = RequireWorld(t)
	}
	scenario := Scenario{Name: "installed-runtime-sysupdate-root-uki"}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result = requirePlannedVMHost(t, runner, scenario, result, HostRequirements{
		Libvirt: true,
		OVMF:    true,
		KVM:     runner.options().KVM,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	vm := runtime.VM
	vm.KVM = runner.options().KVM
	vm.RAMMiB = 2048
	vm.CPUs = 2
	vm.Timeout = 8 * time.Minute
	vm.Network.MAC = first(vm.Network.MAC, plannedMAC)
	vm.VSock.Enabled = true
	vm.Agent.RequireHealth = true
	vm.Agent.Timeout = 30 * time.Second
	vm.PreserveNVRAM = true
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
		Name: spec.Name,
		Runtime: InstalledRuntimeConfig{
			Disk:            runtime.Disk,
			DiskFormat:      runtime.DiskFormat,
			ESPArtifacts:    runtime.ESPArtifacts,
			FixtureManifest: runtime.FixtureManifest,
			NodeMetadata:    runtime.NodeMetadata,
			VM:              vm,
		},
	}, VMRunner{})
	if err != nil {
		t.Fatalf("StartInstalledRuntimeNode() error = %v", err)
	}
	defer func() {
		if err := node.Stop(); err != nil && err != context.Canceled {
			t.Logf("Stop() error = %v", err)
		}
	}()
	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatalf("DialAgent() error = %v", err)
	}
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()
	guest := NewGuestControl(node.Result, client)
	defer func() {
		if t.Failed() {
			collectSysupdateFailureEvidence(ctx, guest)
		}
	}()

	previousGeneration := currentGenerationFromGuest(t, ctx, guest)
	previousSpec, previousStatus := generationRecordsFromGuest(t, ctx, guest, previousGeneration)
	previousSpec.KernelCommandLine = append(previousSpec.KernelCommandLine,
		"console=ttyS0,115200n8", "systemd.log_target=console", "loglevel=6", "katl.vmtest_agent=1")
	digest, err := generation.CanonicalSpecDigest(previousSpec)
	if err != nil {
		t.Fatalf("digest vmtest-visible previous generation: %v", err)
	}
	previousStatus.SpecDigest = digest
	previousStatus.UpdatedAt = time.Now().UTC()
	writeGuestJSON(t, ctx, guest, "/var/lib/katl/generations/"+previousGeneration+"/spec.json", previousSpec)
	writeGuestJSON(t, ctx, guest, "/var/lib/katl/generations/"+previousGeneration+"/status.json", previousStatus)
	stateMarker := "/var/lib/katl/test-artifacts/host-upgrade-state-marker"
	writeGuestFile(t, ctx, guest, stateMarker, []byte("state-survives-host-upgrade-and-rollback\n"), 0o600)

	upgrade := discoverBuiltUpgradeImage(t, previousSpec.RuntimeVersion)
	localRef := filepath.ToSlash(filepath.Join("updates", filepath.Base(upgrade.Path)))
	uploadGuestFile(t, ctx, guest, upgrade.Path, "/var/lib/katl/artifacts/"+localRef, 4<<20)
	endpoint := katlcEndpoint(t, node, "")
	candidateGeneration := "host-upgrade-" + strings.ReplaceAll(upgrade.Version, ".", "-")
	conn, katlc := dialKatlcAgentForVMTest(t, ctx, endpoint)
	nodeStatus, err := katlc.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		conn.Close()
		t.Fatalf("read node status before host upgrade: %v", err)
	}
	accepted, err := katlc.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
		ApiVersion:                  operation.APIVersion,
		Kind:                        agent.RequestKind,
		ClientRequestId:             "vmtest-host-upgrade-" + candidateGeneration,
		OperationKind:               agent.OperationKindHostUpgrade,
		Actor:                       "installed runtime host upgrade vmtest",
		ExpectedMachineId:           nodeStatus.GetMachineId(),
		ExpectedCurrentGenerationId: previousGeneration,
		HostUpgrade: &agentapi.HostUpgradeOperationRequest{
			ImageLocalRef:         localRef,
			CandidateGenerationId: candidateGeneration,
		},
	})
	conn.Close()
	if err != nil {
		t.Fatalf("submit host upgrade operation: %v", err)
	}
	status := waitKatlcOperationTerminal(t, ctx, endpoint, accepted.GetOperationId())
	if status.GetResult() != operation.ResultSucceeded || !status.GetBootHealthPending() || status.GetCandidateGenerationId() != candidateGeneration {
		t.Fatalf("host upgrade operation status = %+v", status)
	}
	recordData := readGuestFile(t, ctx, guest, "/var/lib/katl/operations/"+accepted.GetOperationId()+"/record.json")
	envelope, err := persistedrecord.DecodeEnvelope([]byte(recordData))
	if err != nil {
		t.Fatalf("decode host upgrade operation envelope: %v", err)
	}
	persisted, err := persistedrecord.DecodePayload[operation.Snapshot](envelope)
	if err != nil {
		t.Fatalf("decode host upgrade operation snapshot: %v", err)
	}
	request := persisted.Record.HostUpgradeRequest
	if request == nil || request.ImageSHA256 != upgrade.SHA256 || request.ImageSizeBytes != upgrade.SizeBytes {
		t.Fatalf("host upgrade did not persist derived image identity: %#v", request)
	}
	trial := bootSelectionFromGuest(t, ctx, guest)
	if trial.TrialGenerationID != candidateGeneration || trial.PreviousKnownGoodGenerationID != previousGeneration || !trial.PendingHealthValidation {
		t.Fatalf("host upgrade trial selection = %#v", trial)
	}
	candidateSpec := generationFromGuest(t, ctx, guest, candidateGeneration)
	if candidateSpec.RuntimeVersion != upgrade.Version || candidateSpec.Root.Slot == previousSpec.Root.Slot {
		t.Fatalf("host upgrade candidate spec = %#v; previous = %#v", candidateSpec, previousSpec)
	}

	guest, client = restartGuestAndReconnect(t, ctx, &node, guest, client)
	waitGenerationPromotion(t, ctx, guest, candidateGeneration)
	assertBootedGenerationIdentity(t, ctx, guest, candidateSpec)
	assertGuestFileContains(t, ctx, guest, stateMarker, "state-survives-host-upgrade-and-rollback")
	promoted := bootSelectionFromGuest(t, ctx, guest)
	if promoted.DefaultGenerationID != candidateGeneration || promoted.PreviousKnownGoodGenerationID != previousGeneration || promoted.PendingHealthValidation {
		t.Fatalf("promoted host upgrade selection = %#v", promoted)
	}

	rollback := promoted
	rollback.TrialGenerationID = previousGeneration
	rollback.PreviousKnownGoodGenerationID = candidateGeneration
	rollback.TrialBootEntry = previousSpec.Boot.LoaderEntryPath
	rollback.PreviousKnownGoodBootEntry = candidateSpec.Boot.LoaderEntryPath
	rollback.PendingTransactionID = "vmtest-host-rollback-" + previousGeneration
	rollback.PendingHealthValidation = true
	rollback.PersistentDefaultPromotion = generation.DefaultPromotionPending
	rollback.UpdatedAt = time.Now().UTC()
	writeGuestJSON(t, ctx, guest, "/var/lib/katl/boot/selection.json", rollback)
	guestCommand(t, ctx, guest, "select-previous-known-good", "bootctl", "set-oneshot", filepath.Base(previousSpec.Boot.LoaderEntryPath))

	guest, client = restartGuestAndReconnect(t, ctx, &node, guest, client)
	waitGenerationPromotion(t, ctx, guest, previousGeneration)
	assertBootedGenerationIdentity(t, ctx, guest, previousSpec)
	assertGuestFileContains(t, ctx, guest, stateMarker, "state-survives-host-upgrade-and-rollback")
	rolledBack := bootSelectionFromGuest(t, ctx, guest)
	if rolledBack.DefaultGenerationID != previousGeneration || rolledBack.PreviousKnownGoodGenerationID != candidateGeneration || rolledBack.PendingHealthValidation {
		t.Fatalf("rolled-back boot selection = %#v", rolledBack)
	}
	if previousSpec.RuntimeVersion == candidateSpec.RuntimeVersion || previousSpec.Boot.UKIPath == candidateSpec.Boot.UKIPath || previousSpec.Root.Slot == candidateSpec.Root.Slot {
		t.Fatalf("upgrade did not produce distinct version, root slot, and UKI identities: previous=%#v candidate=%#v", previousSpec, candidateSpec)
	}
	guestCommand(t, ctx, guest, "boot-health-evidence", "systemctl", "show", "katl-boot-health.service", "--property=Result,ExecMainStatus")
	guestCommand(t, ctx, guest, "boot-complete-evidence", "systemctl", "is-active", "katl-boot-complete.target")
	t.Log("host upgrade and rollback are serialized per node in v0.1; multi-node rollout orchestration remains operator-controlled")
	powerOffGuestForCleanSuccess(t, ctx, &node, guest, client)
	client = nil

	node.Result.finish(StatusPassed, "", runner.time())
	if err := runner.Write(scenario, node.Result); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if worldScenario != nil {
		if err := worldScenario.WriteResult(WorldStatusPassed, ""); err != nil {
			t.Fatalf("write world scenario result: %v", err)
		}
	}
}

type builtUpgradeImage struct {
	Path      string
	Version   string
	SHA256    string
	SizeBytes uint64
}

func discoverBuiltUpgradeImage(t *testing.T, baseVersion string) builtUpgradeImage {
	t.Helper()
	runtimeData, err := os.ReadFile(filepath.Join(repoRoot(t), "_build", "mkosi", "katl-runtime-root.squashfs.json"))
	if err != nil {
		t.Fatalf("read current runtime artifact metadata: %v", err)
	}
	var runtimeMetadata struct {
		BuildID string `json:"generation"`
	}
	if err := json.Unmarshal(runtimeData, &runtimeMetadata); err != nil || runtimeMetadata.BuildID == "" {
		t.Fatalf("decode current runtime artifact identity: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(repoRoot(t), "_build", "mkosi", "katlos-upgrade-*-x86_64.squashfs.json"))
	if err != nil {
		t.Fatalf("discover KatlOS upgrade image metadata: %v", err)
	}
	var metadata struct {
		ImageRole string `json:"imageRole"`
		Version   string `json:"version"`
		BuildID   string `json:"buildID"`
		Path      string `json:"path"`
		SizeBytes uint64 `json:"sizeBytes"`
		SHA256    string `json:"sha256"`
	}
	metadataPath := ""
	for _, candidate := range matches {
		data, readErr := os.ReadFile(candidate)
		if readErr != nil {
			t.Fatalf("read KatlOS upgrade image metadata: %v", readErr)
		}
		var found = metadata
		if err := json.Unmarshal(data, &found); err != nil {
			t.Fatalf("decode KatlOS upgrade image metadata: %v", err)
		}
		if found.BuildID == runtimeMetadata.BuildID && found.Version != baseVersion {
			if metadataPath != "" {
				t.Fatalf("multiple upgrade images match current runtime build %s: %s and %s", runtimeMetadata.BuildID, metadataPath, candidate)
			}
			metadataPath, metadata = candidate, found
		}
	}
	if metadataPath == "" {
		t.Fatalf("no next-version KatlOS upgrade image matches current runtime build %s and base version %s; candidates: %v", runtimeMetadata.BuildID, baseVersion, matches)
	}
	if metadata.ImageRole != string(katlosimage.RoleUpgrade) || metadata.Version == "" || metadata.Path == "" || metadata.SizeBytes == 0 || len(metadata.SHA256) != 64 {
		t.Fatalf("incomplete KatlOS upgrade image metadata: %+v", metadata)
	}
	path := filepath.Join(filepath.Dir(metadataPath), metadata.Path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat KatlOS upgrade image: %v", err)
	}
	if uint64(info.Size()) != metadata.SizeBytes {
		t.Fatalf("KatlOS upgrade image size = %d, metadata = %d", info.Size(), metadata.SizeBytes)
	}
	digest, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("hash KatlOS upgrade image: %v", err)
	}
	if digest != metadata.SHA256 {
		t.Fatalf("KatlOS upgrade image SHA-256 = %s, metadata = %s", digest, metadata.SHA256)
	}
	return builtUpgradeImage{Path: path, Version: metadata.Version, SHA256: metadata.SHA256, SizeBytes: metadata.SizeBytes}
}

func uploadGuestFile(t *testing.T, ctx context.Context, guest *GuestControl, source, target string, chunkSize int) {
	t.Helper()
	file, err := os.Open(source)
	if err != nil {
		t.Fatalf("open guest upload source: %v", err)
	}
	defer file.Close()
	buffer := make([]byte, chunkSize)
	var offset uint64
	for {
		n, readErr := io.ReadFull(file, buffer)
		if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
			t.Fatalf("read guest upload source at %d: %v", offset, readErr)
		}
		if n > 0 {
			if _, err := guest.WriteFile(ctx, GuestFileRequest{
				Name:     fmt.Sprintf("host-upgrade-image-%08d", offset),
				Path:     target,
				Content:  buffer[:n],
				Mode:     0o600,
				Timeout:  30 * time.Second,
				Offset:   offset,
				Truncate: offset == 0,
			}); err != nil {
				t.Fatalf("upload guest file %s at %d: %v", target, offset, err)
			}
			offset += uint64(n)
		}
		if readErr == io.ErrUnexpectedEOF || readErr == io.EOF {
			break
		}
	}
}

func assertBootedGenerationIdentity(t *testing.T, ctx context.Context, guest *GuestControl, spec generation.GenerationSpec) {
	t.Helper()
	if got := currentGenerationFromGuest(t, ctx, guest); got != spec.GenerationID {
		t.Fatalf("booted generation = %q, want %q", got, spec.GenerationID)
	}
	cmdline := readGuestFile(t, ctx, guest, "/proc/cmdline")
	if !strings.Contains(cmdline, "katl.generation="+spec.GenerationID) || !strings.Contains(cmdline, "root=PARTUUID="+spec.Root.PartitionUUID) {
		t.Fatalf("booted kernel command line does not identify generation root: %s", cmdline)
	}
	for _, argument := range spec.KernelCommandLine {
		if !strings.Contains(cmdline, argument) {
			t.Fatalf("booted kernel command line is missing generation argument %q: %s", argument, cmdline)
		}
	}
	guestCommand(t, ctx, guest, "verify-automatic-efi-mount", "findmnt", "--mountpoint", "/efi")
	assertGuestExists(t, ctx, guest, spec.Boot.UKIPath)
}

func waitGenerationPromotion(t *testing.T, ctx context.Context, guest *GuestControl, generationID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastStatus generation.GenerationStatus
	var lastSelection generation.BootSelectionRecord
	for time.Now().Before(deadline) {
		_, lastStatus = generationRecordsFromGuest(t, ctx, guest, generationID)
		lastSelection = bootSelectionFromGuest(t, ctx, guest)
		if lastStatus.CommitState == generation.CommitStateCommitted &&
			lastStatus.BootState == generation.BootStateGood &&
			lastStatus.HealthState == generation.HealthStateHealthy &&
			lastSelection.DefaultGenerationID == generationID &&
			!lastSelection.PendingHealthValidation {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("generation %s was not promoted; status=%#v selection=%#v", generationID, lastStatus, lastSelection)
}

type sysupdateGuestFixture struct {
	RootPath         string
	UKIPath          string
	RootSourcePath   string
	UKISourcePath    string
	RootTransferPath string
	UKITransferPath  string
}

func writeSysupdateGuestFixtureFromImage(t *testing.T, ctx context.Context, guest *GuestControl, runDir, version string, payload katlosimage.Payload) sysupdateGuestFixture {
	t.Helper()
	rootBytes, err := os.ReadFile(payload.ComponentPath(payload.Runtime))
	if err != nil {
		t.Fatalf("read runtime-root image component: %v", err)
	}
	ukiBytes, err := os.ReadFile(payload.ComponentPath(payload.Boot))
	if err != nil {
		t.Fatalf("read runtime-uki image component: %v", err)
	}
	if got := sha256Hex(rootBytes); got != payload.Runtime.SHA256 {
		t.Fatalf("runtime-root source digest = %s, want image component %s", got, payload.Runtime.SHA256)
	}
	if got := sha256Hex(ukiBytes); got != payload.Boot.SHA256 {
		t.Fatalf("runtime-uki source digest = %s, want image component %s", got, payload.Boot.SHA256)
	}
	guestSource := "/var/lib/katl/test-artifacts/sysupdate/source"
	guestDefinitions := "/var/lib/katl/test-artifacts/sysupdate/sysupdate.d"
	hostSource := filepath.Join(runDir, "sysupdate-source")
	hostDefinitions := filepath.Join(runDir, "sysupdate.d")
	rootName := "katl_" + version + ".root.squashfs"
	ukiName := "katl_" + version + ".efi"
	rootPath := guestSource + "/" + rootName
	ukiPath := guestSource + "/" + ukiName
	hostRootPath := filepath.Join(hostSource, rootName)
	hostUKIPath := filepath.Join(hostSource, ukiName)
	if err := os.MkdirAll(hostSource, 0o755); err != nil {
		t.Fatalf("create host sysupdate source: %v", err)
	}
	if err := os.MkdirAll(hostDefinitions, 0o755); err != nil {
		t.Fatalf("create host sysupdate definitions: %v", err)
	}
	if err := os.WriteFile(hostRootPath, rootBytes, 0o644); err != nil {
		t.Fatalf("write host sysupdate root source: %v", err)
	}
	if err := os.WriteFile(hostUKIPath, ukiBytes, 0o644); err != nil {
		t.Fatalf("write host sysupdate UKI source: %v", err)
	}
	shaSums := fmt.Sprintf("%s  %s\n%s  %s\n", sha256Hex(rootBytes), rootName, sha256Hex(ukiBytes), ukiName)
	if err := os.WriteFile(filepath.Join(hostSource, "SHA256SUMS"), []byte(shaSums), 0o644); err != nil {
		t.Fatalf("write host sysupdate SHA256SUMS: %v", err)
	}
	hostRootTransfer := filepath.Join(hostDefinitions, "50-katl-root.transfer")
	hostUKITransfer := filepath.Join(hostDefinitions, "70-katl-uki.transfer")
	if err := os.WriteFile(hostRootTransfer, []byte(rootTransfer(guestSource)), 0o644); err != nil {
		t.Fatalf("write host root transfer: %v", err)
	}
	if err := os.WriteFile(hostUKITransfer, []byte(ukiTransfer(guestSource)), 0o644); err != nil {
		t.Fatalf("write host UKI transfer: %v", err)
	}
	writeGuestFile(t, ctx, guest, rootPath, rootBytes, 0o644)
	writeGuestFile(t, ctx, guest, ukiPath, ukiBytes, 0o644)
	writeGuestFile(t, ctx, guest, guestSource+"/SHA256SUMS", []byte(shaSums), 0o644)
	writeGuestFile(t, ctx, guest, guestDefinitions+"/50-katl-root.transfer", []byte(rootTransfer(guestSource)), 0o644)
	writeGuestFile(t, ctx, guest, guestDefinitions+"/70-katl-uki.transfer", []byte(ukiTransfer(guestSource)), 0o644)
	return sysupdateGuestFixture{
		RootPath:         rootPath,
		UKIPath:          ukiPath,
		RootSourcePath:   hostRootPath,
		UKISourcePath:    hostUKIPath,
		RootTransferPath: hostRootTransfer,
		UKITransferPath:  hostUKITransfer,
	}
}

func writeSysupdateUpgradeImagePayload(t *testing.T, runDir string, version string, rootBytes, ukiBytes []byte) (katlosimage.Payload, string) {
	t.Helper()
	imageRoot := filepath.Join(runDir, "katlos-upgrade-image")
	components := map[string][]byte{
		"components/runtime/root.squashfs": rootBytes,
		"components/boot/katl.efi":         ukiBytes,
	}
	digests := make(map[string]string, len(components))
	sizes := make(map[string]int64, len(components))
	for rel, data := range components {
		path := filepath.Join(imageRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create upgrade image component dir: %v", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write upgrade image component: %v", err)
		}
		digests[rel] = sha256Hex(data)
		sizes[rel] = int64(len(data))
	}
	index := katlosimage.Index{
		APIVersion:       katlosimage.APIVersion,
		Kind:             katlosimage.Kind,
		ImageRole:        katlosimage.RoleUpgrade,
		Format:           katlosimage.FormatSquashFS,
		Version:          version,
		BuildID:          "vmtest-sysupdate-" + version,
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		CreatedAt:        "2026-06-17T12:00:00Z",
		Components: []katlosimage.Component{
			{
				Name:         "runtime-root",
				Role:         katlosimage.ComponentRuntimeRoot,
				Path:         "components/runtime/root.squashfs",
				Format:       "squashfs",
				SizeBytes:    sizes["components/runtime/root.squashfs"],
				SHA256:       digests["components/runtime/root.squashfs"],
				Version:      version,
				Architecture: "x86_64",
				Compatibility: katlosimage.Compatibility{
					RuntimeInterface: "katl-runtime-1",
				},
				InstallTarget: katlosimage.InstallTarget{
					Kind:       "root-slot",
					Filesystem: "squashfs",
				},
			},
			{
				Name:         "runtime-uki",
				Role:         katlosimage.ComponentRuntimeUKI,
				Path:         "components/boot/katl.efi",
				Format:       "uki",
				SizeBytes:    sizes["components/boot/katl.efi"],
				SHA256:       digests["components/boot/katl.efi"],
				Version:      version,
				Architecture: "x86_64",
				Compatibility: katlosimage.Compatibility{
					RuntimeInterface: "katl-runtime-1",
					RuntimeRoot: katlosimage.RuntimeRoot{
						Interface:      "katl-runtime-1",
						ArtifactPath:   "components/runtime/root.squashfs",
						ArtifactSHA256: digests["components/runtime/root.squashfs"],
					},
					KernelCommandLine: []string{"quiet"},
				},
				InstallTarget: katlosimage.InstallTarget{
					Kind:     "esp-or-xbootldr",
					Filename: "katl.efi",
				},
			},
		},
	}
	if err := os.MkdirAll(filepath.Join(imageRoot, "katlos"), 0o755); err != nil {
		t.Fatalf("create upgrade image index dir: %v", err)
	}
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("marshal upgrade image index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageRoot, "katlos", "image.json"), append(indexData, '\n'), 0o644); err != nil {
		t.Fatalf("write upgrade image index: %v", err)
	}
	imagePath := filepath.Join(runDir, "katlos-upgrade-"+version+"-x86_64.squashfs")
	if output, err := exec.Command("mksquashfs", imageRoot, imagePath, "-noappend", "-quiet").CombinedOutput(); err != nil {
		t.Fatalf("build upgrade image squashfs: %v\n%s", err, output)
	}
	imageInfo, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("stat upgrade image squashfs: %v", err)
	}
	imageSHA, err := fileSHA256(imagePath)
	if err != nil {
		t.Fatalf("hash upgrade image squashfs: %v", err)
	}
	expected := manifest.KatlosImage{
		LocalRef:         filepath.Base(imagePath),
		SHA256:           imageSHA,
		SizeBytes:        uint64(imageInfo.Size()),
		Version:          version,
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             katlosimage.RoleUpgrade,
	}
	payload, err := katlosimage.ResolveDirectory(context.Background(), imageRoot, expected)
	if err != nil {
		t.Fatalf("resolve upgrade image payload: %v", err)
	}
	return payload, imagePath
}

func rootTransfer(source string) string {
	return fmt.Sprintf(`[Transfer]
ProtectVersion=0

[Source]
Type=regular-file
Path=%s
MatchPattern=katl_@v.root.squashfs

[Target]
Type=partition
Path=auto
MatchPattern=katl_@v
MatchPartitionType=root
ReadOnly=1
InstancesMax=2
`, source)
}

func ukiTransfer(source string) string {
	return fmt.Sprintf(`[Transfer]
ProtectVersion=0

[Source]
Type=regular-file
Path=%s
MatchPattern=katl_@v.efi

[Target]
Type=regular-file
Path=/EFI/Linux
PathRelativeTo=boot
MatchPattern=katl_@v+@l-@d.efi katl_@v+@l.efi katl_@v.efi
Mode=0644
TriesLeft=1
TriesDone=0
InstancesMax=2
`, source)
}

func rootSlotsFromSpec(t *testing.T, spec string) (string, string, string, string) {
	t.Helper()
	switch {
	case strings.Contains(spec, `"slot": "root-a"`):
		return "root-a", "root-b", "KATL_ROOT_A", "KATL_ROOT_B"
	case strings.Contains(spec, `"slot": "root-b"`):
		return "root-b", "root-a", "KATL_ROOT_B", "KATL_ROOT_A"
	default:
		t.Fatalf("current generation spec does not identify root-a/root-b slot:\n%s", spec)
		return "", "", "", ""
	}
}

type candidateGenerationSpecInput struct {
	GenerationID   string
	RuntimeVersion string
	Slot           string
	PartitionUUID  string
	RootSHA256     string
	UKIPath        string
	LoaderEntry    string
	CreatedAt      time.Time
}

func candidateGenerationSpec(t *testing.T, currentSpec string, input candidateGenerationSpecInput) generation.GenerationSpec {
	t.Helper()
	var current generation.GenerationSpec
	if err := json.Unmarshal([]byte(currentSpec), &current); err != nil {
		t.Fatalf("decode current generation spec: %v\n%s", err, currentSpec)
	}
	spec := current
	spec.GenerationID = input.GenerationID
	spec.RuntimeVersion = input.RuntimeVersion
	spec.PreviousGenerationID = current.GenerationID
	spec.Root.Slot = input.Slot
	spec.Root.PartitionUUID = input.PartitionUUID
	spec.Root.RuntimeVersion = input.RuntimeVersion
	spec.Root.RuntimeArtifactSHA256 = input.RootSHA256
	spec.Boot.UKIPath = input.UKIPath
	spec.Boot.LoaderEntryPath = input.LoaderEntry
	spec.CreatedAt = input.CreatedAt
	if err := generation.ValidateGenerationSpec(spec); err != nil {
		t.Fatalf("candidate generation spec invalid: %v\n%#v", err, spec)
	}
	return spec
}

func partitionDiskAndNumber(t *testing.T, device string) (string, string) {
	t.Helper()
	if device == "" {
		t.Fatal("partition device is empty")
	}
	lastDigit := len(device) - 1
	for lastDigit >= 0 && device[lastDigit] >= '0' && device[lastDigit] <= '9' {
		lastDigit--
	}
	if lastDigit == len(device)-1 {
		t.Fatalf("partition device %q has no numeric suffix", device)
	}
	disk := device[:lastDigit+1]
	part := device[lastDigit+1:]
	if strings.HasSuffix(disk, "p") {
		disk = strings.TrimSuffix(disk, "p")
	}
	if disk == "" || part == "" || disk == device {
		t.Fatalf("could not split partition device %q", device)
	}
	return disk, part
}

func bootSelectionFromGuest(t *testing.T, ctx context.Context, guest *GuestControl) generation.BootSelectionRecord {
	t.Helper()
	data := readGuestFile(t, ctx, guest, "/var/lib/katl/boot/selection.json")
	selection, err := decodeGuestRecord[generation.BootSelectionRecord]([]byte(data), generation.BootSelectionRecordType)
	if err != nil {
		t.Fatalf("decode boot selection: %v\n%s", err, data)
	}
	if err := generation.ValidateBootSelection(selection); err != nil {
		t.Fatalf("guest boot selection is invalid: %v\n%s", err, data)
	}
	return selection
}

func generationFromGuest(t *testing.T, ctx context.Context, guest *GuestControl, generationID string) generation.GenerationSpec {
	t.Helper()
	spec, _ := generationRecordsFromGuest(t, ctx, guest, generationID)
	return spec
}

func generationRecordsFromGuest(t *testing.T, ctx context.Context, guest *GuestControl, generationID string) (generation.GenerationSpec, generation.GenerationStatus) {
	t.Helper()
	specData := readGuestFile(t, ctx, guest, "/var/lib/katl/generations/"+generationID+"/spec.json")
	statusData := readGuestFile(t, ctx, guest, "/var/lib/katl/generations/"+generationID+"/status.json")
	spec, err := decodeGuestRecord[generation.GenerationSpec]([]byte(specData), generation.GenerationSpecRecordType)
	if err != nil {
		t.Fatalf("decode candidate generation spec: %v\n%s", err, specData)
	}
	status, err := decodeGuestRecord[generation.GenerationStatus]([]byte(statusData), generation.GenerationStatusRecordType)
	if err != nil {
		t.Fatalf("decode candidate generation status: %v\n%s", err, statusData)
	}
	if err := generation.ValidateGenerationStatus(spec, status); err != nil {
		t.Fatalf("candidate generation invalid: %v\nspec=%s\nstatus=%s", err, specData, statusData)
	}
	return spec, status
}

func writeGuestJSON(t *testing.T, ctx context.Context, guest *GuestControl, path string, value any) {
	t.Helper()
	data, err := generation.MarshalCanonicalJSON(value)
	if err != nil {
		t.Fatalf("marshal guest JSON %s: %v", path, err)
	}
	recordType := ""
	switch value.(type) {
	case generation.BootSelectionRecord:
		recordType = generation.BootSelectionRecordType
	case generation.GenerationSpec:
		recordType = generation.GenerationSpecRecordType
	case generation.GenerationStatus:
		recordType = generation.GenerationStatusRecordType
	}
	if recordType != "" {
		data, err = persistedrecord.MarshalEnvelope(persistedrecord.Envelope{
			RecordType:    recordType,
			RecordVersion: 1,
			Payload:       data,
		})
		if err != nil {
			t.Fatalf("marshal guest boot selection envelope %s: %v", path, err)
		}
	}
	writeGuestFile(t, ctx, guest, path, data, 0o644)
}

func decodeGuestRecord[T any](data []byte, recordType string) (T, error) {
	var value T
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return value, err
	}
	if _, enveloped := fields["recordType"]; !enveloped {
		return value, json.Unmarshal(data, &value)
	}
	envelope, err := persistedrecord.DecodeEnvelope(data)
	if err != nil {
		return value, err
	}
	if envelope.RecordType != recordType || envelope.RecordVersion != 1 {
		return value, fmt.Errorf("got %s/v%d, want %s/v1", envelope.RecordType, envelope.RecordVersion, recordType)
	}
	return persistedrecord.DecodePayload[T](envelope)
}

func writeGuestFile(t *testing.T, ctx context.Context, guest *GuestControl, path string, content []byte, mode fs.FileMode) {
	t.Helper()
	if _, err := guest.WriteFile(ctx, GuestFileRequest{
		Name:     filepath.Base(path),
		Path:     path,
		Content:  content,
		Mode:     mode,
		Truncate: true,
	}); err != nil {
		t.Fatalf("write guest file %s: %v", path, err)
	}
}

func guestCommand(t *testing.T, ctx context.Context, guest *GuestControl, name string, argv ...string) GuestCommandArtifact {
	t.Helper()
	record, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name:         name,
		Argv:         argv,
		Timeout:      2 * time.Minute,
		StdoutLimit:  1 << 20,
		StderrLimit:  1 << 20,
		AllowFailure: false,
	})
	if err != nil {
		t.Fatalf("%s failed: %v\nstdout:\n%s\nstderr:\n%s", name, err, readOptionalFile(t, record.Stdout), readOptionalFile(t, record.Stderr))
	}
	return record
}

func guestCommandOutput(t *testing.T, ctx context.Context, guest *GuestControl, name string, argv ...string) string {
	t.Helper()
	record := guestCommand(t, ctx, guest, name, argv...)
	return readFile(t, record.Stdout)
}

func collectSysupdateFailureEvidence(ctx context.Context, guest *GuestControl) {
	for _, req := range []GuestCommandRequest{
		{Name: "sysupdate-status", Argv: []string{"/usr/lib/systemd/systemd-sysupdate", "--no-pager", "--verify=no", "--definitions=/var/lib/katl/test-artifacts/sysupdate/sysupdate.d", "list"}, AllowFailure: true},
		{Name: "root-labels", Argv: []string{"blkid", "-o", "export"}, AllowFailure: true, StdoutLimit: 1 << 20},
		{Name: "sysupdate-files", Argv: []string{"find", "/var/lib/katl/test-artifacts/sysupdate", "-maxdepth", "4", "-type", "f", "-print"}, AllowFailure: true},
		{Name: "efi-linux-files", Argv: []string{"find", "/efi/EFI/Linux", "-maxdepth", "1", "-type", "f", "-print"}, AllowFailure: true},
	} {
		_, _ = guest.RunCommand(ctx, req)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readOptionalFile(t *testing.T, path string) string {
	t.Helper()
	if path == "" {
		return ""
	}
	return readFile(t, path)
}
