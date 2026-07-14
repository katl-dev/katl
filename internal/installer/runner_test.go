package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
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

func TestRebootPersistsStatusBeforeRequest(t *testing.T) {
	store := &MemoryStateStore{}
	commands := &rebootCommandRunner{store: store}
	install := &Context{Commands: commands, Store: store}

	if err := (rebootStep{}).Run(context.Background(), install); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := []CommandCall{
		{Name: "sync"},
		{Name: "systemctl", Args: []string{"--no-block", "reboot"}},
	}
	if !reflect.DeepEqual(commands.calls, want) {
		t.Fatalf("command calls = %#v, want %#v", commands.calls, want)
	}
	if commands.statusAtReboot != installstatus.StateRebootRequested {
		t.Fatalf("status at reboot request = %q, want %q", commands.statusAtReboot, installstatus.StateRebootRequested)
	}
	if commands.statusAtSync != installstatus.StateRebootRequested {
		t.Fatalf("status at sync = %q, want persisted reboot status", commands.statusAtSync)
	}
}

func TestRebootRequestFailureIsRecorded(t *testing.T) {
	store := &MemoryStateStore{}
	commands := &rebootCommandRunner{store: store, rebootErr: errors.New("reboot transaction rejected")}
	install := &Context{
		Commands:  commands,
		Store:     store,
		Completed: []StepID{PrepareDisk},
	}

	err := NewRunner(Plan{rebootStep{}}, install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "request system reboot: reboot transaction rejected") {
		t.Fatalf("Run() error = %v, want reboot request failure", err)
	}
	if len(store.Statuses) != 2 {
		t.Fatalf("status records = %d, want reboot request and failure", len(store.Statuses))
	}
	if store.Statuses[0].State != installstatus.StateRebootRequested {
		t.Fatalf("first status = %q, want %q", store.Statuses[0].State, installstatus.StateRebootRequested)
	}
	if got := store.Statuses[1]; got.State != installstatus.StateFailedAfterMutation || !strings.Contains(got.LastError, "request system reboot") {
		t.Fatalf("failure status = %#v", got)
	}
}

func TestRunnerReportsStepBeforeExecution(t *testing.T) {
	store := &MemoryStateStore{}
	var reported []StepID
	install := &Context{
		Commands:   &NoopCommandRunner{},
		Store:      store,
		ReportStep: func(step StepID) { reported = append(reported, step) },
	}
	runner := NewRunner(Plan{stubStep{id: DiscoverInstallerInput}, stubStep{id: WaitForLocalConfig}}, install)
	if err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []StepID{DiscoverInstallerInput, WaitForLocalConfig}; !reflect.DeepEqual(reported, want) {
		t.Fatalf("reported steps = %#v, want %#v", reported, want)
	}
}

func TestBuildClusterIntentPersistsBootstrapPlanContext(t *testing.T) {
	root := t.TempDir()
	installedAt := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	manifestDoc := manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		Node: manifest.NodeConfig{
			Identity: manifest.NodeIdentity{
				Hostname: "cp-1-host",
				SSH:      manifest.SSHIdentity{AuthorizedKeys: []string{sshKey}},
			},
			SystemRole: "control-plane",
			Kubernetes: manifest.KubernetesConfig{
				Kubeadm: manifest.KubeadmReference{ConfigRef: "control-plane"},
			},
			Bootstrap: &manifest.BootstrapIntent{
				ClusterName:          "lab",
				InventoryNodeName:    "cp-1",
				NodeAddress:          "10.0.0.11",
				ControlPlaneEndpoint: "api.katl.test:6443",
				BootstrapProfileRef:  "control-plane",
				ProfileResolvedID:    "kubeadm:control-plane",
				KubernetesBundle:     "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1@sha256:" + strings.Repeat("1", 64),
				Access: manifest.BootstrapAccess{
					Method:        "agent",
					CredentialRef: "vsock:1234:10240",
				},
				Labels: map[string]string{"katl.dev/zone": "rack-a"},
				Taints: []manifest.NodeTaint{{Key: "node-role.kubernetes.io/control-plane", Effect: "NoSchedule"}},
			},
		},
		Install: manifest.InstallConfig{
			WipeTarget: true,
			TargetDisk: manifest.DiskSelector{ByID: "/dev/disk/by-id/ata-root", MinSizeMiB: 32768},
		},
		KatlosImage: manifest.KatlosImage{
			URL:              "https://example.invalid/katlos-install.squashfs",
			SHA256:           strings.Repeat("a", 64),
			SizeBytes:        1073741824,
			Version:          "2026.06.04",
			Architecture:     "x86_64",
			RuntimeInterface: "katl-runtime-1",
			Role:             "install",
		},
	}
	intent, err := BuildClusterIntent(ClusterIntentRequest{
		Manifest:           manifestDoc,
		KubeadmConfigs:     kubeadmPlans(),
		KubernetesVersion:  "v1.36.1",
		GenerationID:       "0",
		RequestDigest:      strings.Repeat("d", 64),
		InstalledAt:        installedAt,
		TargetDiskStableID: "/dev/disk/by-id/ata-root",
	})
	if err != nil {
		t.Fatalf("BuildClusterIntent() error = %v", err)
	}
	if intent.Inventory.ClusterName != "lab" || intent.Inventory.NodeName != "cp-1" || intent.Inventory.ControlPlaneEndpoint != "api.katl.test:6443" {
		t.Fatalf("inventory intent = %#v", intent.Inventory)
	}
	if intent.Inventory.Access.Method != "agent" || intent.Inventory.Access.CredentialRef != "vsock:1234:10240" {
		t.Fatalf("inventory access = %#v", intent.Inventory.Access)
	}
	if intent.Inventory.Labels["katl.dev/zone"] != "rack-a" || len(intent.Inventory.Taints) != 1 {
		t.Fatalf("labels/taints = %#v %#v", intent.Inventory.Labels, intent.Inventory.Taints)
	}
	if intent.Kubernetes.PayloadVersion != "v1.36.1" ||
		intent.Kubernetes.BundleSource != "https://ghcr.io/v2/katl-dev/kubernetes" ||
		intent.Kubernetes.BundleRef != "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1@sha256:"+strings.Repeat("1", 64) ||
		intent.Kubernetes.SysextPath != "" ||
		intent.Kubernetes.SysextSHA256 != "" {
		t.Fatalf("kubernetes intent = %#v", intent.Kubernetes)
	}
	if intent.Kubeadm == nil || intent.Kubeadm.ConfigRef != "control-plane" || intent.Kubeadm.ConfigPath != "/etc/katl/kubeadm/control-plane/config.yaml" {
		t.Fatalf("kubeadm intent = %#v", intent.Kubeadm)
	}
	if intent.BootstrapProfile == nil || intent.BootstrapProfile.Ref != "control-plane" || intent.BootstrapProfile.ResolvedID != "kubeadm:control-plane" {
		t.Fatalf("bootstrap profile = %#v", intent.BootstrapProfile)
	}
	if intent.BootstrapProfile.KubeadmInputDigest == "" || intent.BootstrapProfile.KubeadmInputDigest != intent.Kubeadm.InputDigest {
		t.Fatalf("profile/input digest = profile %#v kubeadm %#v", intent.BootstrapProfile, intent.Kubeadm)
	}
	if intent.Source.RequestDigest != strings.Repeat("d", 64) || intent.Source.KatlosImageSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("source intent = %#v", intent.Source)
	}

	path, err := WriteClusterIntent(ClusterIntentRequest{
		TargetRoot:         root,
		Manifest:           manifestDoc,
		KubeadmConfigs:     kubeadmPlans(),
		KubernetesVersion:  "v1.36.1",
		GenerationID:       "0",
		RequestDigest:      strings.Repeat("d", 64),
		InstalledAt:        installedAt,
		TargetDiskStableID: "/dev/disk/by-id/ata-root",
	})
	if err != nil {
		t.Fatalf("WriteClusterIntent() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cluster intent: %v", err)
	}
	if !strings.Contains(string(data), `"recordType": "katl.cluster.intent"`) || !strings.Contains(string(data), `"payload": {`) {
		t.Fatalf("cluster intent is not enveloped:\n%s", data)
	}
	stored, digest, err := ReadClusterIntent(root)
	if err != nil {
		t.Fatalf("ReadClusterIntent() error = %v", err)
	}
	if stored.Source.RequestDigest != intent.Source.RequestDigest || digest == "" {
		t.Fatalf("stored intent = %#v digest %q, want request digest %q", stored.Source, digest, intent.Source.RequestDigest)
	}
}

