package katlosimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestResolveDirectoryAcceptsInstallImage(t *testing.T) {
	root, _ := writeImagePayload(t, func(*Index) {})

	payload, err := ResolveDirectory(context.Background(), root, expectedImage())
	if err != nil {
		t.Fatalf("ResolveDirectory() error = %v", err)
	}

	if payload.Runtime.Role != ComponentRuntimeRoot || payload.Boot.Role != ComponentRuntimeUKI || payload.Kubernetes.Name != "" {
		t.Fatalf("resolved components = %#v %#v %#v", payload.Runtime, payload.Boot, payload.Kubernetes)
	}

	createdAt := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	request, err := payload.FirstInstallRequest(FirstInstallRequest{
		GenerationID:      "2026.06.06-001",
		RootSlot:          "root-a",
		RootPartitionUUID: "11111111-2222-3333-4444-555555555555",
		UKIPath:           "/efi/EFI/Linux/katl-2026.06.06-001.efi",
		CreatedAt:         createdAt,
	})
	if err != nil {
		t.Fatalf("FirstInstallRequest() error = %v", err)
	}
	if request.RuntimeArtifactSHA256 != payload.Runtime.SHA256 {
		t.Fatalf("generation request digests = %#v, payload = %#v", request, payload)
	}
	if len(request.Sysexts) != 0 {
		t.Fatalf("generation 0 request selected sysexts = %#v", request.Sysexts)
	}
	if request.RuntimeInterface != "katl-runtime-1" || request.RuntimeArchitecture != "x86_64" {
		t.Fatalf("generation runtime fields = %#v", request)
	}
	if !request.CreatedAt.Equal(createdAt) {
		t.Fatalf("createdAt = %s, want %s", request.CreatedAt, createdAt)
	}
	if got := strings.Join(request.KernelCommandLine, " "); !strings.Contains(got, "katl.generation=2026.06.06-001") {
		t.Fatalf("kernel command line = %#v", request.KernelCommandLine)
	}
}

func TestResolveDirectoryAcceptsUpgradeImageWhenExpected(t *testing.T) {
	root, _ := writeImagePayload(t, func(index *Index) {
		index.ImageRole = RoleUpgrade
	})

	payload, err := ResolveDirectory(context.Background(), root, expectedUpgradeImage())
	if err != nil {
		t.Fatalf("ResolveDirectory() error = %v", err)
	}

	if payload.Index.ImageRole != RoleUpgrade || payload.Runtime.Role != ComponentRuntimeRoot || payload.Boot.Role != ComponentRuntimeUKI {
		t.Fatalf("upgrade payload = %#v", payload)
	}
}

