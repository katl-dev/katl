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

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
	"github.com/katl-dev/katl/internal/kernelmodule"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/persistedrecord"
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

func TestFirstInstallSelectsEndpointAdvertiserOnlyForManagedVIP(t *testing.T) {
	payload := planningPayload()
	payload.EndpointAdvertiser = katlosimage.Component{
		Name:           katlosimage.EndpointAdvertiserName,
		Role:           katlosimage.ComponentEndpointAdvertiser,
		Path:           "components/sysext/endpoint-advertiser.raw",
		SHA256:         strings.Repeat("c", 64),
		Version:        "2026.06.06",
		PayloadVersion: "2026.06.06",
		Architecture:   "x86_64",
		Compatibility:  katlosimage.Compatibility{RuntimeInterface: "katl-runtime-1"},
	}
	rootPlan := disk.RootSlotWritePlan{Slot: disk.RootSlotA}
	install := &Context{RootPartitionUUID: "11111111-2222-3333-4444-555555555555"}

	external, err := firstInstallRecordFromImage(payload, rootPlan, install)
	if err != nil {
		t.Fatal(err)
	}
	if len(external.Sysexts) != 0 {
		t.Fatalf("external endpoint sysexts = %#v", external.Sysexts)
	}

	install.Manifest.Node.ControlPlaneEndpoint = &controlplaneendpoint.Config{}
	managed, err := firstInstallRecordFromImage(payload, rootPlan, install)
	if err != nil {
		t.Fatal(err)
	}
	if len(managed.Sysexts) != 1 || managed.Sysexts[0].Name != katlosimage.EndpointAdvertiserName {
		t.Fatalf("managed endpoint sysexts = %#v", managed.Sysexts)
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
		{Name: "systemd-run", Args: []string{"--unit=katl-installer-reboot", "--on-active=2s", "systemctl", "--no-block", "reboot"}},
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
	requested := false
	for _, status := range store.Statuses {
		requested = requested || status.State == installstatus.StateRebootRequested
	}
	if !requested {
		t.Fatal("reboot request was not recorded")
	}
	if got := store.Statuses[len(store.Statuses)-1]; got.State != installstatus.StateFailedAfterMutation || !strings.Contains(got.LastError, "request system reboot") {
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
	if intent.Inventory.Labels["katl.dev/zone"] != "rack-a" || len(intent.Inventory.Taints) != 1 {
		t.Fatalf("labels/taints = %#v %#v", intent.Inventory.Labels, intent.Inventory.Taints)
	}
	if intent.Kubernetes.PayloadVersion != "v1.36.1" ||
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
	if intent.Source.RequestDigest != strings.Repeat("d", 64) {
		t.Fatalf("source intent = %#v", intent.Source)
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"katlosImage", "catalogRef", "bundleSource", "bundleRef", "credentialRef"} {
		if bytes.Contains(intentJSON, []byte(`"`+field+`"`)) {
			t.Fatalf("cluster intent contains operation-only field %q: %s", field, intentJSON)
		}
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
	if got := commandNames(commands.Calls); got != "sync systemd-run" {
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

func TestRunnerUsesExplicitMediaRelease(t *testing.T) {
	path := writeManifest(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	decoded.KatlosImage.ExtensionRelease = &extensionrelease.Manifest{
		Target: extensionrelease.Target{
			Version:          decoded.KatlosImage.Version,
			Architecture:     decoded.KatlosImage.Architecture,
			Flavour:          "standard",
			RuntimeInterface: decoded.KatlosImage.RuntimeInterface,
			Kernel: kernelmodule.Target{
				Release:       "6.12.0",
				RuntimeSHA256: strings.Repeat("b", 64),
			},
		},
		Extensions: map[string]string{
			"registry.invalid/drbd9": "registry.invalid/drbd9@sha256:" + strings.Repeat("c", 64),
		},
	}
	data, err = json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	external := &recordingKatlosResolver{err: errString("media image resolved outside installer media")}
	media := &recordingKatlosResolver{}
	install := &Context{
		ManifestPath:        path,
		Commands:            &NoopCommandRunner{},
		Store:               &MemoryStateStore{},
		KatlosResolver:      external,
		MediaKatlosResolver: media,
		DefaultKatlosImage:  decoded.KatlosImage,
	}

	if err := NewRunner(Plan{loadManifestStep{}, verifyKatlosImageStep{}}, install).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if media.image.SHA256 != decoded.KatlosImage.SHA256 || !manifest.KatlosImageEmpty(external.image) {
		t.Fatal("explicit media image did not retain its installer media resolver")
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
	payload.Index.Flavour = "lts"
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
	if record.Root.Flavour != "lts" {
		t.Fatalf("planned root lost image flavour: %#v", record.Root)
	}
	if record.GenerationID != "0" || record.Root.RuntimeArtifactSHA256 != payload.Runtime.SHA256 {
		t.Fatalf("record root fields = %#v", record.Root)
	}
	if record.Root.PartitionUUID != "11111111-2222-3333-4444-555555555555" || record.Boot.UKIPath != "/efi/EFI/katl/katl-0.efi" {
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

func TestRunnerHaltIfInstalledRefusesBeforeMutation(t *testing.T) {
	store := &MemoryStateStore{}
	facts := appliedLayoutFacts("")
	facts.Mounts = nil
	install := &Context{
		ManifestPath:    writeManifest(t),
		Commands:        &NoopCommandRunner{},
		Store:           store,
		KatlosResolver:  &recordingKatlosResolver{payload: planningPayload()},
		Discovery:       discovery.StaticDiscoverySource{Facts: facts},
		HaltIfInstalled: true,
	}
	err := NewRunner(Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}, prepareDiskStep{}}, install).Run(context.Background())
	if !errors.Is(err, ErrInstalledTarget) {
		t.Fatalf("Run() error = %v, want ErrInstalledTarget", err)
	}
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest, CollectHardwareFacts, VerifyTrust}) {
		t.Fatalf("completed steps = %#v", got)
	}
	status := store.Statuses[len(store.Statuses)-1]
	if status.DestructiveMutation || status.State != installstatus.StateInstallRefused || status.CurrentStep != string(PlanInstall) || !strings.Contains(status.RetryHint, "katlctl install apply") {
		t.Fatalf("installed-target refusal = %#v", status)
	}
}

func TestPlanInstallHonorsVolumeWipe(t *testing.T) {
	file, err := os.Open(writeManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	installManifest, err := manifest.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	installManifest.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "data"}}, Filesystem: "xfs", Wipe: true,
	}}
	payload := planningPayload()
	install := &Context{
		Manifest:    installManifest,
		KatlosImage: &payload,
		HardwareFacts: discovery.HardwareFacts{BlockDevices: []discovery.BlockDevice{
			{Path: "/dev/vda", Type: discovery.DeviceDisk, ByID: []string{"/dev/disk/by-id/ata-root"}, SizeBytes: 64 << 30},
			{Path: "/dev/vdb", Type: discovery.DeviceDisk, Serial: "data", SizeBytes: 64 << 30, PartitionSignature: "gpt"},
		}},
	}
	if err := planInstall(install); err != nil {
		t.Fatalf("planInstall() error = %v", err)
	}
}

func TestRunnerCapturesRootUUIDAfterMountTarget(t *testing.T) {
	store := &MemoryStateStore{}
	payload := planningPayload()
	commands := &recordingOutputRunner{}
	install := &Context{
		ManifestPath:   writeManifest(t),
		Commands:       commands,
		Store:          store,
		KatlosResolver: &recordingKatlosResolver{payload: payload},
		Discovery:      discovery.StaticDiscoverySource{Facts: partitionedFacts()},
		GenerationID:   "2026.06.06-001",
	}

	plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}, createPartitionsStep{}, mountTargetStep{}}
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
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest, CollectHardwareFacts, VerifyTrust, PlanInstall, CreatePartitions, MountTarget}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerMountTargetRequiresBoundPartitions(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "is not bound to target disk") {
		t.Fatalf("Run() error = %v, want unbound partition failure", err)
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
	commands := &recordingOutputRunner{}
	install := &Context{
		ManifestPath:   writeManifest(t),
		TargetRoot:     t.TempDir(),
		Commands:       commands,
		Store:          store,
		KatlosResolver: &recordingKatlosResolver{payload: payload},
		Discovery:      discovery.StaticDiscoverySource{Facts: partitionedFacts()},
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
	for _, name := range []string{"wipefs", "env", "partprobe", "udevadm", "mkdir", "mount", "bootctl"} {
		if !strings.Contains(calls, name) {
			t.Fatalf("command calls %q missing %s; calls = %#v", calls, name, commands.Calls)
		}
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
	payload.Index.ExtensionRelease = &extensionrelease.Manifest{
		Target: extensionrelease.Target{
			Version:          payload.Index.Version,
			Architecture:     payload.Index.Architecture,
			Flavour:          "standard",
			RuntimeInterface: payload.Index.RuntimeInterface,
			Kernel: kernelmodule.Target{
				Release:       "6.12.1",
				RuntimeSHA256: payload.Runtime.SHA256,
			},
		},
		Extensions: map[string]string{"registry.example/drbd9": "registry.example/drbd9@sha256:" + strings.Repeat("d", 64)},
	}
	store := &MemoryStateStore{}
	targetRoot := t.TempDir()
	rootTarget := newRunnerRootSlot(len(contents.runtime) + 4096)
	commands := &recordingOutputRunner{}
	discovery := &sequenceDiscoverySource{facts: []discovery.HardwareFacts{
		planningFacts(),
		planningFacts(),
		planningFacts(),
		planningFacts(),
		partitionedFacts(),
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

	if discovery.calls != 6 {
		t.Fatalf("discovery calls = %d, want collection, planning, revalidation before wiping and partitioning, partition binding, and verification", discovery.calls)
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
	if len(install.LoaderRecord.Confexts) != 1 || install.LoaderRecord.Confexts[0].Name != generation.GeneratedConfextName {
		t.Fatalf("confext metadata = %#v", install.LoaderRecord.Confexts)
	}
	assertText(t, filepath.Join(targetRoot, "efi/EFI/katl/katl-0.efi"), string(contents.boot))
	assertMissing(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/sysext/katl-kubernetes.raw"))
	assertMissing(t, filepath.Join(targetRoot, "var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw"))
	assertText(t, filepath.Join(targetRoot, strings.TrimPrefix(EndpointAdvertiserArtifactPath, "/")), string(contents.endpoint))
	artifact, err := ReadEndpointAdvertiserArtifact(targetRoot)
	if err != nil {
		t.Fatalf("read retained endpoint advertiser artifact: %v", err)
	}
	if artifact.Extension.Name != katlosimage.EndpointAdvertiserName || artifact.Extension.SHA256 != payload.EndpointAdvertiser.SHA256 {
		t.Fatalf("retained endpoint advertiser artifact = %#v", artifact)
	}
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/identity/machine-id"), "30313233343536373839616263646566\n")
	assertMissing(t, filepath.Join(targetRoot, "etc/systemd/system/var.mount"))
	assertMissing(t, filepath.Join(targetRoot, "etc/systemd/system/efi.mount"))
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/confext/etc/katl/node.json"), `"configRef": "control-plane"`)
	assertMissing(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/confext/etc/katl/kubeadm/control-plane/config.yaml"))
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/cluster/kubeadm/control-plane/config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n")
	assertDirEmpty(t, filepath.Join(targetRoot, "etc/kubernetes"))
	assertMissing(t, filepath.Join(targetRoot, "etc/systemd/system/multi-user.target.wants/kubelet.service"))
	assertMissing(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/metadata.json"))
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/spec.json"), `"sysexts": []`)
	installed, _, err := generation.ReadGeneration(targetRoot, "0")
	if err != nil || !reflect.DeepEqual(installed.ExtensionRelease, payload.Index.ExtensionRelease) {
		t.Fatalf("installed release selection metadata = %#v, %v", installed.ExtensionRelease, err)
	}
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/spec.json"), `"loaderEntryPath": "loader/entries/katl-0.conf"`)
	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/generations/0/manifest.json"), `"hostname": "lab-node-01"`)
	selection, err := generation.ReadBootSelection(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "0" || selection.TargetBootGenerationID != "0" || selection.BootedGenerationID != "" || !selection.PendingHealthValidation {
		t.Fatalf("offline boot selection = %+v", selection)
	}

	assertContains(t, filepath.Join(targetRoot, "var/lib/katl/cluster/intent.json"), `"payloadVersion": "v1.34.8"`)
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
	for _, name := range []string{"wipefs", "env", "partprobe", "udevadm", "mkdir", "mount", "bootctl"} {
		if !strings.Contains(commandNames(commands.Calls), name) {
			t.Fatalf("command calls missing %s: %#v", name, commands.Calls)
		}
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

	if discovery.calls != 3 {
		t.Fatalf("discovery calls = %d, want collection, planning, and verification", discovery.calls)
	}
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{LoadManifest, CollectHardwareFacts, VerifyTrust, PlanInstall, VerifyTarget}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestRunnerVerifiesBootstrapStateBeforeReboot(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprintf("dirty=%t", dirty), func(t *testing.T) {
			root := t.TempDir()
			record := minimalRecord("0")
			spec := generation.SpecFromRecord(*record)
			status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStatePending, generation.HealthStateUnknown, record.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := generation.WriteGeneration(root, spec, status); err != nil {
				t.Fatal(err)
			}
			if dirty {
				path := filepath.Join(root, "var/lib/katl/kubernetes/etc-kubernetes/pki/ca.crt")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("existing Kubernetes identity"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			store := &MemoryStateStore{}
			commands := &NoopCommandRunner{}
			install := &Context{
				TargetRoot: root, LoaderRecord: record, Store: store, Commands: commands,
				Completed: []StepID{MountTarget},
			}

			err = NewRunner(Plan{verifyTargetStep{}, rebootStep{}}, install).Run(context.Background())
			persisted, readErr := installstatus.ReadFile(filepath.Join(root, "var/lib/katl/install/status.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if dirty {
				if err == nil || !strings.Contains(err.Error(), "verify initial bootstrap state: generation 0 is not clean: kubeadm PKI exists") {
					t.Fatalf("verification error = %v", err)
				}
				if len(commands.Calls) != 0 || persisted.State != installstatus.StateFailedAfterMutation || persisted.CurrentStep != string(VerifyTarget) {
					t.Fatalf("dirty target reached reboot or lost failure: commands=%+v status=%+v", commands.Calls, persisted)
				}
			} else if err != nil || persisted.State != installstatus.StateRebootRequested {
				t.Fatalf("clean target: error=%v status=%+v", err, persisted)
			}
		})
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
			planningFacts(),
			applied,
		}},
	}

	plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}, verifyTargetStep{}}
	err := NewRunner(plan, install).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "KATL_STATE partition is not mounted") {
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
		Manifest:    manifest.Manifest{Node: manifest.NodeConfig{SystemRole: "control-plane"}},
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
	assertText(t, filepath.Join(targetRoot, strings.TrimPrefix(EndpointAdvertiserArtifactPath, "/")), string(contents.endpoint))
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{InstallRootSlot, InstallBootArtifacts, InstallExtensions}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestInstallExtensionsKeepsEndpointAdvertiserOffWorkers(t *testing.T) {
	payload, _ := writeInstallPayload(t)
	targetRoot := t.TempDir()
	install := &Context{
		TargetRoot:  targetRoot,
		KatlosImage: &payload,
		Store:       &MemoryStateStore{},
		Manifest:    manifest.Manifest{Node: manifest.NodeConfig{SystemRole: "worker"}},
		LoaderRecord: &generation.Record{
			GenerationID: "0",
		},
	}
	if err := (installExtensionsStep{}).Run(context.Background(), install); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, filepath.Join(targetRoot, strings.TrimPrefix(EndpointAdvertiserArtifactPath, "/")))
	assertMissing(t, filepath.Join(targetRoot, strings.TrimPrefix(EndpointAdvertiserArtifactMetadata, "/")))
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
		DiskLayout: &disk.DiskLayoutPlan{
			TargetDiskPath: "/dev/vdb",
			PartitionUUIDs: map[string]string{disk.GPTLabelRootA: "11111111-2222-3333-4444-555555555555"},
		},
	}

	if err := NewRunner(Plan{installRootSlotStep{}}, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if want := "/dev/disk/by-partuuid/11111111-2222-3333-4444-555555555555"; opener.devicePath != want {
		t.Fatalf("opened %q, want the bound root slot %s", opener.devicePath, want)
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
	bound := DiskBinding{RequestDigest: strings.Repeat("b", 64), TargetDisk: discovery.DiskIdentity{Path: "/dev/nvme0n1", SizeBytes: 64 << 30}}
	if err := store.SaveDiskBinding(context.Background(), bound); err != nil {
		t.Fatalf("SaveDiskBinding() error = %v", err)
	}
	manifestPath := writeManifest(t)

	// The refusal must not release the binding, so repeating the changed
	// request is refused again.
	for attempt := 1; attempt <= 2; attempt++ {
		install := &Context{
			ManifestPath: manifestPath,
			StateDir:     dir,
			TargetRoot:   t.TempDir(),
			Commands:     &NoopCommandRunner{},
			Store:        NewFileStateStore(dir),
			InputMode:    installstatus.InputModeTest,
			InputSource:  "https://example.invalid/install.json",
		}

		err := NewRunner(Plan{loadManifestStep{}}, install).Run(context.Background())
		if !errors.Is(err, ErrInstallRefused) {
			t.Fatalf("attempt %d: Run() error = %v, want install refused", attempt, err)
		}
		status, err := store.LoadStatus(context.Background())
		if err != nil {
			t.Fatalf("LoadStatus() error = %v", err)
		}
		if status.State != installstatus.StateInstallRefused || !status.DestructiveMutation {
			t.Fatalf("attempt %d: status = %s destructive=%v, want %s with the earlier mutation reported", attempt, status.State, status.DestructiveMutation, installstatus.StateInstallRefused)
		}
		if status.TargetDisk == nil || *status.TargetDisk != bound.TargetDisk {
			t.Fatalf("attempt %d: status target disk = %#v, want the bound %#v", attempt, status.TargetDisk, bound.TargetDisk)
		}
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
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.01-005/confext/etc/ssh/authorized_keys/katl"), sshKey+"\n")
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.01-005/confext/etc/ssh/authorized_keys/root"), sshKey+"\n")
	assertContains(t, filepath.Join(bootRoot, "loader/entries/katl-2026.06.01-005.conf"), "systemd.machine_id="+machineID)
	assertText(t, filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.01-005/confext/etc/extension-release.d/extension-release."+generation.GeneratedConfextName), "ID=katlos\nVERSION_ID=0.1.0\nCONFEXT_LEVEL=1\n")
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
		DiskLayout: &disk.DiskLayoutPlan{VolumeMounts: []disk.VolumePlan{{
			Name:        "data",
			MountSource: "PARTUUID=data-partuuid",
			Filesystem:  "ext4",
			MountPath:   "/var/mnt/data",
		}}},
		Commands: &NoopCommandRunner{},
		Store:    store,
	}

	if err := NewRunner(Plan{installMountUnitsStep{}}, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertMissing(t, filepath.Join(targetRoot, "etc/systemd/system/var.mount"))
	assertContains(t, filepath.Join(targetRoot, "etc/systemd/system/etc-kubernetes.mount"), "Where=/etc/kubernetes")
	assertContains(t, filepath.Join(targetRoot, "etc/systemd/system/var-mnt-data.mount"), "What=PARTUUID=data-partuuid")
	assertContains(t, filepath.Join(targetRoot, "etc/systemd/system/var-mnt-data.mount"), "Where=/var/mnt/data")
	assertContains(t, filepath.Join(targetRoot, "etc/systemd/system/katl-kubeadm-ready.target"), "Requires=systemd-sysext.service systemd-confext.service containerd.service kubelet.service etc-kubernetes.mount")
	assertMissing(t, filepath.Join(targetRoot, "etc/systemd/system/multi-user.target.wants/katl-kubeadm-ready.target"))
	assertContains(t, filepath.Join(targetRoot, "etc/tmpfiles.d/katl-state.conf"), "d /var/lib/katl/kubernetes/etc-kubernetes 0755 root root -")
	assertContains(t, filepath.Join(targetRoot, "etc/tmpfiles.d/katl-state.conf"), "d /var/lib/etcd 0755 root root -")
	assertDir(t, filepath.Join(targetRoot, "etc/kubernetes"), 0o755)
	assertDir(t, filepath.Join(targetRoot, "var/mnt/data"), 0o755)
	if got := install.Completed; !reflect.DeepEqual(got, []StepID{InstallMountUnits}) {
		t.Fatalf("completed steps = %#v", got)
	}
}

func TestFormatFilesystemsBindsVolumeToDiscoveredIdentity(t *testing.T) {
	install := &Context{
		Commands: &NoopCommandRunner{}, Store: &MemoryStateStore{},
		Discovery: discovery.StaticDiscoverySource{Facts: discovery.HardwareFacts{BlockDevices: []discovery.BlockDevice{{
			Path: "/dev/vdb", Type: discovery.DeviceDisk, Partitions: []discovery.BlockDevice{{
				Path: "/dev/vdb1", Type: discovery.DevicePartition, GPTLabel: "u-data",
				PartitionUUID: "part-data", FilesystemUUID: "fs-data", FilesystemSignature: "xfs",
			}},
		}}}},
		DiskLayout: &disk.DiskLayoutPlan{VolumeMounts: []disk.VolumePlan{{
			Name: "data", DevicePath: "/dev/vdb1", MountSource: "/dev/disk/by-partlabel/u-data",
			Filesystem: "xfs", MountPath: "/var/mnt/data",
		}}},
	}
	if err := bindInstalledVolumes(context.Background(), install); err != nil {
		t.Fatalf("bindInstalledVolumes() error = %v", err)
	}
	if got := install.DiskLayout.VolumeMounts[0].MountSource; got != "PARTUUID=part-data" {
		t.Fatalf("bound mount source = %q", got)
	}
	want := []generation.VolumeBinding{{Name: "data", PartitionUUID: "part-data", FilesystemUUID: "fs-data"}}
	if !reflect.DeepEqual(install.VolumeBindings, want) {
		t.Fatalf("volume bindings = %#v, want %#v", install.VolumeBindings, want)
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
			"hostConfiguration": {
				"sets": {
					"lan": {
						"files": [
							{"path": "/etc/systemd/network/10-lan.network", "content": "[Match]\nName=enp1s0\n"}
						]
					}
				}
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
	assertText(t, filepath.Join(confextDir, "etc/extension-release.d/extension-release."+generation.GeneratedConfextName), "ID=katlos\nVERSION_ID=0.1.0\nCONFEXT_LEVEL=1\n")
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
	assertMissing(t, metadataPath)
	decoded, err := generation.ReadRecord(metadataPath)
	if err != nil {
		t.Fatal(err)
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
	if decoded.Confexts[0].ActivationPath != "/run/confexts/"+generation.GeneratedConfextName || decoded.Confexts[0].SHA256 != digest {
		t.Fatalf("confext activation/digest = %#v, digest %s", decoded.Confexts[0], digest)
	}
	if decoded.Confexts[0].Compatibility.ID != "katlos" || decoded.Confexts[0].Compatibility.ConfextLevel != 1 {
		t.Fatalf("confext compatibility = %#v", decoded.Confexts[0].Compatibility)
	}
	if decoded.Confexts[0].Name != generation.GeneratedConfextName || decoded.Confexts[0].Compatibility.VersionID != "0.1.0" {
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
			"hostConfiguration": {
				"sets": {
					"lan": {
						"files": [
							{"path": "/etc/systemd/network/10-lan.network", "content": "[Match]\nName=enp1s0\n"}
						]
					}
				}
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
				"management": ` + testManagementIdentityJSON(t, "lab-node-01") + `,
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
	data := `{"apiVersion":"install.katl.dev/v1alpha1","kind":"InstallManifest","node":{"identity":{"hostname":"lab-node-01","management":` + testManagementIdentityJSON(t, "lab-node-01") + `,"ssh":{"authorizedKeys":["` + sshKey + `"]}},"systemRole":"control-plane"},"install":{"wipeTarget":true,"targetDisk":{"byID":"/dev/disk/by-id/ata-root","minSizeMiB":32768}},"katlosImage":{"url":"https://example.invalid/katlos-install.squashfs","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sizeBytes":1073741824,"version":"2026.06.04","architecture":"x86_64","runtimeInterface":"katl-runtime-1","role":"install"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func testManagementIdentityJSON(t *testing.T, nodeName string) string {
	t.Helper()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	randomBytes := make([]byte, 4096)
	for index := range randomBytes {
		randomBytes[index] = byte(index)
	}
	random := bytes.NewReader(randomBytes)
	bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "runner-test", Now: now, Random: random})
	if err != nil {
		t.Fatal(err)
	}
	credentials, _, err := managementidentity.EnsureNode(&bundle, nodeName, now, random)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(manifest.ManagementIdentity{
		CACertificate: credentials.CACertificate, ServerCertificate: credentials.ServerCertificate, ServerPrivateKey: credentials.ServerPrivateKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
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

// partitionedFacts is the planning disk after the installer created the Katl
// partitions, with an earlier Katl install on a second disk listed first.
func partitionedFacts() discovery.HardwareFacts {
	facts := planningFacts()
	facts.BlockDevices[0].Partitions = []discovery.BlockDevice{
		{Path: "/dev/nvme0n1p1", Type: discovery.DevicePartition, GPTLabel: disk.GPTLabelESP, PartitionUUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
		{Path: "/dev/nvme0n1p2", Type: discovery.DevicePartition, GPTLabel: disk.GPTLabelRootA, PartitionUUID: "11111111-2222-3333-4444-555555555555"},
		{Path: "/dev/nvme0n1p3", Type: discovery.DevicePartition, GPTLabel: disk.GPTLabelRootB, PartitionUUID: "66666666-7777-8888-9999-000000000000"},
		{Path: "/dev/nvme0n1p4", Type: discovery.DevicePartition, GPTLabel: disk.GPTLabelState, PartitionUUID: "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"},
	}
	old := discovery.BlockDevice{Name: "sda", Path: "/dev/sda", Type: discovery.DeviceDisk, SizeBytes: 64 << 30, Partitions: []discovery.BlockDevice{
		{Path: "/dev/sda1", Type: discovery.DevicePartition, GPTLabel: disk.GPTLabelESP, PartitionUUID: "0ld00000-0000-0000-0000-00000000e5b0"},
		{Path: "/dev/sda2", Type: discovery.DevicePartition, GPTLabel: disk.GPTLabelRootA, PartitionUUID: "0ld00000-0000-0000-0000-0000000000a0"},
	}}
	facts.BlockDevices = append([]discovery.BlockDevice{old}, facts.BlockDevices...)
	return facts
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
	runtime  []byte
	boot     []byte
	endpoint []byte
}

func writeInstallPayload(t *testing.T) (katlosimage.Payload, installPayloadContents) {
	t.Helper()
	root := t.TempDir()
	contents := installPayloadContents{
		runtime:  []byte("runtime-root-payload"),
		boot:     []byte("runtime-uki-payload"),
		endpoint: []byte("endpoint-advertiser-sysext"),
	}
	writePayloadComponent(t, root, "components/runtime/root.squashfs", contents.runtime)
	writePayloadComponent(t, root, "components/boot/katl.efi", contents.boot)
	writePayloadComponent(t, root, "components/sysext/endpoint-advertiser.raw", contents.endpoint)
	payload := katlosimage.Payload{
		Root: root,
		Index: katlosimage.Index{
			Version:          "2026.06.06",
			Architecture:     "x86_64",
			RuntimeInterface: "katl-runtime-1",
		},
		Runtime:            payloadComponent("runtime-root", katlosimage.ComponentRuntimeRoot, "components/runtime/root.squashfs", contents.runtime),
		Boot:               payloadComponent("runtime-uki", katlosimage.ComponentRuntimeUKI, "components/boot/katl.efi", contents.boot),
		EndpointAdvertiser: payloadComponent(katlosimage.EndpointAdvertiserName, katlosimage.ComponentEndpointAdvertiser, "components/sysext/endpoint-advertiser.raw", contents.endpoint),
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
	target     *runnerRootSlot
	devicePath string
}

func (o *recordingRootSlotOpener) OpenRootSlotDevice(_ context.Context, devicePath string) (disk.RootSlotDevice, error) {
	o.devicePath = devicePath
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
	if name != "systemd-run" {
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

func (failingStatusStore) SaveStatus(_ context.Context, record installstatus.Record) error {
	if strings.HasPrefix(record.State, "failed") {
		return errString("status store failed")
	}
	return nil
}

func (failingStatusStore) SaveDiskBinding(context.Context, DiskBinding) error {
	return nil
}

func (failingStatusStore) LoadDiskBinding(context.Context) (DiskBinding, error) {
	return DiskBinding{}, os.ErrNotExist
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

func TestInstalledTargetPolicy(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		guard, unrelated bool
		label            string
		refuse           bool
	}{
		{name: "explicit reinstall", label: disk.GPTLabelESP},
		{name: "partial install", guard: true, label: disk.GPTLabelESP, refuse: true},
		{name: "upgraded install", guard: true, label: disk.GPTLabelState, refuse: true},
		{name: "another disk", guard: true, label: disk.GPTLabelESP, unrelated: true},
		{name: "other operating system", guard: true, label: "EFI System Partition"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store := &MemoryStateStore{}
			facts := planningFacts()
			target := &facts.BlockDevices[0]
			if scenario.unrelated {
				facts.BlockDevices = append(facts.BlockDevices, discovery.BlockDevice{Path: "/dev/vdb", Type: discovery.DeviceDisk, SizeBytes: 64 << 30})
				target = &facts.BlockDevices[len(facts.BlockDevices)-1]
			}
			target.Partitions = []discovery.BlockDevice{{Path: target.Path + "1", Type: discovery.DevicePartition, GPTLabel: scenario.label}}
			install := &Context{
				ManifestPath: writeManifest(t), Commands: &NoopCommandRunner{}, Store: store,
				KatlosResolver: &recordingKatlosResolver{payload: planningPayload()}, Discovery: discovery.StaticDiscoverySource{Facts: facts}, HaltIfInstalled: scenario.guard,
			}
			err := NewRunner(Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}}, install).Run(context.Background())
			if scenario.refuse {
				if !errors.Is(err, ErrInstalledTarget) {
					t.Fatalf("wanted refusal, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunnerRerunAfterMutationKeepsResolvedDisk(t *testing.T) {
	// The selector would match /dev/nvme0n1, but the interrupted attempt
	// resolved a different disk that has since disappeared.
	path := writeManifest(t)
	install := &Context{
		ManifestPath:   path,
		TargetRoot:     t.TempDir(),
		Commands:       &NoopCommandRunner{},
		Store:          &MemoryStateStore{Binding: &DiskBinding{RequestDigest: manifestDigest(t, path), TargetDisk: discovery.DiskIdentity{Path: "/dev/sdb", DiskSeq: 4, SizeBytes: 64 << 30}}},
		KatlosResolver: &recordingKatlosResolver{payload: planningPayload()},
		Discovery:      discovery.StaticDiscoverySource{Facts: planningFacts()},
	}
	plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}}

	err := NewRunner(plan, install).Run(context.Background())

	if !errors.Is(err, discovery.ErrTargetDiskChanged) || !strings.Contains(err.Error(), "/dev/sdb") {
		t.Fatalf("Run() error = %v, want refusal naming the previously selected /dev/sdb", err)
	}
}

func TestRunnerRevalidatesTargetDiskBeforeEachDestructiveGroup(t *testing.T) {
	replaced := planningFacts()
	replaced.BlockDevices[0].DiskSeq = 9
	for _, tt := range []struct {
		name        string
		facts       []discovery.HardwareFacts
		ran         []string
		notRan      string
		state       string
		destructive bool
	}{
		{
			name:   "disk replaced before wiping",
			facts:  []discovery.HardwareFacts{planningFacts(), planningFacts(), replaced},
			notRan: "wipefs", state: installstatus.StateInstallRefused, destructive: false,
		},
		{
			name:   "disk replaced between wiping and partitioning",
			facts:  []discovery.HardwareFacts{planningFacts(), planningFacts(), planningFacts(), replaced},
			ran:    []string{"wipefs"},
			notRan: "systemd-repart", state: installstatus.StateFailedAfterMutation, destructive: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			commands := &NoopCommandRunner{}
			store := &MemoryStateStore{}
			install := &Context{
				ManifestPath:   writeManifest(t),
				TargetRoot:     t.TempDir(),
				Commands:       commands,
				Store:          store,
				KatlosResolver: &recordingKatlosResolver{payload: planningPayload()},
				Discovery:      &sequenceDiscoverySource{facts: tt.facts},
			}
			plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}, prepareDiskStep{}, createPartitionsStep{}}

			err := NewRunner(plan, install).Run(context.Background())

			if !errors.Is(err, discovery.ErrTargetDiskChanged) {
				t.Fatalf("Run() error = %v, want ErrTargetDiskChanged", err)
			}
			ran := map[string]bool{}
			for _, call := range commands.Calls {
				ran[call.Name] = true
			}
			for _, name := range tt.ran {
				if !ran[name] {
					t.Errorf("%s did not run: %v", name, commands.Calls)
				}
			}
			if ran[tt.notRan] {
				t.Errorf("%s ran on a changed disk: %v", tt.notRan, commands.Calls)
			}
			status := store.Statuses[len(store.Statuses)-1]
			if status.State != tt.state || status.DestructiveMutation != tt.destructive {
				t.Errorf("status = %s destructive=%v, want %s destructive=%v", status.State, status.DestructiveMutation, tt.state, tt.destructive)
			}
			if status.TargetDisk == nil || status.TargetDisk.Path != "/dev/nvme0n1" {
				t.Errorf("status target disk = %#v, want the planned /dev/nvme0n1", status.TargetDisk)
			}
		})
	}
}

func TestRunnerRerunAfterMutationBindsToRetainedDisk(t *testing.T) {
	// The interrupted attempt selected /dev/sdb by its GPT GUID and replaced
	// that partition table; /dev/sda now carries the GUID the selector names.
	const guid = "6f1e3c2a-8d4b-4f5e-9a7c-1b2d3e4f5a6b"
	facts := discovery.HardwareFacts{BlockDevices: []discovery.BlockDevice{
		{Name: "sda", Path: "/dev/sda", Type: discovery.DeviceDisk, DiskSeq: 1, SizeBytes: 64 << 30, PartitionSignature: "gpt", PartitionTableUUID: guid},
		{Name: "sdb", Path: "/dev/sdb", Type: discovery.DeviceDisk, DiskSeq: 2, SizeBytes: 64 << 30},
	}}
	path := writeManifestWithTarget(t, `{"gptGUID": "`+guid+`"}`)
	install := &Context{
		ManifestPath:   path,
		TargetRoot:     t.TempDir(),
		Commands:       &NoopCommandRunner{},
		Store:          &MemoryStateStore{Binding: &DiskBinding{RequestDigest: manifestDigest(t, path), TargetDisk: discovery.DiskIdentity{Path: "/dev/sdb", DiskSeq: 2, SizeBytes: 64 << 30}}},
		KatlosResolver: &recordingKatlosResolver{payload: planningPayload()},
		Discovery:      discovery.StaticDiscoverySource{Facts: facts},
	}
	plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}}

	if err := NewRunner(plan, install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if got := install.DiskLayout.TargetDiskPath; got != "/dev/sdb" {
		t.Fatalf("rerun planned %s, want the retained /dev/sdb", got)
	}
}

// A failure before planning on a rerun must not release the disk an earlier
// attempt in the same installer boot wiped. Each attempt is a fresh process
// sharing only the state directory.
func TestRunnerStaysBoundToWipedDiskAcrossAttempts(t *testing.T) {
	stateDir := t.TempDir()
	manifestPath := writeManifestWithTarget(t, `{"name": "/dev/sda"}`)
	diskA := discovery.BlockDevice{Name: "sda", Path: "/dev/sda", Type: discovery.DeviceDisk, DiskSeq: 1, SizeBytes: 64 << 30}
	diskB := discovery.BlockDevice{Name: "sdb", Path: "/dev/sdb", Type: discovery.DeviceDisk, DiskSeq: 2, SizeBytes: 64 << 30}
	attempt := func(facts discovery.HardwareFacts, commands CommandRunner, resolver KatlosImageResolver) error {
		install := &Context{
			ManifestPath:   manifestPath,
			StateDir:       stateDir,
			TargetRoot:     t.TempDir(),
			Commands:       commands,
			Store:          NewFileStateStore(stateDir),
			KatlosResolver: resolver,
			Discovery:      discovery.StaticDiscoverySource{Facts: facts},
		}
		plan := Plan{loadManifestStep{}, collectHardwareFactsStep{}, verifyKatlosImageStep{}, planInstallStep{}, prepareDiskStep{}, createPartitionsStep{}}
		return NewRunner(plan, install).Run(context.Background())
	}
	original := discovery.HardwareFacts{BlockDevices: []discovery.BlockDevice{diskA, diskB}}

	first := &failingCommandRunner{fail: "systemd-repart"}
	if err := attempt(original, first, &recordingKatlosResolver{payload: planningPayload()}); err == nil || !first.ran("wipefs") {
		t.Fatalf("attempt 1: error = %v, wipefs ran = %v; want a failure after wiping", err, first.ran("wipefs"))
	}

	second := &failingCommandRunner{}
	if err := attempt(original, second, &recordingKatlosResolver{err: errString("image unavailable")}); err == nil || second.ran("wipefs") {
		t.Fatalf("attempt 2: error = %v, wipefs ran = %v; want a failure before planning", err, second.ran("wipefs"))
	}
	status, err := installstatus.ReadFile(filepath.Join(stateDir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if status.State != installstatus.StateFailedAfterMutation || !status.DestructiveMutation || status.TargetDisk == nil || status.TargetDisk.Path != "/dev/sda" {
		t.Fatalf("attempt 2: status = %s destructive=%v target=%#v, want %s on the wiped /dev/sda", status.State, status.DestructiveMutation, status.TargetDisk, installstatus.StateFailedAfterMutation)
	}

	// The kernel names swap, so the name selector now matches disk B.
	renamedA, renamedB := diskA, diskB
	renamedA.Name, renamedA.Path = "sdb", "/dev/sdb"
	renamedB.Name, renamedB.Path = "sda", "/dev/sda"
	third := &failingCommandRunner{}
	err = attempt(discovery.HardwareFacts{BlockDevices: []discovery.BlockDevice{renamedB, renamedA}}, third, &recordingKatlosResolver{payload: planningPayload()})
	if !errors.Is(err, discovery.ErrTargetDiskChanged) {
		t.Fatalf("attempt 3: error = %v, want ErrTargetDiskChanged", err)
	}
	if third.ran("wipefs") {
		t.Fatalf("attempt 3 wiped a disk the boot was not bound to: %v", third.calls)
	}
}

type failingCommandRunner struct {
	fail  string
	calls []CommandCall
}

func (r *failingCommandRunner) Run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, CommandCall{Name: name, Args: append([]string(nil), args...)})
	// Some commands run through env, so the failing program may be an argument.
	for _, word := range append([]string{name}, args...) {
		if r.fail != "" && word == r.fail {
			return errString(r.fail + " failed")
		}
	}
	return nil
}

func (r *failingCommandRunner) ran(name string) bool {
	for _, call := range r.calls {
		if call.Name == name {
			return true
		}
	}
	return false
}

func writeManifestWithTarget(t *testing.T, targetDisk string) string {
	t.Helper()
	path := writeManifest(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replaced := strings.Replace(string(data), `"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}`, `"targetDisk": `+targetDisk, 1)
	if replaced == string(data) {
		t.Fatal("manifest fixture has no target disk to replace")
	}
	if err := os.WriteFile(path, []byte(replaced), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// manifestDigest is the request digest an install of the manifest at path
// records when it binds a disk.
func manifestDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := manifest.DecodeWithDefaultImage(bytes.NewReader(data), manifest.KatlosImage{})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := installstatus.DigestManifest(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