func TestReadClusterIntentRejectsUnsupportedEnvelopeVersion(t *testing.T) {
	data, err := persistedrecord.MarshalEnvelope(persistedrecord.Envelope{
		RecordType:    ClusterIntentRecordType,
		RecordVersion: 2,
		Payload:       []byte("{}\n"),
	})
	if err != nil {
		t.Fatalf("MarshalEnvelope() error = %v", err)
	}
	_, err = decodeClusterIntent(data)
	if err == nil || !strings.Contains(err.Error(), "unsupported persisted record") {
		t.Fatalf("decodeClusterIntent() error = %v, want unsupported persisted record", err)
	}
}

func TestInstalledKubernetesPayloadVersionFallsBackToGenerationRecord(t *testing.T) {
	install := &Context{
		Manifest: manifest.Manifest{
			Node: manifest.NodeConfig{
				Kubernetes: manifest.KubernetesConfig{
					Kubeadm: manifest.KubeadmReference{ConfigRef: "worker"},
				},
			},
		},
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"worker": {
				Name: "worker",
				Config: kubeadmconfig.File{
					RenderPath: "/etc/katl/kubeadm/worker/config.yaml",
					Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n"),
					Mode:       0o644,
				},
				Documents: []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "JoinConfiguration"}},
			},
		},
		LoaderRecord: &generation.Record{
			Sysexts: []generation.ExtensionRef{{
				Name:           "kubernetes",
				PayloadVersion: "v1.36.1",
			}},
		},
	}

	if got := installedKubernetesPayloadVersion(install); got != "v1.36.1" {
		t.Fatalf("installedKubernetesPayloadVersion() = %q, want generation sysext payload", got)
	}
}

func TestInstalledKubernetesPayloadVersionUsesExactBootstrapCatalogRef(t *testing.T) {
	install := &Context{
		Manifest: manifest.Manifest{
			Node: manifest.NodeConfig{
				Bootstrap: &manifest.BootstrapIntent{KubernetesCatalogRef: "v1.36.0"},
				Kubernetes: manifest.KubernetesConfig{
					Kubeadm: manifest.KubeadmReference{ConfigRef: "worker"},
				},
			},
		},
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"worker": {
				Name:      "worker",
				Documents: []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "JoinConfiguration"}},
			},
		},
	}

	if got := installedKubernetesPayloadVersion(install); got != "v1.36.0" {
		t.Fatalf("installedKubernetesPayloadVersion() = %q, want bootstrap catalog payload", got)
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
	if len(store.Statuses) != len(want) {
		t.Fatalf("status count = %d, want %d", len(store.Statuses), len(want))
	}
	finalStatus := store.Statuses[len(store.Statuses)-1]
	if finalStatus.State != installstatus.StateRebootRequested || finalStatus.CurrentStep != string(Reboot) {
		t.Fatalf("final status = %#v", finalStatus)
	}
	if finalStatus.RequestDigest == "" || finalStatus.KatlosImage.SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("status missing request/image metadata: %#v", finalStatus)
	}
	if !finalStatus.WipeTargetAccepted {
		t.Fatalf("status missing wipe target evidence: %#v", finalStatus)
	}
	if finalStatus.TargetDiskStableID != "/dev/disk/by-id/ata-root" || finalStatus.SelectedRootSlot != "root-a" {
		t.Fatalf("status target/generation fields = %#v", finalStatus)
	}
	targetStatus, err := installstatus.ReadFile(filepath.Join(install.TargetRoot, "var/lib/katl/install/status.json"))
	if err != nil {
		t.Fatalf("read target status: %v", err)
	}
	if targetStatus.State != installstatus.StateRebootRequested || targetStatus.InstalledGeneration != "2026.06.04-000" || !targetStatus.WipeTargetAccepted {
		t.Fatalf("target status = %#v", targetStatus)
	}
	if got := commandNames(commands.Calls); got != "sync systemctl" {
		t.Fatalf("command calls = %q, want sync and reboot request", got)
	}
}

func TestRunnerRecordsFailureStatus(t *testing.T) {
	store := &MemoryStateStore{}
	install := &Context{
		ManifestPath:   writeManifest(t),
		StateDir:       t.TempDir(),
		TargetRoot:     t.TempDir(),
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		Commands:       &NoopCommandRunner{},
		Store:          store,
	}

	err := NewRunner(PreseededManifestPlan(), install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "loader generation record is required") {
		t.Fatalf("Run() error = %v, want generation record failure", err)
	}
	if len(store.Statuses) == 0 {
		t.Fatal("no status records written")
	}
	finalStatus := store.Statuses[len(store.Statuses)-1]
	if finalStatus.State != installstatus.StateFailedAfterMutation {
		t.Fatalf("failure state = %q, want failed-after-mutation", finalStatus.State)
	}
	if finalStatus.LastError == "" || finalStatus.RefusalReason == "" || finalStatus.RetryHint == "" {
		t.Fatalf("failure status missing diagnostics: %#v", finalStatus)
	}
	if !finalStatus.DestructiveMutation {
		t.Fatalf("failure after install states should mark destructive mutation possible: %#v", finalStatus)
	}
	if !finalStatus.WipeTargetAccepted {
		t.Fatalf("failure status missing wipe target evidence: %#v", finalStatus)
	}
}

func TestRunnerUsesNormalizedRequestDigest(t *testing.T) {
	first := &MemoryStateStore{}
	second := &MemoryStateStore{}
	firstInstall := &Context{
		ManifestPath: writeManifest(t),
		Commands:     &NoopCommandRunner{},
		Store:        first,
	}
	secondInstall := &Context{
		ManifestPath: writeCompactManifest(t),
		Commands:     &NoopCommandRunner{},
		Store:        second,
	}

	if err := NewRunner(Plan{loadManifestStep{}}, firstInstall).Run(context.Background()); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if err := NewRunner(Plan{loadManifestStep{}}, secondInstall).Run(context.Background()); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	firstDigest := first.Statuses[len(first.Statuses)-1].RequestDigest
	secondDigest := second.Statuses[len(second.Statuses)-1].RequestDigest
	if firstDigest == "" || firstDigest != secondDigest {
		t.Fatalf("normalized digests = %q and %q, want equal", firstDigest, secondDigest)
	}
}