func TestResolveDirectoryRejectsInvalidInstallImage(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Index)
		raw  func(Index) []byte
		want string
	}{
		{
			name: "digest mismatch",
			edit: func(index *Index) {
				index.Components[0].SHA256 = strings.Repeat("b", sha256.Size*2)
			},
			want: "digest",
		},
		{
			name: "missing runtime root",
			edit: func(index *Index) {
				index.Components = append([]Component{}, index.Components[1:]...)
			},
			want: `missing required component role "runtime-root"`,
		},
		{
			name: "missing runtime UKI",
			edit: func(index *Index) {
				index.Components = append(index.Components[:1], index.Components[2:]...)
			},
			want: `missing required component role "runtime-uki"`,
		},
		{
			name: "architecture mismatch",
			edit: func(index *Index) {
				index.Architecture = "aarch64"
			},
			want: "architecture",
		},
		{
			name: "embedded Kubernetes sysext",
			edit: func(index *Index) {
				index.Components = append(index.Components, validKubernetesComponent(index))
			},
			want: "must not include Kubernetes sysext component",
		},
		{
			name: "node scoped field",
			raw: func(index Index) []byte {
				data, err := json.Marshal(map[string]any{
					"apiVersion":       index.APIVersion,
					"kind":             index.Kind,
					"imageRole":        index.ImageRole,
					"format":           index.Format,
					"version":          index.Version,
					"buildID":          index.BuildID,
					"architecture":     index.Architecture,
					"runtimeInterface": index.RuntimeInterface,
					"createdAt":        index.CreatedAt,
					"components":       index.Components,
					"node":             map[string]string{"hostname": "node-01"},
				})
				if err != nil {
					t.Fatalf("marshal raw index: %v", err)
				}
				return data
			},
			want: `unknown field "node"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, index := writeImagePayload(t, tt.edit)
			if tt.raw != nil {
				if err := os.WriteFile(filepath.Join(root, "katlos", "image.json"), tt.raw(index), 0o600); err != nil {
					t.Fatalf("write raw index: %v", err)
				}
			}

			_, err := ResolveDirectory(context.Background(), root, expectedImage())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ResolveDirectory() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestHostUpgradePlanPreservesKubernetesAndStagesTrialBoot(t *testing.T) {
	root := t.TempDir()
	payload := upgradePayload(t, nil)
	previousSpec, previousStatus := knownGoodGeneration(t, "gen0", sha256Bytes([]byte("kubernetes sysext")), "v1.35.0")
	previousSpec, previousStatus = writePreservedGenerationAssets(t, root, previousSpec)
	previousKubernetes := previousSpec.Sysexts[0]

	plan, err := payload.HostUpgradePlan(HostUpgradeRequest{
		GenerationID:         "gen1",
		PreviousSpec:         previousSpec,
		PreviousStatus:       previousStatus,
		RootSlot:             "root-b",
		RootPartitionUUID:    "bbbbbbbb-1111-2222-3333-444444444444",
		UKIPath:              "/efi/EFI/Linux/katl-gen1.efi",
		LoaderEntryPath:      "loader/entries/katl-gen1.conf",
		OperationID:          "op-host-upgrade",
		BootCountedTrialPath: "/efi/loader/entries/katl-gen1+3.conf",
		Bootstrapped:         true,
		CreatedAt:            time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("HostUpgradePlan() error = %v", err)
	}

	if plan.Spec.PreviousGenerationID != "gen0" || plan.Spec.Root.Slot != "root-b" || plan.Spec.Root.RuntimeArtifactSHA256 != payload.Runtime.SHA256 {
		t.Fatalf("candidate spec root/previous = %#v", plan.Spec)
	}
	if plan.Spec.Boot.UKIPath != "/efi/EFI/Linux/katl-gen1.efi" || plan.Spec.Boot.LoaderEntryPath != "loader/entries/katl-gen1.conf" {
		t.Fatalf("candidate boot = %#v", plan.Spec.Boot)
	}
	if len(plan.Spec.Sysexts) != 1 {
		t.Fatalf("candidate sysexts = %#v, want one preserved sysext", plan.Spec.Sysexts)
	}
	preservedKubernetes := plan.Spec.Sysexts[0]
	if preservedKubernetes.Path != "/var/lib/katl/generations/gen1/sysext/kubernetes.raw" ||
		preservedKubernetes.SHA256 != previousKubernetes.SHA256 ||
		preservedKubernetes.PayloadVersion != previousKubernetes.PayloadVersion ||
		!reflect.DeepEqual(preservedKubernetes.Compatibility, previousKubernetes.Compatibility) {
		t.Fatalf("candidate sysext = %#v, want preserved metadata from %#v", preservedKubernetes, previousKubernetes)
	}
	if len(plan.Spec.Confexts) != 1 || plan.Spec.Confexts[0].Path != "/var/lib/katl/generations/gen1/confext" {
		t.Fatalf("candidate confexts = %#v, want rehomed preserved confext", plan.Spec.Confexts)
	}
	activation, err := generation.PlanActivation(generation.RecordFromSplit(plan.Spec, plan.Status))
	if err != nil {
		t.Fatalf("PlanActivation(candidate) error = %v", err)
	}
	if len(activation.Sysexts) != 1 || activation.Sysexts[0].SourcePath != "/var/lib/katl/generations/gen1/sysext/kubernetes.raw" {
		t.Fatalf("activation plan = %#v", activation)
	}
	if err := generation.WriteGeneration(root, previousSpec, previousStatus); err != nil {
		t.Fatalf("WriteGeneration(previous) error = %v", err)
	}
	if err := generation.WriteGeneration(root, plan.Spec, plan.Status); err != nil {
		t.Fatalf("WriteGeneration(candidate) error = %v", err)
	}
	if err := StagePreservedAssets(root, plan); err != nil {
		t.Fatalf("StagePreservedAssets() error = %v", err)
	}
	if _, err := generation.ApplyActivation(root, generation.RecordFromSplit(plan.Spec, plan.Status)); err != nil {
		t.Fatalf("ApplyActivation(candidate) error = %v", err)
	}
	if plan.Status.CommitState != generation.CommitStateCandidate || plan.Status.BootState != generation.BootStatePending || plan.Status.HealthState != generation.HealthStateUnknown {
		t.Fatalf("candidate status = %#v", plan.Status)
	}
	if plan.BootSelection.DefaultGenerationID != "gen0" ||
		plan.BootSelection.TrialGenerationID != "gen1" ||
		plan.BootSelection.PreviousKnownGoodGenerationID != "gen0" ||
		plan.BootSelection.TrialBootEntry != "loader/entries/katl-gen1.conf" ||
		plan.BootSelection.PreviousKnownGoodBootEntry != "loader/entries/katl-gen0.conf" ||
		!plan.BootSelection.PendingHealthValidation ||
		plan.BootSelection.PersistentDefaultPromotion != generation.DefaultPromotionPending ||
		plan.BootSelection.BootCountedTrialPath != "/efi/loader/entries/katl-gen1+3.conf" {
		t.Fatalf("boot selection = %#v", plan.BootSelection)
	}
}

func TestHostUpgradePlanRejectsIncompatibleImage(t *testing.T) {
	payload := upgradePayload(t, nil)
	payload.Index.Architecture = "aarch64"
	previousSpec, previousStatus := knownGoodGeneration(t, "gen0", strings.Repeat("b", sha256.Size*2), "v1.35.0")

	_, err := payload.HostUpgradePlan(validHostUpgradeRequest(previousSpec, previousStatus))
	if err == nil || !strings.Contains(err.Error(), "architecture") {
		t.Fatalf("HostUpgradePlan() error = %v, want architecture mismatch", err)
	}
}

func TestHostUpgradePlanRejectsRuntimeInterfaceChange(t *testing.T) {
	payload := upgradePayload(t, nil)
	payload.Index.RuntimeInterface = "katl-runtime-2"
	previousSpec, previousStatus := knownGoodGeneration(t, "gen0", strings.Repeat("b", 64), "v1.36.0")
	_, err := payload.HostUpgradePlan(validHostUpgradeRequest(previousSpec, previousStatus))
	if err == nil || !strings.Contains(err.Error(), "runtime interface") {
		t.Fatalf("HostUpgradePlan() error = %v, want runtime interface mismatch", err)
	}
}

func TestHostUpgradePlanPreservesKubernetesOnBootstrappedNode(t *testing.T) {
	payload := upgradePayload(t, nil)
	previousSpec, previousStatus := knownGoodGeneration(t, "gen0", strings.Repeat("b", sha256.Size*2), "v1.35.0")
	request := validHostUpgradeRequest(previousSpec, previousStatus)
	request.Bootstrapped = true

	plan, err := payload.HostUpgradePlan(request)
	if err != nil {
		t.Fatalf("HostUpgradePlan() error = %v", err)
	}
	if len(plan.Spec.Sysexts) != 1 || plan.Spec.Sysexts[0].PayloadVersion != "v1.35.0" {
		t.Fatalf("candidate sysexts = %#v, want preserved Kubernetes sysext", plan.Spec.Sysexts)
	}
}

func TestHostUpgradePlanRejectsNonUpgradePayload(t *testing.T) {
	payload := upgradePayload(t, nil)
	payload.Index.ImageRole = RoleInstall
	previousSpec, previousStatus := knownGoodGeneration(t, "gen0", strings.Repeat("b", sha256.Size*2), "v1.35.0")

	_, err := payload.HostUpgradePlan(validHostUpgradeRequest(previousSpec, previousStatus))
	if err == nil || !strings.Contains(err.Error(), "role must be upgrade") {
		t.Fatalf("HostUpgradePlan() error = %v, want upgrade role refusal", err)
	}
}

func TestHostUpgradePlanRequiresKnownGoodPreviousGeneration(t *testing.T) {
	payload := upgradePayload(t, nil)
	previousSpec, previousStatus := knownGoodGeneration(t, "gen0", strings.Repeat("b", sha256.Size*2), "v1.35.0")
	previousStatus.BootState = generation.BootStatePending
	request := validHostUpgradeRequest(previousSpec, previousStatus)

	_, err := payload.HostUpgradePlan(request)
	if err == nil || !strings.Contains(err.Error(), "not known-good") {
		t.Fatalf("HostUpgradePlan() error = %v, want known-good refusal", err)
	}
}

func TestHostUpgradeRollbackMetadataRestoresPreviousKnownGood(t *testing.T) {
	root := t.TempDir()
	payload := upgradePayload(t, nil)
	previousSpec, previousStatus := knownGoodGeneration(t, "gen0", sha256Bytes([]byte("kubernetes sysext")), "v1.35.0")
	previousSpec, previousStatus = writePreservedGenerationAssets(t, root, previousSpec)
	plan, err := payload.HostUpgradePlan(HostUpgradeRequest{
		GenerationID:      "gen1",
		PreviousSpec:      previousSpec,
		PreviousStatus:    previousStatus,
		RootSlot:          "root-b",
		RootPartitionUUID: "bbbbbbbb-1111-2222-3333-444444444444",
		UKIPath:           "/efi/EFI/Linux/katl-gen1.efi",
		LoaderEntryPath:   "loader/entries/katl-gen1.conf",
		OperationID:       "op-host-upgrade",
		Bootstrapped:      true,
		CreatedAt:         time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("HostUpgradePlan() error = %v", err)
	}
	if err := generation.WriteGeneration(root, previousSpec, previousStatus); err != nil {
		t.Fatalf("WriteGeneration(previous) error = %v", err)
	}
	if err := generation.WriteGeneration(root, plan.Spec, plan.Status); err != nil {
		t.Fatalf("WriteGeneration(candidate) error = %v", err)
	}
	booted := plan.BootSelection
	booted.BootedGenerationID = "gen1"
	booted.BootedBootEntry = "loader/entries/katl-gen1.conf"
	if err := generation.WriteBootSelection(root, booted); err != nil {
		t.Fatalf("WriteBootSelection() error = %v", err)
	}

	result, err := generation.RecordBootHealth(generation.BootHealthRequest{
		Root:         root,
		GenerationID: "gen1",
		CommandLine:  "root=PARTUUID=bbbbbbbb-1111-2222-3333-444444444444 katl.generation=gen1",
		Result:       generation.BootHealthFailure,
		Reason:       "host upgrade trial failed",
		Now:          time.Date(2026, 6, 17, 12, 10, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RecordBootHealth(failure) error = %v", err)
	}
	if !result.Failed || result.DefaultGeneration != "gen0" || result.RecoveryRequired {
		t.Fatalf("boot health result = %#v, want rollback to gen0", result)
	}
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if selection.DefaultGenerationID != "gen0" ||
		selection.BootedGenerationID != "gen0" ||
		selection.FailedBootGenerationID != "gen1" ||
		selection.TrialGenerationID != "" ||
		selection.PendingHealthValidation ||
		selection.RecoveryRequired {
		t.Fatalf("rollback selection = %#v", selection)
	}
	_, previousAfter, err := generation.ReadGeneration(root, "gen0")
	if err != nil {
		t.Fatalf("ReadGeneration(gen0) error = %v", err)
	}
	if previousAfter.CommitState != generation.CommitStateCommitted || previousAfter.BootState != generation.BootStateGood || previousAfter.HealthState != generation.HealthStateHealthy {
		t.Fatalf("previous status = %#v, want still known-good", previousAfter)
	}
}

func TestLocalResolverAcceptsDirectoryRef(t *testing.T) {
	mediaRoot := t.TempDir()
	imageRoot := filepath.Join(mediaRoot, "payloads", "katlos-install.squashfs")
	index := writeImagePayloadAt(t, imageRoot, func(*Index) {})
	expected := expectedImage()

	payload, err := (LocalResolver{MediaRoot: mediaRoot}).ResolveKatlosImage(context.Background(), expected)
	if err != nil {
		t.Fatalf("ResolveKatlosImage() error = %v", err)
	}
	if payload.Root != imageRoot || payload.Index.BuildID != index.BuildID {
		t.Fatalf("payload = %#v, index = %#v", payload, index)
	}
}

func TestLocalResolverMountsFileRef(t *testing.T) {
	mediaRoot := t.TempDir()
	imagePath := filepath.Join(mediaRoot, "payloads", "katlos-install.squashfs")
	imageBytes := []byte("squashfs image bytes")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		t.Fatalf("mkdir image dir: %v", err)
	}
	if err := os.WriteFile(imagePath, imageBytes, 0o600); err != nil {
		t.Fatalf("write image file: %v", err)
	}
	sum := sha256.Sum256(imageBytes)
	expected := expectedImage()
	expected.SHA256 = ""
	expected.SizeBytes = 0
	mounter := &fixtureMountRunner{populate: func(root string) {
		writeImagePayloadAt(t, root, func(*Index) {})
	}}

	payload, err := (LocalResolver{
		MediaRoot: mediaRoot,
		WorkDir:   filepath.Join(t.TempDir(), "mounts"),
		Commands:  mounter,
	}).ResolveKatlosImage(context.Background(), expected)
	if err != nil {
		t.Fatalf("ResolveKatlosImage() error = %v", err)
	}

	wantCall := []string{"mount", "-o", "ro,loop", imagePath}
	if len(mounter.calls) != 1 || !reflect.DeepEqual(mounter.calls[0][:4], wantCall) {
		t.Fatalf("mount calls = %#v, want prefix %#v", mounter.calls, wantCall)
	}
	if payload.Root != mounter.calls[0][4] {
		t.Fatalf("payload root = %q, mountpoint = %q", payload.Root, mounter.calls[0][4])
	}
	if payload.ImageSHA256 != hex.EncodeToString(sum[:]) || payload.ImageSizeBytes != uint64(len(imageBytes)) {
		t.Fatalf("derived image identity = %s/%d", payload.ImageSHA256, payload.ImageSizeBytes)
	}
}

func TestLocalResolverRejectsBadTopLevelDigest(t *testing.T) {
	mediaRoot := t.TempDir()
	imagePath := filepath.Join(mediaRoot, "payloads", "katlos-install.squashfs")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		t.Fatalf("mkdir image dir: %v", err)
	}
	if err := os.WriteFile(imagePath, []byte("bad image"), 0o600); err != nil {
		t.Fatalf("write image file: %v", err)
	}
	expected := expectedImage()
	expected.SizeBytes = uint64(len("bad image"))

	_, err := (LocalResolver{
		MediaRoot: mediaRoot,
		WorkDir:   t.TempDir(),
		Commands:  &fixtureMountRunner{},
	}).ResolveKatlosImage(context.Background(), expected)
	if err == nil || !strings.Contains(err.Error(), "does not match manifest") {
		t.Fatalf("ResolveKatlosImage() error = %v, want digest mismatch", err)
	}
}

func TestRemoteResolverDownloadsAndMountsURL(t *testing.T) {
	imageBytes := []byte("remote squashfs image")
	sum := sha256.Sum256(imageBytes)
	expected := expectedImage()
	expected.LocalRef = ""
	expected.URL = "https://artifacts.example.invalid/katlos-install.squashfs"
	expected.SHA256 = ""
	expected.SizeBytes = 0
	mounter := &fixtureMountRunner{populate: func(root string) {
		writeImagePayloadAt(t, root, func(*Index) {})
	}}
	client := &fixtureHTTPClient{response: &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader(imageBytes)),
	}}

	payload, err := (RemoteResolver{
		WorkDir:  filepath.Join(t.TempDir(), "work"),
		Commands: mounter,
		Client:   client,
	}).ResolveKatlosImage(context.Background(), expected)
	if err != nil {
		t.Fatalf("ResolveKatlosImage() error = %v", err)
	}

	if client.requestURL != expected.URL {
		t.Fatalf("request URL = %q, want %q", client.requestURL, expected.URL)
	}
	if len(mounter.calls) != 1 || mounter.calls[0][0] != "mount" {
		t.Fatalf("mount calls = %#v", mounter.calls)
	}
	if payload.Root != mounter.calls[0][4] {
		t.Fatalf("payload root = %q, mountpoint = %q", payload.Root, mounter.calls[0][4])
	}
	if payload.ImageSHA256 != hex.EncodeToString(sum[:]) || payload.ImageSizeBytes != uint64(len(imageBytes)) {
		t.Fatalf("derived image identity = %s/%d", payload.ImageSHA256, payload.ImageSizeBytes)
	}
}

func TestRemoteResolverRejectsBadTopLevelDigest(t *testing.T) {
	expected := expectedImage()
	expected.LocalRef = ""
	expected.URL = "https://artifacts.example.invalid/katlos-install.squashfs"
	expected.SizeBytes = uint64(len("remote image"))

	_, err := (RemoteResolver{
		WorkDir:  t.TempDir(),
		Commands: &fixtureMountRunner{},
		Client: &fixtureHTTPClient{response: &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader("remote image")),
		}},
	}).ResolveKatlosImage(context.Background(), expected)
	if err == nil || !strings.Contains(err.Error(), "does not match manifest") {
		t.Fatalf("ResolveKatlosImage() error = %v, want digest mismatch", err)
	}
}

func TestResolverSelectsLocalOrRemoteRef(t *testing.T) {
	mediaRoot := t.TempDir()
	imageRoot := filepath.Join(mediaRoot, "payloads", "katlos-install.squashfs")
	writeImagePayloadAt(t, imageRoot, func(*Index) {})
	if _, err := (Resolver{MediaRoot: mediaRoot}).ResolveKatlosImage(context.Background(), expectedImage()); err != nil {
		t.Fatalf("local ResolveKatlosImage() error = %v", err)
	}

	imageBytes := []byte("remote image")
	sum := sha256.Sum256(imageBytes)
	expected := expectedImage()
	expected.LocalRef = ""
	expected.URL = "https://artifacts.example.invalid/katlos-install.squashfs"
	expected.SHA256 = hex.EncodeToString(sum[:])
	expected.SizeBytes = uint64(len(imageBytes))
	mounter := &fixtureMountRunner{populate: func(root string) {
		writeImagePayloadAt(t, root, func(*Index) {})
	}}
	if _, err := (Resolver{
		WorkDir:  t.TempDir(),
		Commands: mounter,
		Client: &fixtureHTTPClient{response: &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(bytes.NewReader(imageBytes)),
		}},
	}).ResolveKatlosImage(context.Background(), expected); err != nil {
		t.Fatalf("remote ResolveKatlosImage() error = %v", err)
	}
}

func writeImagePayload(t *testing.T, edit func(*Index)) (string, Index) {
	t.Helper()
	root := t.TempDir()
	index := writeImagePayloadAt(t, root, edit)
	return root, index
}

func writeImagePayloadAt(t *testing.T, root string, edit func(*Index)) Index {
	t.Helper()
	files := map[string][]byte{
		"components/runtime/root.squashfs": []byte("runtime root"),
		"components/boot/katl.efi":         []byte("runtime uki"),
		"components/sysext/kubernetes.raw": []byte("kubernetes sysext"),
	}
	digests := make(map[string]string, len(files))
	sizes := make(map[string]int64, len(files))
	for rel, data := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir component: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write component: %v", err)
		}
		sum := sha256.Sum256(data)
		digests[rel] = hex.EncodeToString(sum[:])
		sizes[rel] = int64(len(data))
	}

	index := Index{
		APIVersion:       APIVersion,
		Kind:             Kind,
		ImageRole:        RoleInstall,
		Format:           FormatSquashFS,
		Version:          "2026.06.06",
		BuildID:          "test-build",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		CreatedAt:        "2026-06-06T12:00:00Z",
		Components: []Component{
			{
				Name:         "runtime-root",
				Role:         ComponentRuntimeRoot,
				Path:         "components/runtime/root.squashfs",
				Format:       "squashfs",
				SizeBytes:    sizes["components/runtime/root.squashfs"],
				SHA256:       digests["components/runtime/root.squashfs"],
				Version:      "2026.06.06",
				Architecture: "x86_64",
				Compatibility: Compatibility{
					RuntimeInterface: "katl-runtime-1",
				},
				InstallTarget: InstallTarget{
					Kind:         "root-slot",
					Filesystem:   "squashfs",
					MinSizeBytes: sizes["components/runtime/root.squashfs"],
				},
			},
			{
				Name:         "runtime-uki",
				Role:         ComponentRuntimeUKI,
				Path:         "components/boot/katl.efi",
				Format:       "uki",
				SizeBytes:    sizes["components/boot/katl.efi"],
				SHA256:       digests["components/boot/katl.efi"],
				Version:      "2026.06.06",
				Architecture: "x86_64",
				Compatibility: Compatibility{
					RuntimeInterface: "katl-runtime-1",
					RuntimeRoot: RuntimeRoot{
						Interface:      "katl-runtime-1",
						ArtifactPath:   "components/runtime/root.squashfs",
						ArtifactSHA256: digests["components/runtime/root.squashfs"],
					},
					KernelCommandLine: []string{"katl.generation=2026.06.06-001"},
				},
				InstallTarget: InstallTarget{
					Kind:     "esp-or-xbootldr",
					Filename: "katl.efi",
				},
			},
		},
	}
	if edit != nil {
		edit(&index)
	}
	if err := os.MkdirAll(filepath.Join(root, "katlos"), 0o755); err != nil {
		t.Fatalf("mkdir katlos: %v", err)
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "katlos", "image.json"), data, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	return index
}

func validKubernetesComponent(index *Index) Component {
	sum := sha256.Sum256([]byte("kubernetes sysext"))
	return Component{
		Name:           "kubernetes",
		Role:           ComponentKubernetes,
		Path:           "components/sysext/kubernetes.raw",
		Format:         "raw",
		SizeBytes:      int64(len("kubernetes sysext")),
		SHA256:         hex.EncodeToString(sum[:]),
		Version:        "v1.34.8",
		PayloadVersion: "v1.34.8",
		Architecture:   index.Architecture,
		Compatibility: Compatibility{
			RuntimeInterface: index.RuntimeInterface,
			RuntimeRoot: RuntimeRoot{
				Interface:      index.RuntimeInterface,
				ArtifactPath:   index.Components[0].Path,
				ArtifactSHA256: index.Components[0].SHA256,
			},
		},
		InstallTarget: InstallTarget{
			Kind: "systemd-sysext",
			Name: "kubernetes.raw",
		},
	}
}

func expectedImage() manifest.KatlosImage {
	return manifest.KatlosImage{
		LocalRef:         "payloads/katlos-install.squashfs",
		SHA256:           strings.Repeat("a", sha256.Size*2),
		SizeBytes:        1024,
		Version:          "2026.06.06",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	}
}

func expectedUpgradeImage() manifest.KatlosImage {
	expected := expectedImage()
	expected.Role = RoleUpgrade
	return expected
}

func upgradePayload(t *testing.T, edit func(*Index)) Payload {
	t.Helper()
	root, _ := writeImagePayload(t, func(index *Index) {
		index.ImageRole = RoleUpgrade
		index.Version = "2026.06.17"
		index.Components[0].Version = "2026.06.17"
		index.Components[1].Version = "2026.06.17"
		index.Components[1].Compatibility.KernelCommandLine = []string{"quiet"}
		if edit != nil {
			edit(index)
		}
	})
	expected := expectedUpgradeImage()
	expected.Version = "2026.06.17"
	payload, err := ResolveDirectory(context.Background(), root, expected)
	if err != nil {
		t.Fatalf("ResolveDirectory(upgrade) error = %v", err)
	}
	return payload
}

func knownGoodGeneration(t *testing.T, id string, kubernetesSHA string, kubernetesPayloadVersion string) (generation.GenerationSpec, generation.GenerationStatus) {
	t.Helper()
	spec := generation.GenerationSpec{
		APIVersion:     generation.APIVersion,
		Kind:           generation.SpecKind,
		GenerationID:   id,
		RuntimeVersion: "2026.06.06",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "aaaaaaaa-1111-2222-3333-444444444444",
			RuntimeVersion:        "2026.06.06",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: strings.Repeat("a", sha256.Size*2),
		},
		Boot: generation.BootSelection{
			UKIPath:         "/efi/EFI/Linux/katl-" + id + ".efi",
			LoaderEntryPath: "loader/entries/katl-" + id + ".conf",
		},
		Sysexts: []generation.ExtensionRef{
			{
				Name:            "kubernetes",
				Path:            "/var/lib/katl/generations/" + id + "/sysext/kubernetes.raw",
				ActivationPath:  "/run/extensions/kubernetes.raw",
				SHA256:          kubernetesSHA,
				ArtifactVersion: "k8s-" + kubernetesPayloadVersion,
				PayloadVersion:  kubernetesPayloadVersion,
				Architecture:    "x86_64",
				Compatibility: generation.ExtensionCompatibility{
					RuntimeInterfaces: []string{"katl-runtime-1"},
				},
			},
		},
		Confexts: []generation.GeneratedConfext{
			{
				Name:           "katl-node",
				Path:           "/var/lib/katl/generations/" + id + "/confext",
				ActivationPath: "/run/confexts/katl-node",
				SHA256:         strings.Repeat("c", sha256.Size*2),
				Compatibility: generation.ConfextCompatibility{
					ID:           "katl",
					VersionID:    "2026.06.06",
					ConfextLevel: 1,
				},
			},
		},
		KernelCommandLine: []string{"quiet"},
		CreatedAt:         time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC),
	}
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, spec.CreatedAt)
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}
	return spec, status
}

func writePreservedGenerationAssets(t *testing.T, root string, spec generation.GenerationSpec) (generation.GenerationSpec, generation.GenerationStatus) {
	t.Helper()
	sysextPath := filepath.Join(root, strings.TrimPrefix(spec.Sysexts[0].Path, "/"))
	if err := os.MkdirAll(filepath.Dir(sysextPath), 0o755); err != nil {
		t.Fatalf("mkdir sysext: %v", err)
	}
	if err := os.WriteFile(sysextPath, []byte("kubernetes sysext"), 0o600); err != nil {
		t.Fatalf("write sysext: %v", err)
	}
	confextPath := filepath.Join(root, strings.TrimPrefix(spec.Confexts[0].Path, "/"))
	if err := os.MkdirAll(filepath.Join(confextPath, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir confext: %v", err)
	}
	if err := os.WriteFile(filepath.Join(confextPath, "etc", "katl.conf"), []byte("node config\n"), 0o600); err != nil {
		t.Fatalf("write confext: %v", err)
	}
	digest, err := generation.DigestDirectory(confextPath)
	if err != nil {
		t.Fatalf("DigestDirectory(confext) error = %v", err)
	}
	spec.Confexts[0].SHA256 = digest
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, spec.CreatedAt)
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}
	return spec, status
}

func validHostUpgradeRequest(previousSpec generation.GenerationSpec, previousStatus generation.GenerationStatus) HostUpgradeRequest {
	return HostUpgradeRequest{
		GenerationID:      "gen1",
		PreviousSpec:      previousSpec,
		PreviousStatus:    previousStatus,
		RootSlot:          "root-b",
		RootPartitionUUID: "bbbbbbbb-1111-2222-3333-444444444444",
		UKIPath:           "/efi/EFI/Linux/katl-gen1.efi",
		LoaderEntryPath:   "loader/entries/katl-gen1.conf",
		OperationID:       "op-host-upgrade",
		Bootstrapped:      true,
		CreatedAt:         time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC),
	}
}

type fixtureMountRunner struct {
	calls    [][]string
	populate func(root string)
}

func (r *fixtureMountRunner) Run(_ context.Context, name string, args ...string) error {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)
	if name != "mount" || len(args) != 4 {
		return nil
	}
	if r.populate != nil {
		r.populate(args[3])
	}
	return nil
}

type fixtureHTTPClient struct {
	response   *http.Response
	err        error
	requestURL string
}

func (c *fixtureHTTPClient) Do(request *http.Request) (*http.Response, error) {
	c.requestURL = request.URL.String()
	return c.response, c.err
}