func TestRunnerResolvesKatlosImageBeforePlanning(t *testing.T) {
	store := &MemoryStateStore{}
	resolver := &recordingKatlosResolver{
		payload: katlosimage.Payload{
			Index: katlosimage.Index{
				Version:          "2026.06.04",
				Architecture:     "x86_64",
				RuntimeInterface: "katl-runtime-1",
			},
		},
	}
	install := &Context{
		ManifestPath:   writeManifest(t),
		Commands:       &NoopCommandRunner{},
		Store:          store,
		KatlosResolver: resolver,
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		LoaderRecord:   minimalRecord("2026.06.04-000"),
		TargetRoot:     t.TempDir(),
		Chown:          func(string, int, int) error { return nil },
		KubeadmConfigs: kubeadmPlans(),
	}

	plan := Plan{loadManifestStep{}, verifyKatlosImageStep{}, stubStep{id: PlanInstall}}
	if err := NewRunner(plan, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if resolver.image.SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("resolver image = %#v", resolver.image)
	}
	if install.KatlosImage == nil || install.KatlosImage.Index.Version != "2026.06.04" {
		t.Fatalf("resolved payload = %#v", install.KatlosImage)
	}
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest, VerifyTrust, PlanInstall}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerUsesInstallMediaImage(t *testing.T) {
	defaultImage := manifest.KatlosImage{
		LocalRef: "images/katlos-install.squashfs", SHA256: strings.Repeat("b", 64),
		SizeBytes: 4096, Version: "2026.7.0", Architecture: "x86_64",
		RuntimeInterface: "katl-runtime-1", Role: "install",
	}
	external := &recordingKatlosResolver{err: errString("external resolver must not run")}
	media := &recordingKatlosResolver{payload: katlosimage.Payload{Index: katlosimage.Index{Version: defaultImage.Version}}}
	install := &Context{
		ManifestPath:        writeManifestWithoutImage(t),
		Commands:            &NoopCommandRunner{},
		Store:               &MemoryStateStore{},
		KatlosResolver:      external,
		MediaKatlosResolver: media,
		DefaultKatlosImage:  defaultImage,
	}
	if err := NewRunner(Plan{loadManifestStep{}, verifyKatlosImageStep{}}, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !install.KatlosImageFromMedia || media.image != defaultImage {
		t.Fatalf("media selected = %v, image = %#v", install.KatlosImageFromMedia, media.image)
	}
	if !manifest.KatlosImageEmpty(external.image) {
		t.Fatalf("external resolver image = %#v", external.image)
	}
}

func TestRunnerRejectsKatlosImageBeforeMutation(t *testing.T) {
	store := &MemoryStateStore{}
	install := &Context{
		ManifestPath:   writeManifest(t),
		TargetRoot:     t.TempDir(),
		Commands:       &NoopCommandRunner{},
		Store:          store,
		KatlosResolver: &recordingKatlosResolver{err: errString("image digest mismatch")},
	}

	plan := Plan{loadManifestStep{}, verifyKatlosImageStep{}, stubStep{id: PrepareDisk}}
	err := NewRunner(plan, install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "image digest mismatch") {
		t.Fatalf("Run() error = %v, want image digest mismatch", err)
	}
	if install.KatlosImage != nil {
		t.Fatalf("resolved payload = %#v, want nil", install.KatlosImage)
	}
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest}) {
		t.Fatalf("completed steps = %#v", got)
	}
	if len(store.Statuses) == 0 {
		t.Fatal("no status records written")
	}
	status := store.Statuses[len(store.Statuses)-1]
	if status.State != installstatus.StateFailedBeforeMutation || status.CurrentStep != string(VerifyTrust) || status.DestructiveMutation {
		t.Fatalf("failure status = %#v", status)
	}
	if _, err := os.Stat(filepath.Join(install.TargetRoot, "var/lib/katl/install/status.json")); !os.IsNotExist(err) {
		t.Fatalf("target status err = %v, want no target write before mutation", err)
	}
}

func TestRunnerPlansInstallFromKatlosImagePayload(t *testing.T) {
	store := &MemoryStateStore{}
	payload := planningPayload()
	install := &Context{
		ManifestPath:      writeManifest(t),
		Commands:          &NoopCommandRunner{},
		Store:             store,
		KatlosResolver:    &recordingKatlosResolver{payload: payload},
		Discovery:         discovery.StaticDiscoverySource{Facts: planningFacts()},
		RootPartitionUUID: "11111111-2222-3333-4444-555555555555",
		GenerationID:      "2026.06.06-001",
	}

	plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}}
	if err := NewRunner(plan, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if install.DiskLayout == nil || install.DiskLayout.TargetDiskPath != "/dev/nvme0n1" {
		t.Fatalf("disk layout = %#v", install.DiskLayout)
	}
	if install.RootSlotPlan == nil || install.RootSlotPlan.Slot != "root-a" || install.RootSlotPlan.ArtifactDigest != payload.Runtime.SHA256 {
		t.Fatalf("root slot plan = %#v", install.RootSlotPlan)
	}
	if install.LoaderRecord == nil {
		t.Fatal("loader record is nil")
	}
	record := install.LoaderRecord
	if record.GenerationID != "0" || record.Root.RuntimeArtifactSHA256 != payload.Runtime.SHA256 {
		t.Fatalf("record root fields = %#v", record.Root)
	}
	if record.Root.PartitionUUID != "11111111-2222-3333-4444-555555555555" || record.Boot.UKIPath != "/efi/EFI/Linux/katl-0.efi" {
		t.Fatalf("record boot/root target = %#v %#v", record.Root, record.Boot)
	}
	if record.GenerationID != "0" {
		t.Fatalf("generation id = %q", record.GenerationID)
	}
	if len(record.Sysexts) != 0 {
		t.Fatalf("generation 0 selected sysexts = %#v", record.Sysexts)
	}
	if record.Confexts != nil {
		t.Fatalf("planned record should not include node confext before WriteInstallRecord: %#v", record.Confexts)
	}
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest, CollectHardwareFacts, VerifyTrust, PlanInstall}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerCapturesRootUUIDAfterMountTarget(t *testing.T) {
	store := &MemoryStateStore{}
	payload := planningPayload()
	commands := &recordingOutputRunner{
		outputs: map[string][]byte{
			"blkid -t PARTLABEL=KATL_ESP -s PARTUUID -o value":    []byte("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n"),
			"blkid -t PARTLABEL=KATL_ROOT_A -s PARTUUID -o value": []byte("11111111-2222-3333-4444-555555555555\n"),
		},
	}
	install := &Context{
		ManifestPath:   writeManifest(t),
		Commands:       commands,
		Store:          store,
		KatlosResolver: &recordingKatlosResolver{payload: payload},
		Discovery:      discovery.StaticDiscoverySource{Facts: planningFacts()},
		GenerationID:   "2026.06.06-001",
	}

	plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}, mountTargetStep{}}
	if err := NewRunner(plan, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if install.RootPartitionUUID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("root partition UUID = %q", install.RootPartitionUUID)
	}
	if install.LoaderRecord == nil {
		t.Fatal("loader record is nil")
	}
	if install.LoaderRecord.Root.PartitionUUID != install.RootPartitionUUID || install.LoaderRecord.Root.Slot != string(disk.RootSlotA) {
		t.Fatalf("loader root = %#v", install.LoaderRecord.Root)
	}
	if got := strings.Join(commands.OutputCalls, ";"); got != "blkid -t PARTLABEL=KATL_ESP -s PARTUUID -o value;blkid -t PARTLABEL=KATL_ROOT_A -s PARTUUID -o value" {
		t.Fatalf("output calls = %#v", commands.OutputCalls)
	}
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest, CollectHardwareFacts, VerifyTrust, PlanInstall, MountTarget}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerMountTargetRequiresOutputForRootUUID(t *testing.T) {
	store := &MemoryStateStore{}
	payload := planningPayload()
	install := &Context{
		ManifestPath:   writeManifest(t),
		Commands:       &NoopCommandRunner{},
		Store:          store,
		KatlosResolver: &recordingKatlosResolver{payload: payload},
		Discovery:      discovery.StaticDiscoverySource{Facts: planningFacts()},
	}

	plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}, mountTargetStep{}}
	err := NewRunner(plan, install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "support output") {
		t.Fatalf("Run() error = %v, want output command runner failure", err)
	}
	if install.LoaderRecord != nil {
		t.Fatalf("loader record = %#v, want nil", install.LoaderRecord)
	}
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest, CollectHardwareFacts, VerifyTrust, PlanInstall}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerExecutesDiskOperationSteps(t *testing.T) {
	store := &MemoryStateStore{}
	payload := planningPayload()
	commands := &recordingOutputRunner{
		outputs: map[string][]byte{
			"blkid -t PARTLABEL=KATL_ESP -s PARTUUID -o value":    []byte("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n"),
			"blkid -t PARTLABEL=KATL_ROOT_A -s PARTUUID -o value": []byte("11111111-2222-3333-4444-555555555555\n"),
		},
	}
	install := &Context{
		ManifestPath:   writeManifest(t),
		TargetRoot:     t.TempDir(),
		Commands:       commands,
		Store:          store,
		KatlosResolver: &recordingKatlosResolver{payload: payload},
		Discovery:      discovery.StaticDiscoverySource{Facts: planningFacts()},
	}

	plan := Plan{
		loadManifestStep{},
		collectHardwareFactsStep{},
		verifyKatlosImageStep{},
		planInstallStep{},
		prepareDiskStep{},
		createPartitionsStep{},
		formatFilesystemsStep{},
		mountTargetStep{},
	}
	if err := NewRunner(plan, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	calls := commandNames(commands.Calls)
	for _, name := range []string{"wipefs", "sfdisk", "partprobe", "udevadm", "mkfs.vfat", "mkfs.ext4", "mkdir", "mount", "bootctl"} {
		if !strings.Contains(calls, name) {
			t.Fatalf("command calls %q missing %s; calls = %#v", calls, name, commands.Calls)
		}
	}
	if commands.Inputs["sfdisk"] == "" || !strings.Contains(commands.Inputs["sfdisk"], `name="KATL_ROOT_A"`) {
		t.Fatalf("sfdisk input = %q", commands.Inputs["sfdisk"])
	}
	if install.LoaderRecord == nil || install.LoaderRecord.Root.PartitionUUID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("loader record = %#v", install.LoaderRecord)
	}
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest, CollectHardwareFacts, VerifyTrust, PlanInstall, PrepareDisk, CreatePartitions, FormatFilesystems, MountTarget}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerInstallsSingleKatlosImageThroughTargetVerification(t *testing.T) {
	payload, contents := writeInstallPayload(t)
	store := &MemoryStateStore{}
	targetRoot := t.TempDir()
	rootTarget := newRunnerRootSlot(len(contents.runtime) + 4096)
	commands := &recordingOutputRunner{
		outputs: map[string][]byte{
			"blkid -t PARTLABEL=KATL_ESP -s PARTUUID -o value":    []byte("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n"),
			"blkid -t PARTLABEL=KATL_ROOT_A -s PARTUUID -o value": []byte("11111111-2222-3333-4444-555555555555\n"),
		},
	}
	discovery := &sequenceDiscoverySource{facts: []discovery.HardwareFacts{
		planningFacts(),
		appliedLayoutFacts(targetRoot),
	}}
	install := &Context{
		ManifestPath:   writeManifestWithNode(t, `, "kubernetes": {"kubeadm": {"configRef": "control-plane"}}, "bootstrap": {"kubernetesBundle": "ghcr.io/katl-dev/kubernetes:v1.34.8-katl.1@sha256:`+strings.Repeat("1", 64)+`"}`),
		TargetRoot:     targetRoot,
		Commands:       commands,
		Store:          store,
		KatlosResolver: &recordingKatlosResolver{payload: payload},
		Discovery:      discovery,
		RootSlotTarget: rootTarget,
		GenerationID:   "2026.06.06-001",
		KubeadmConfigs: kubeadmPlans(),
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		Chown:          func(string, int, int) error { return nil },
	}

	if err := NewRunner(PreseededManifestPlan(), install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if discovery.calls != 2 {
		t.Fatalf("discovery calls = %d, want planning and verification", discovery.calls)
	}
	if got := string(rootTarget.data[:len(contents.runtime)]); got != string(contents.runtime) {
		t.Fatalf("root slot bytes = %q, want runtime payload", got)
	}
	if !rootTarget.synced {
		t.Fatal("root slot target was not synced")
	}
	if install.LoaderRecord == nil {
		t.Fatal("loader record is nil")
	}
	if install.LoaderRecord.GenerationID != "0" || install.LoaderRecord.Root.PartitionUUID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("loader record = %#v", install.LoaderRecord)
	}
	if len(install.LoaderRecord.Sysexts) != 0 {
		t.Fatalf("generation 0 selected sysexts = %#v", install.LoaderRecord.Sysexts)
	}
	if len(install.LoaderRecord.Confexts) != 1 || install.LoaderRecord.Confexts[0].Name != "katl-node" {
		t.Fatalf("confext metadata = %#v", install.LoaderRecord.Confexts)
	}
	assertText(t, filepath.Join(targetRoot, "efi/EFI/Linux/katl-0.efi"), string(contents.boot))
	assertMissing(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/sysext/katl-kubernetes.raw"))
	assertMissing(t, filepath.Join(targetRoot, "var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw"))
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/identity/machine-id"), "30313233343536373839616263646566\n")
	assertContains(t, filepath.Join(targetRoot, "etc/systemd/system/var.mount"), "What=PARTUUID=11111111-2222-3333-4444-555555555555")
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/confext/etc/katl/node.json"), `"configRef": "control-plane"`)
	assertMissing(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/confext/etc/katl/kubeadm/control-plane/config.yaml"))
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/cluster/kubeadm/control-plane/config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n")
	assertDirEmpty(t, filepath.Join(targetRoot, "etc/kubernetes"))
	assertMissing(t, filepath.Join(targetRoot, "etc/systemd/system/multi-user.target.wants/kubelet.service"))
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/metadata.json"), `"generationID": "0"`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/metadata.json"), `"loaderEntryPath": "loader/entries/katl-0.conf"`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/spec.json"), `"sysexts": []`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/spec.json"), `"loaderEntryPath": "loader/entries/katl-0.conf"`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/boot/selection.json"), `"defaultGenerationID": "0"`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/boot/selection.json"), `"bootedGenerationID": "0"`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/boot/selection.json"), `"defaultBootEntry": "loader/entries/katl-0.conf"`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/cluster/intent.json"), `"payloadVersion": "v1.34.8"`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/cluster/intent.json"), `"bundleSource": "https://ghcr.io/v2/katl-dev/kubernetes"`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/cluster/intent.json"), `"bundleRef": "ghcr.io/katl-dev/kubernetes:v1.34.8-katl.1@sha256:`+strings.Repeat("1", 64)+`"`)
	installedManifestFile, err := os.Open(filepath.Join(targetRoot, "var/lib/katl/install/manifest.json"))
	if err != nil {
		t.Fatalf("open installed manifest: %v", err)
	}
	installedManifest, err := manifest.Decode(installedManifestFile)
	closeErr := installedManifestFile.Close()
	if err != nil {
		t.Fatalf("decode installed manifest: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close installed manifest: %v", closeErr)
	}
	if installedManifest.Node.Identity.Hostname != "lab-node-01" || installedManifest.Node.Kubernetes.Kubeadm.ConfigRef != "control-plane" {
		t.Fatalf("installed manifest = %#v", installedManifest.Node)
	}
	for _, name := range []string{"wipefs", "sfdisk", "partprobe", "udevadm", "mkfs.vfat", "mkfs.ext4", "mkdir", "mount", "bootctl"} {
		if !strings.Contains(commandNames(commands.Calls), name) {
			t.Fatalf("command calls missing %s: %#v", name, commands.Calls)
		}
	}
	if commands.Inputs["sfdisk"] == "" || !strings.Contains(commands.Inputs["sfdisk"], `name="KATL_ROOT_A"`) {
		t.Fatalf("sfdisk input = %q", commands.Inputs["sfdisk"])
	}
	if got := install.Completed; !reflect.DeepEqual(got, PreseededManifestPlan().IDs()) {
		t.Fatalf("completed steps = %#v, want %#v", got, PreseededManifestPlan().IDs())
	}
	finalStatus := store.Statuses[len(store.Statuses)-1]
	if finalStatus.State != installstatus.StateRebootRequested || finalStatus.InstalledGeneration != "0" {
		t.Fatalf("final status = %#v", finalStatus)
	}
}

func TestRunnerVerifiesAppliedTargetLayout(t *testing.T) {
	store := &MemoryStateStore{}
	payload := planningPayload()
	targetRoot := t.TempDir()
	discovery := &sequenceDiscoverySource{facts: []discovery.HardwareFacts{
		planningFacts(),
		appliedLayoutFacts(targetRoot),
	}}
	install := &Context{
		ManifestPath:   writeManifest(t),
		TargetRoot:     targetRoot,
		Commands:       &NoopCommandRunner{},
		Store:          store,
		KatlosResolver: &recordingKatlosResolver{payload: payload},
		Discovery:      discovery,
	}

	plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}, verifyTargetStep{}}
	if err := NewRunner(plan, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if discovery.calls != 2 {
		t.Fatalf("discovery calls = %d, want 2", discovery.calls)
	}
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest, CollectHardwareFacts, VerifyTrust, PlanInstall, VerifyTarget}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerRejectsMissingAppliedMount(t *testing.T) {
	store := &MemoryStateStore{}
	payload := planningPayload()
	targetRoot := t.TempDir()
	applied := appliedLayoutFacts(targetRoot)
	applied.Mounts = applied.Mounts[:1]
	install := &Context{
		ManifestPath:   writeManifest(t),
		TargetRoot:     targetRoot,
		Commands:       &NoopCommandRunner{},
		Store:          store,
		KatlosResolver: &recordingKatlosResolver{payload: payload},
		Discovery: &sequenceDiscoverySource{facts: []discovery.HardwareFacts{
			planningFacts(),
			applied,
		}},
	}

	plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}, verifyTargetStep{}}
	err := NewRunner(plan, install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "state partition is not mounted") {
		t.Fatalf("Run() error = %v, want missing state mount", err)
	}
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest, CollectHardwareFacts, VerifyTrust, PlanInstall}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerInstallsKatlosImageComponents(t *testing.T) {
	payload, contents := writeInstallPayload(t)
	store := &MemoryStateStore{}
	targetRoot := t.TempDir()
	rootTarget := newRunnerRootSlot(len(contents.runtime) + 4096)
	install := &Context{
		TargetRoot:  targetRoot,
		Commands:    &NoopCommandRunner{},
		Store:       store,
		KatlosImage: &payload,
		RootSlotPlan: &disk.RootSlotWritePlan{
			Slot:              disk.RootSlotA,
			TargetPartition:   disk.RootSlotTarget{GPTLabel: disk.GPTLabelRootA},
			ArtifactDigest:    payload.Runtime.SHA256,
			ExpectedSizeBytes: payload.Runtime.SizeBytes,
		},
		RootSlotTarget: rootTarget,
		LoaderRecord: &generation.Record{
			GenerationID: "2026.06.06-001",
			Root: generation.RootSelection{
				Slot: "root-a",
			},
			Boot: generation.BootSelection{
				UKIPath: "/efi/EFI/Linux/katl-2026.06.06-001.efi",
			},
		},
	}

	plan := Plan{installRootSlotStep{}, installBootArtifactsStep{}, installExtensionsStep{}}
	if err := NewRunner(plan, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if got := string(rootTarget.data[:len(contents.runtime)]); got != string(contents.runtime) {
		t.Fatalf("root slot bytes = %q, want runtime payload", got)
	}
	if !rootTarget.synced {
		t.Fatal("root slot target was not synced")
	}
	assertText(t, filepath.Join(targetRoot, "efi/EFI/Linux/katl-2026.06.06-001.efi"), string(contents.boot))
	assertMissing(t, filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.06-001/sysext/kubernetes.raw"))
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{InstallRootSlot, InstallBootArtifacts, InstallExtensions}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerOpensRootSlotTarget(t *testing.T) {
	payload, contents := writeInstallPayload(t)
	store := &MemoryStateStore{}
	rootTarget := newRunnerRootSlot(len(contents.runtime) + 4096)
	opener := &recordingRootSlotOpener{target: rootTarget}
	install := &Context{
		Commands:    &NoopCommandRunner{},
		Store:       store,
		KatlosImage: &payload,
		RootSlotPlan: &disk.RootSlotWritePlan{
			Slot:              disk.RootSlotA,
			TargetPartition:   disk.RootSlotTarget{Name: "root-a", GPTLabel: disk.GPTLabelRootA},
			ArtifactDigest:    payload.Runtime.SHA256,
			ExpectedSizeBytes: payload.Runtime.SizeBytes,
		},
		RootSlotOpener: opener,
	}

	if err := NewRunner(Plan{installRootSlotStep{}}, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if opener.targetLabel != disk.GPTLabelRootA {
		t.Fatalf("opened label = %q, want %s", opener.targetLabel, disk.GPTLabelRootA)
	}
	if !rootTarget.closed {
		t.Fatal("opened root slot target was not closed")
	}
	if got := string(rootTarget.data[:len(contents.runtime)]); got != string(contents.runtime) {
		t.Fatalf("root slot bytes = %q, want runtime payload", got)
	}
}

func TestRunnerRecordsRefusalBeforeMutationStatus(t *testing.T) {
	store := &MemoryStateStore{}
	manifestPath := filepath.Join(t.TempDir(), "install.json")
	if err := os.WriteFile(manifestPath, []byte(`{"apiVersion":"install.katl.dev/v1alpha1"`), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	install := &Context{
		ManifestPath: manifestPath,
		StateDir:     t.TempDir(),
		TargetRoot:   t.TempDir(),
		Commands:     &NoopCommandRunner{},
		Store:        store,
		InputMode:    installstatus.InputModeTest,
		InputSource:  "https://user:secret@example.invalid/install.json?token=secret",
	}

	err := NewRunner(PreseededManifestPlan(), install).Run(context.Background())
	if err == nil {
		t.Fatal("Run() error = nil, want manifest refusal")
	}
	if len(store.Statuses) == 0 {
		t.Fatal("no status records written")
	}
	status := store.Statuses[len(store.Statuses)-1]
	if status.State != installstatus.StateFailedBeforeMutation || status.DestructiveMutation {
		t.Fatalf("refusal status = %#v", status)
	}
	if !strings.Contains(status.RetryHint, "before disk mutation") {
		t.Fatalf("retry hint = %q, want before mutation guidance", status.RetryHint)
	}
	if strings.Contains(status.InputSource, "secret") {
		t.Fatalf("input source leaked secret: %#v", status)
	}
	if _, err := os.Stat(filepath.Join(install.TargetRoot, "var/lib/katl/install/status.json")); !os.IsNotExist(err) {
		t.Fatalf("target status err = %v, want no target write before mutation", err)
	}
}

func TestRunnerRefusesChangedInterruptedRequest(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStateStore(dir)
	previous := installstatus.New(installstatus.StateFailedAfterMutation, time.Time{})
	previous.RequestDigest = strings.Repeat("b", 64)
	previous.DestructiveMutation = true
	if err := store.SaveStatus(context.Background(), previous); err != nil {
		t.Fatalf("SaveStatus() error = %v", err)
	}
	install := &Context{
		ManifestPath: writeManifest(t),
		StateDir:     dir,
		TargetRoot:   t.TempDir(),
		Commands:     &NoopCommandRunner{},
		Store:        store,
		InputMode:    installstatus.InputModeTest,
		InputSource:  "https://example.invalid/install.json",
	}

	err := NewRunner(Plan{loadManifestStep{}}, install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "install refused") {
		t.Fatalf("Run() error = %v, want install refused", err)
	}
	status, err := store.LoadStatus(context.Background())
	if err != nil {
		t.Fatalf("LoadStatus() error = %v", err)
	}
	if status.State != installstatus.StateInstallRefused || status.DestructiveMutation {
		t.Fatalf("rerun status = %#v", status)
	}
	if status.RequestDigest == previous.RequestDigest || status.RequestDigest == "" {
		t.Fatalf("rerun digest = %q, previous %q", status.RequestDigest, previous.RequestDigest)
	}
}

func TestRunnerSurfacesFailureStatusWriteError(t *testing.T) {
	store := failingStatusStore{}
	install := &Context{
		Commands: &NoopCommandRunner{},
		Store:    store,
	}

	err := NewRunner(Plan{failingStep{id: VerifyTrust, err: errString("verification failed")}}, install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "record failure status") {
		t.Fatalf("Run() error = %v, want status write failure", err)
	}
}

func TestRunnerPersistsFailedVerificationStatusToTarget(t *testing.T) {
	store := &MemoryStateStore{}
	targetRoot := t.TempDir()
	install := &Context{
		ManifestPath:   writeManifest(t),
		StateDir:       t.TempDir(),
		TargetRoot:     targetRoot,
		BootRoot:       filepath.Join(t.TempDir(), "efi"),
		LoaderRecord:   minimalRecord("2026.06.04-001"),
		IdentityRandom: bytes.NewReader([]byte("0123456789abcdef")),
		Commands:       &NoopCommandRunner{},
		Store:          store,
		Chown:          func(string, int, int) error { return nil },
		InputMode:      installstatus.InputModeTest,
		InputSource:    "https://user:secret@example.invalid/install.json?token=secret",
	}
	plan := Plan{
		loadManifestStep{},
		stubStep{id: SelectNode},
		stubStep{id: CollectHardwareFacts},
		stubStep{id: VerifyTrust},
		stubStep{id: PlanInstall},
		stubStep{id: PrepareDisk},
		stubStep{id: CreatePartitions},
		stubStep{id: FormatFilesystems},
		stubStep{id: MountTarget},
		stubStep{id: InstallRootSlot},
		stubStep{id: InstallBootArtifacts},
		stubStep{id: InstallExtensions},
		installSeedStep{},
		stubStep{id: InstallMountUnits},
		writeInstallRecordStep{},
		failingStep{id: VerifyTarget, err: errString("runtime verification failed: https://user:secret@example.invalid/log?token=secret")},
	}

	err := NewRunner(plan, install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "runtime verification failed") {
		t.Fatalf("Run() error = %v, want verification failure", err)
	}

	targetStatus, err := installstatus.ReadFile(filepath.Join(targetRoot, "var/lib/katl/install/status.json"))
	if err != nil {
		t.Fatalf("read target status: %v", err)
	}
	if targetStatus.State != installstatus.StateFailedAfterMutation || !targetStatus.DestructiveMutation {
		t.Fatalf("target failure status = %#v", targetStatus)
	}
	if strings.Contains(targetStatus.InputSource, "secret") || strings.Contains(targetStatus.LastError, "secret") {
		t.Fatalf("target status leaked secret: %#v", targetStatus)
	}
}

func TestRunnerInstallsIdentity(t *testing.T) {
	store := &MemoryStateStore{}
	targetRoot := t.TempDir()
	bootRoot := t.TempDir()
	record := *minimalRecord("2026.06.01-005")
	record.Boot.UKIPath = "/efi/EFI/Linux/katl.efi"
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
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.01-005/confext/etc/extension-release.d/extension-release.katl-node"), "ID=fedora\nVERSION_ID=0.1.0\nCONFEXT_LEVEL=1\n")
}

func TestRunnerInstallsMountUnits(t *testing.T) {
	store := &MemoryStateStore{}
	targetRoot := t.TempDir()
	record := generation.Record{
		GenerationID: "2026.06.06-001",
		Root: generation.RootSelection{
			Slot:          "root-a",
			PartitionUUID: "11111111-2222-3333-4444-555555555555",
		},
		Boot: generation.BootSelection{
			UKIPath: "/efi/EFI/Linux/katl.efi",
		},
	}
	install := &Context{
		TargetRoot:   targetRoot,
		LoaderRecord: &record,
		Commands:     &NoopCommandRunner{},
		Store:        store,
	}

	if err := NewRunner(Plan{installMountUnitsStep{}}, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertContains(t, filepath.Join(targetRoot, "etc/systemd/system/var.mount"), "What=PARTUUID=11111111-2222-3333-4444-555555555555")
	assertContains(t, filepath.Join(targetRoot, "etc/systemd/system/etc-kubernetes.mount"), "Where=/etc/kubernetes")
	assertContains(t, filepath.Join(targetRoot, "etc/systemd/system/katl-kubeadm-ready.target"), "Requires=systemd-sysext.service systemd-confext.service containerd.service kubelet.service etc-kubernetes.mount")
	assertMissing(t, filepath.Join(targetRoot, "etc/systemd/system/multi-user.target.wants/katl-kubeadm-ready.target"))
	assertContains(t, filepath.Join(targetRoot, "etc/tmpfiles.d/katl-state.conf"), "d /var/lib/katl/kubernetes/etc-kubernetes 0755 root root -")
	assertContains(t, filepath.Join(targetRoot, "etc/tmpfiles.d/katl-state.conf"), "d /var/lib/etcd 0755 root root -")
	assertDir(t, filepath.Join(targetRoot, "etc/kubernetes"), 0o755)
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{InstallMountUnits}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerRejectsMountUnitsWithoutGenerationRecord(t *testing.T) {
	install := &Context{
		TargetRoot: t.TempDir(),
		Commands:   &NoopCommandRunner{},
		Store:      &MemoryStateStore{},
	}

	err := NewRunner(Plan{installMountUnitsStep{}}, install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "loader generation record is required") {
		t.Fatalf("Run() error = %v, want generation record failure", err)
	}
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
		CreatedAt: time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
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
	assertMissing(t, filepath.Join(confextDir, "etc/katl/kubeadm/control-plane/config.yaml"))
	assertText(t, filepath.Join(confextDir, "etc/extension-release.d/extension-release.katl-node"), "ID=fedora\nVERSION_ID=0.1.0\nCONFEXT_LEVEL=1\n")
	assertText(t, filepath.Join(confextDir, "etc/katl/node.json"), `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "NodeMetadata",
  "identity": {
    "hostname": "lab-node-01"
  },
  "systemRole": "control-plane",
  "kubeadm": {
    "configRef": "control-plane",
    "configPath": "/etc/katl/kubeadm/control-plane/config.yaml",
    "intent": "control-plane"
  },
  "kubernetes": {}
}
`)

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
	if decoded.Root.Slot != "root-a" || len(decoded.Sysexts) != 0 {
		t.Fatalf("metadata did not preserve clean generation 0 selection: %#v", decoded)
	}
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.04-001/spec.json"), `"sysexts": []`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.04-001/status.json"), `"commitState": "committed"`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/cluster/intent.json"), `"configRef": "control-plane"`)
	if len(decoded.Confexts) != 1 || decoded.Confexts[0].Path != "/var/lib/katl/generations/2026.06.04-001/confext" {
		t.Fatalf("confext metadata = %#v", decoded.Confexts)
	}
	if decoded.Confexts[0].ActivationPath != "/run/confexts/katl-node" || decoded.Confexts[0].SHA256 != digest {
		t.Fatalf("confext activation/digest = %#v, digest %s", decoded.Confexts[0], digest)
	}
	if decoded.Confexts[0].Compatibility.ID != "fedora" || decoded.Confexts[0].Compatibility.ConfextLevel != 1 {
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

const sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"

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
			},
			"systemRole": "control-plane"` + nodeExtra + `
		},
		"install": {
			"wipeTarget": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}
		},
		"katlosImage": {
			"url": "https://example.invalid/katlos-install.squashfs",
			"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sizeBytes": 1073741824,
			"version": "2026.06.04",
			"architecture": "x86_64",
			"runtimeInterface": "katl-runtime-1",
			"role": "install"
		}
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func writeCompactManifest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "install.json")
	data := `{"apiVersion":"install.katl.dev/v1alpha1","kind":"InstallManifest","node":{"identity":{"hostname":"lab-node-01","ssh":{"authorizedKeys":["` + sshKey + `"]}},"systemRole":"control-plane"},"install":{"wipeTarget":true,"targetDisk":{"byID":"/dev/disk/by-id/ata-root","minSizeMiB":32768}},"katlosImage":{"url":"https://example.invalid/katlos-install.squashfs","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sizeBytes":1073741824,"version":"2026.06.04","architecture":"x86_64","runtimeInterface":"katl-runtime-1","role":"install"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func writeManifestWithoutImage(t *testing.T) string {
	t.Helper()
	path := writeManifest(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	delete(object, "katlosImage")
	data, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func minimalRecord(id string) *generation.Record {
	return &generation.Record{
		GenerationID:   id,
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
			UKIPath: "/efi/EFI/Linux/katl-" + strings.TrimSpace(id) + ".efi",
		},
		CreatedAt: time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
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
			Documents: []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "InitConfiguration"}},
		},
	}
}

func planningPayload() katlosimage.Payload {
	runtimeSHA := strings.Repeat("a", 64)
	bootSHA := strings.Repeat("b", 64)
	return katlosimage.Payload{
		Root: "/payload",
		Index: katlosimage.Index{
			Version:          "2026.06.06",
			BuildID:          "test-build",
			Architecture:     "x86_64",
			RuntimeInterface: "katl-runtime-1",
		},
		Runtime: katlosimage.Component{
			Name:         "runtime-root",
			Role:         katlosimage.ComponentRuntimeRoot,
			Path:         "components/runtime/root.squashfs",
			SizeBytes:    2 * 1024 * 1024,
			SHA256:       runtimeSHA,
			Version:      "2026.06.06",
			Architecture: "x86_64",
		},
		Boot: katlosimage.Component{
			Name:         "runtime-uki",
			Role:         katlosimage.ComponentRuntimeUKI,
			Path:         "components/boot/katl.efi",
			SizeBytes:    4096,
			SHA256:       bootSHA,
			Version:      "2026.06.06",
			Architecture: "x86_64",
			Compatibility: katlosimage.Compatibility{
				RuntimeInterface:  "katl-runtime-1",
				KernelCommandLine: []string{"rootfstype=squashfs", "ro"},
			},
		},
	}
}

func planningFacts() discovery.HardwareFacts {
	return discovery.HardwareFacts{
		BlockDevices: []discovery.BlockDevice{
			{
				Name:      "nvme0n1",
				Path:      "/dev/nvme0n1",
				Type:      discovery.DeviceDisk,
				ByID:      []string{"/dev/disk/by-id/ata-root"},
				SizeBytes: 64 * 1024 * 1024 * 1024,
			},
		},
	}
}

func appliedLayoutFacts(targetRoot string) discovery.HardwareFacts {
	return discovery.HardwareFacts{
		BlockDevices: []discovery.BlockDevice{
			{
				Name:      "nvme0n1",
				Path:      "/dev/nvme0n1",
				Type:      discovery.DeviceDisk,
				ByID:      []string{"/dev/disk/by-id/ata-root"},
				SizeBytes: 64 * 1024 * 1024 * 1024,
				Partitions: []discovery.BlockDevice{
					{Path: "/dev/nvme0n1p1", GPTLabel: disk.GPTLabelESP},
					{Path: "/dev/nvme0n1p2", GPTLabel: disk.GPTLabelRootA},
					{Path: "/dev/nvme0n1p3", GPTLabel: disk.GPTLabelRootB},
					{Path: "/dev/nvme0n1p4", GPTLabel: disk.GPTLabelState},
				},
			},
		},
		Mounts: []discovery.MountFact{
			{Source: "/dev/nvme0n1p1", Target: filepath.Join(targetRoot, "efi"), Filesystem: "vfat"},
			{Source: "/dev/nvme0n1p4", Target: filepath.Join(targetRoot, "var"), Filesystem: "ext4"},
		},
	}
}

type sequenceDiscoverySource struct {
	facts []discovery.HardwareFacts
	calls int
}

func (s *sequenceDiscoverySource) Discover(ctx context.Context) (discovery.HardwareFacts, error) {
	select {
	case <-ctx.Done():
		return discovery.HardwareFacts{}, ctx.Err()
	default:
	}
	if s.calls >= len(s.facts) {
		return discovery.HardwareFacts{}, fmt.Errorf("unexpected discovery call %d", s.calls+1)
	}
	facts := s.facts[s.calls]
	s.calls++
	return facts, nil
}

type installPayloadContents struct {
	runtime []byte
	boot    []byte
}

func writeInstallPayload(t *testing.T) (katlosimage.Payload, installPayloadContents) {
	t.Helper()
	root := t.TempDir()
	contents := installPayloadContents{
		runtime: []byte("runtime-root-payload"),
		boot:    []byte("runtime-uki-payload"),
	}
	writePayloadComponent(t, root, "components/runtime/root.squashfs", contents.runtime)
	writePayloadComponent(t, root, "components/boot/katl.efi", contents.boot)
	payload := katlosimage.Payload{
		Root: root,
		Index: katlosimage.Index{
			Version:          "2026.06.06",
			Architecture:     "x86_64",
			RuntimeInterface: "katl-runtime-1",
		},
		Runtime: payloadComponent("runtime-root", katlosimage.ComponentRuntimeRoot, "components/runtime/root.squashfs", contents.runtime),
		Boot:    payloadComponent("runtime-uki", katlosimage.ComponentRuntimeUKI, "components/boot/katl.efi", contents.boot),
	}
	return payload, contents
}

func writePayloadComponent(t *testing.T, root string, rel string, data []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir payload component: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write payload component: %v", err)
	}
}

func payloadComponent(name string, role string, path string, data []byte) katlosimage.Component {
	sum := sha256.Sum256(data)
	return katlosimage.Component{
		Name:         name,
		Role:         role,
		Path:         path,
		SizeBytes:    int64(len(data)),
		SHA256:       hex.EncodeToString(sum[:]),
		Version:      "2026.06.06",
		Architecture: "x86_64",
		Compatibility: katlosimage.Compatibility{
			RuntimeInterface: "katl-runtime-1",
		},
	}
}

type runnerRootSlot struct {
	data   []byte
	synced bool
	closed bool
}

func newRunnerRootSlot(size int) *runnerRootSlot {
	return &runnerRootSlot{data: make([]byte, size)}
}

func (s *runnerRootSlot) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *runnerRootSlot) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(s.data[off:], p)
	if n < len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func (s *runnerRootSlot) Sync() error {
	s.synced = true
	return nil
}

func (s *runnerRootSlot) Close() error {
	s.closed = true
	return nil
}

type recordingRootSlotOpener struct {
	target      *runnerRootSlot
	targetLabel string
}

func (o *recordingRootSlotOpener) OpenRootSlotDevice(_ context.Context, target disk.RootSlotTarget) (disk.RootSlotDevice, error) {
	o.targetLabel = target.GPTLabel
	return o.target, nil
}

type recordingOutputRunner struct {
	NoopCommandRunner
	outputs     map[string][]byte
	Inputs      map[string]string
	OutputCalls []string
}

type rebootCommandRunner struct {
	store          *MemoryStateStore
	calls          []CommandCall
	statusAtReboot string
	statusAtSync   string
	rebootErr      error
}

func (r *rebootCommandRunner) Run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, CommandCall{Name: name, Args: append([]string(nil), args...)})
	if len(r.store.Statuses) > 0 && name == "sync" {
		r.statusAtSync = r.store.Statuses[len(r.store.Statuses)-1].State
	}
	if name != "systemctl" {
		return nil
	}
	if len(r.store.Statuses) > 0 {
		r.statusAtReboot = r.store.Statuses[len(r.store.Statuses)-1].State
	}
	return r.rebootErr
}

func (r *recordingOutputRunner) RunInput(_ context.Context, input string, name string, args ...string) error {
	if r.Inputs == nil {
		r.Inputs = make(map[string]string)
	}
	r.Calls = append(r.Calls, CommandCall{Name: name, Args: append([]string(nil), args...)})
	r.Inputs[name] = input
	return nil
}

func (r *recordingOutputRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.OutputCalls = append(r.OutputCalls, call)
	if data, ok := r.outputs[call]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("unexpected output command %s", call)
}

func commandNames(calls []CommandCall) string {
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.Name)
	}
	return strings.Join(names, " ")
}

type failingStep struct {
	id  StepID
	err error
}

func (s failingStep) ID() StepID {
	return s.id
}

func (s failingStep) Run(context.Context, *Context) error {
	return s.err
}

type errString string

func (e errString) Error() string {
	return string(e)
}

type recordingKatlosResolver struct {
	payload katlosimage.Payload
	image   manifest.KatlosImage
	err     error
}

func (r *recordingKatlosResolver) ResolveKatlosImage(_ context.Context, image manifest.KatlosImage) (katlosimage.Payload, error) {
	r.image = image
	if r.err != nil {
		return katlosimage.Payload{}, r.err
	}
	return r.payload, nil
}

type failingStatusStore struct{}

func (failingStatusStore) SaveCheckpoint(context.Context, Checkpoint) error {
	return nil
}

func (failingStatusStore) LoadCheckpoint(context.Context) (Checkpoint, error) {
	return Checkpoint{}, os.ErrNotExist
}

func (failingStatusStore) SaveStatus(context.Context, installstatus.Record) error {
	return errString("status store failed")
}

func (failingStatusStore) LoadStatus(context.Context) (installstatus.Record, error) {
	return installstatus.Record{}, os.ErrNotExist
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

func assertSymlink(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	if got != want {
		t.Fatalf("%s link = %q, want %q", path, got, want)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("%s exists, want missing", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
}

func assertDirEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("read dir %s: %v", path, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s entries = %#v, want empty", path, entries)
	}
}

func assertDir(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %v, want %v", path, got, want)
	}
}
