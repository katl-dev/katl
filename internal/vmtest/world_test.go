package vmtest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeWorldValidManifest(t *testing.T) {
	world, err := DecodeWorld(strings.NewReader(validWorldJSON(t)))
	if err != nil {
		t.Fatalf("DecodeWorld() error = %v", err)
	}
	if world.APIVersion != WorldAPIVersion || world.Kind != WorldKind {
		t.Fatalf("world envelope = %#v", world)
	}
	if world.Libvirt.URI != "qemu:///system" || world.Network.Backend != NetworkLibvirt || world.Network.Gateway != "10.77.0.1" {
		t.Fatalf("network = %#v", world.Network)
	}
	if world.Capabilities["libvirt"] != WorldStatusPassed {
		t.Fatalf("libvirt capability = %q", world.Capabilities["libvirt"])
	}
}

func TestWorldManifestJSONShape(t *testing.T) {
	got, err := json.MarshalIndent(validWorld(), "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	want := `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "VMTestWorld",
  "runID": "20260606T120000Z-abc123",
  "runDir": "/tmp/katl-vmtest/20260606T120000Z-abc123",
  "cacheDir": "/repo/_build/vmtest",
  "artifactDir": "/tmp/katl-vmtest/20260606T120000Z-abc123/artifacts",
  "scenarioDir": "/tmp/katl-vmtest/20260606T120000Z-abc123/scenarios",
  "libvirt": {
    "uri": "qemu:///system",
    "network": "default",
    "storagePool": "default",
    "storagePath": "/var/lib/libvirt/images",
    "domainPrefix": "katl-20260606T120000Z-abc123"
  },
  "network": {
    "backend": "libvirt",
    "name": "default",
    "cidr": "10.77.0.0/24",
    "gateway": "10.77.0.1",
    "leaseFile": "/tmp/katl-vmtest/20260606T120000Z-abc123/network/leases.json"
  },
  "capabilities": {
    "image-tool": "passed",
    "kvm": "passed",
    "libvirt": "passed",
    "libvirt-network": "passed",
    "libvirt-storage-pool": "passed",
    "ovmf": "passed",
    "vsock": "passed"
  }
}`
	if string(got) != want {
		t.Fatalf("manifest JSON:\n%s", got)
	}
}

func TestDecodeWorldRejectsRequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*World)
		want   string
	}{
		{
			name:   "run id",
			mutate: func(world *World) { world.RunID = "" },
			want:   "runID is required",
		},
		{
			name:   "run dir",
			mutate: func(world *World) { world.RunDir = "" },
			want:   "runDir is required",
		},
		{
			name:   "cache dir",
			mutate: func(world *World) { world.CacheDir = "" },
			want:   "cacheDir is required",
		},
		{
			name:   "artifact dir",
			mutate: func(world *World) { world.ArtifactDir = "" },
			want:   "artifactDir is required",
		},
		{
			name:   "scenario dir",
			mutate: func(world *World) { world.ScenarioDir = "" },
			want:   "scenarioDir is required",
		},
		{
			name:   "backend",
			mutate: func(world *World) { world.Network.Backend = "" },
			want:   "network.backend is required",
		},
		{
			name:   "libvirt uri",
			mutate: func(world *World) { world.Libvirt.URI = "" },
			want:   "libvirt.uri is required",
		},
		{
			name:   "libvirt network",
			mutate: func(world *World) { world.Libvirt.Network = "" },
			want:   "libvirt.network is required",
		},
		{
			name:   "libvirt storage pool",
			mutate: func(world *World) { world.Libvirt.StoragePool = "" },
			want:   "libvirt.storagePool is required",
		},
		{
			name:   "libvirt storage path",
			mutate: func(world *World) { world.Libvirt.StoragePath = "" },
			want:   "libvirt.storagePath is required",
		},
		{
			name:   "libvirt domain prefix",
			mutate: func(world *World) { world.Libvirt.DomainPrefix = "" },
			want:   "libvirt.domainPrefix is required",
		},
		{
			name:   "network name",
			mutate: func(world *World) { world.Network.Name = "" },
			want:   "network.name is required",
		},
		{
			name:   "cidr",
			mutate: func(world *World) { world.Network.CIDR = "" },
			want:   "network.cidr",
		},
		{
			name:   "gateway",
			mutate: func(world *World) { world.Network.Gateway = "" },
			want:   "network.gateway",
		},
		{
			name:   "lease file",
			mutate: func(world *World) { world.Network.LeaseFile = "" },
			want:   "leaseFile is required",
		},
		{
			name:   "capabilities",
			mutate: func(world *World) { world.Capabilities = nil },
			want:   "capabilities are required",
		},
		{
			name:   "capability name",
			mutate: func(world *World) { world.Capabilities[""] = WorldStatusPassed },
			want:   "capability name is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world := validWorld()
			tt.mutate(&world)
			err := decodeWorldValue(world)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeWorld() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeWorldRejectsUnsupportedEnvelope(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*World)
		want   string
	}{
		{
			name:   "api version",
			mutate: func(world *World) { world.APIVersion = "katl.dev/v9" },
			want:   "unsupported apiVersion",
		},
		{
			name:   "kind",
			mutate: func(world *World) { world.Kind = "Other" },
			want:   "unsupported kind",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world := validWorld()
			tt.mutate(&world)
			err := decodeWorldValue(world)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeWorld() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeWorldRejectsInvalidNetwork(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*World)
		want   string
	}{
		{
			name:   "unsupported backend",
			mutate: func(world *World) { world.Network.Backend = "slirp" },
			want:   "unsupported network.backend",
		},
		{
			name:   "invalid cidr",
			mutate: func(world *World) { world.Network.CIDR = "10.77.0.0" },
			want:   "network.cidr",
		},
		{
			name:   "invalid gateway",
			mutate: func(world *World) { world.Network.Gateway = "not-an-ip" },
			want:   "network.gateway",
		},
		{
			name:   "gateway outside cidr",
			mutate: func(world *World) { world.Network.Gateway = "10.78.0.1" },
			want:   "outside network.cidr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world := validWorld()
			tt.mutate(&world)
			err := decodeWorldValue(world)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeWorld() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeWorldRejectsRelativePaths(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*World)
		want   string
	}{
		{
			name:   "run dir",
			mutate: func(world *World) { world.RunDir = "_build/vmtest/run" },
			want:   "runDir must be an absolute path",
		},
		{
			name:   "cache dir",
			mutate: func(world *World) { world.CacheDir = "_build/vmtest" },
			want:   "cacheDir must be an absolute path",
		},
		{
			name:   "artifact dir",
			mutate: func(world *World) { world.ArtifactDir = "artifacts" },
			want:   "artifactDir must be an absolute path",
		},
		{
			name:   "scenario dir",
			mutate: func(world *World) { world.ScenarioDir = "scenarios" },
			want:   "scenarioDir must be an absolute path",
		},
		{
			name:   "lease file",
			mutate: func(world *World) { world.Network.LeaseFile = "network/leases.json" },
			want:   "leaseFile must be an absolute path",
		},
		{
			name:   "libvirt storage path",
			mutate: func(world *World) { world.Libvirt.StoragePath = "libvirt/images" },
			want:   "libvirt.storagePath must be an absolute path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world := validWorld()
			tt.mutate(&world)
			err := decodeWorldValue(world)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeWorld() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeWorldCapabilityStatuses(t *testing.T) {
	world := validWorld()
	world.Capabilities = map[string]WorldStatus{
		"libvirt": WorldStatusPassed,
		"fixture": WorldStatusFailed,
		"mkosi":   WorldStatusSetupFailed,
		"kvm":     WorldStatusHostSkipped,
		"suite":   WorldStatusDisabled,
	}
	if err := decodeWorldValue(world); err != nil {
		t.Fatalf("DecodeWorld() error = %v", err)
	}

	world.Capabilities["bad"] = "unknown"
	err := decodeWorldValue(world)
	if err == nil || !strings.Contains(err.Error(), `unsupported capability status "unknown"`) {
		t.Fatalf("DecodeWorld() error = %v, want invalid status", err)
	}
}

func TestDecodeWorldRunIndex(t *testing.T) {
	world := validWorld()
	world.RunIndex = filepath.Join(world.RunDir, "run.json")
	world.ResourceManifest = filepath.Join(world.RunDir, "resource-test-manifest.json")
	world.ResourceDigest = strings.Repeat("a", 64)
	world.PackageLock = "/repo/mkosi.profiles/resource-package-lock.json"
	world.PackageLockDigest = strings.Repeat("b", 64)
	decodedErr := decodeWorldValue(world)
	if decodedErr != nil {
		t.Fatalf("DecodeWorld() error = %v", decodedErr)
	}

	world.RunIndex = "relative/run.json"
	err := decodeWorldValue(world)
	if err == nil || !strings.Contains(err.Error(), "runIndex must be an absolute path") {
		t.Fatalf("DecodeWorld() error = %v, want runIndex path rejection", err)
	}

	world = validWorld()
	world.ResourceManifest = "relative/resource-test-manifest.json"
	err = decodeWorldValue(world)
	if err == nil || !strings.Contains(err.Error(), "resourceManifest must be an absolute path") {
		t.Fatalf("DecodeWorld() error = %v, want resourceManifest path rejection", err)
	}

	world = validWorld()
	world.ResourceDigest = strings.Repeat("A", 64)
	err = decodeWorldValue(world)
	if err == nil || !strings.Contains(err.Error(), "resourceManifestSHA256 must be lowercase SHA-256") {
		t.Fatalf("DecodeWorld() error = %v, want resourceManifestSHA256 rejection", err)
	}

	world = validWorld()
	world.PackageLock = "mkosi.profiles/resource-package-lock.json"
	err = decodeWorldValue(world)
	if err == nil || !strings.Contains(err.Error(), "packageLock must be an absolute path") {
		t.Fatalf("DecodeWorld() error = %v, want packageLock path rejection", err)
	}

	world = validWorld()
	world.PackageLockDigest = "bad"
	err = decodeWorldValue(world)
	if err == nil || !strings.Contains(err.Error(), "packageLockSHA256 must be lowercase SHA-256") {
		t.Fatalf("DecodeWorld() error = %v, want packageLockSHA256 rejection", err)
	}
}

func TestDecodeWorldArtifactProvenance(t *testing.T) {
	world := validWorld()
	world.Artifacts = []WorldArtifact{{
		Name:      "installer-uki",
		Kind:      "uki",
		Path:      "/repo/_build/mkosi/katl-installer.efi",
		RepoPath:  "_build/mkosi/katl-installer.efi",
		Digest:    strings.Repeat("a", 64),
		SizeBytes: 1,
		Source:    "resource-test-manifest",
		Action:    "cache-resolved",
	}}
	world.ArtifactInputs = &WorldArtifactInputs{
		MkosiProfiles: []WorldMkosiProfile{{
			Name:         "installer-image",
			Path:         "mkosi.profiles/installer-image",
			ConfigSHA256: strings.Repeat("b", 64),
		}},
		PackageSets: []WorldPackageSetInput{{
			Name:         "installer-image",
			LockSHA256:   strings.Repeat("c", 64),
			PackageCount: 139,
		}},
	}
	if err := decodeWorldValue(world); err != nil {
		t.Fatalf("DecodeWorld() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*World)
		want   string
	}{
		{
			name:   "relative artifact path",
			mutate: func(world *World) { world.Artifacts[0].Path = "_build/mkosi/katl-installer.efi" },
			want:   "vmtestArtifacts[0].path must be an absolute path",
		},
		{
			name:   "artifact digest",
			mutate: func(world *World) { world.Artifacts[0].Digest = strings.Repeat("A", 64) },
			want:   "vmtestArtifacts[0].sha256 must be lowercase SHA-256",
		},
		{
			name:   "artifact action",
			mutate: func(world *World) { world.Artifacts[0].Action = "maybe" },
			want:   "vmtestArtifacts[0].action must be built, cache-resolved, or validated",
		},
		{
			name:   "profile digest",
			mutate: func(world *World) { world.ArtifactInputs.MkosiProfiles[0].ConfigSHA256 = "bad" },
			want:   "vmtestArtifactInputs.mkosiProfiles[0].configSHA256 must be lowercase SHA-256",
		},
		{
			name:   "package lock digest",
			mutate: func(world *World) { world.ArtifactInputs.PackageSets[0].LockSHA256 = "bad" },
			want:   "vmtestArtifactInputs.packageSets[0].lockSHA256 must be lowercase SHA-256",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := world
			mutated.Artifacts = append([]WorldArtifact(nil), world.Artifacts...)
			inputs := *world.ArtifactInputs
			inputs.MkosiProfiles = append([]WorldMkosiProfile(nil), world.ArtifactInputs.MkosiProfiles...)
			inputs.PackageSets = append([]WorldPackageSetInput(nil), world.ArtifactInputs.PackageSets...)
			mutated.ArtifactInputs = &inputs
			tt.mutate(&mutated)
			err := decodeWorldValue(mutated)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeWorld() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeWorldRejectsUnknownFieldsAndExtraDocuments(t *testing.T) {
	_, err := DecodeWorld(strings.NewReader(`{"apiVersion":"katl.dev/v1alpha1","kind":"VMTestWorld","extra":true}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("DecodeWorld() error = %v, want unknown field", err)
	}

	_, err = DecodeWorld(strings.NewReader(validWorldJSON(t) + "\n{}"))
	if err == nil || !strings.Contains(err.Error(), "exactly one JSON document") {
		t.Fatalf("DecodeWorld() error = %v, want extra document", err)
	}
}

func TestLoadWorldFromEnv(t *testing.T) {
	worldPath := filepath.Join(t.TempDir(), "world.json")
	if err := os.WriteFile(worldPath, []byte(validWorldJSON(t)), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv(WorldManifestEnv, worldPath)

	world, err := LoadWorldFromEnv()
	if err != nil {
		t.Fatalf("LoadWorldFromEnv() error = %v", err)
	}
	if world.RunID != "20260606T120000Z-abc123" {
		t.Fatalf("RunID = %q", world.RunID)
	}
}

func TestRequireWorldReportsRunnerHint(t *testing.T) {
	t.Setenv(WorldManifestEnv, "")
	tb := &fakeTB{}

	_ = RequireWorld(tb)
	if !tb.failed {
		t.Fatal("RequireWorld() did not fail")
	}
	if !strings.Contains(tb.message, WorldManifestEnv) || !strings.Contains(tb.message, "scripts/vmtest-run") {
		t.Fatalf("Fatalf message = %q", tb.message)
	}
}

func decodeWorldValue(world World) error {
	data, err := json.Marshal(world)
	if err != nil {
		return err
	}
	_, err = DecodeWorld(bytes.NewReader(data))
	return err
}

func validWorldJSON(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(validWorld())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return string(data)
}

func validWorld() World {
	return World{
		APIVersion:  WorldAPIVersion,
		Kind:        WorldKind,
		RunID:       "20260606T120000Z-abc123",
		RunDir:      "/tmp/katl-vmtest/20260606T120000Z-abc123",
		CacheDir:    "/repo/_build/vmtest",
		ArtifactDir: "/tmp/katl-vmtest/20260606T120000Z-abc123/artifacts",
		ScenarioDir: "/tmp/katl-vmtest/20260606T120000Z-abc123/scenarios",
		Libvirt: WorldLibvirt{
			URI:          "qemu:///system",
			Network:      "default",
			StoragePool:  "default",
			StoragePath:  "/var/lib/libvirt/images",
			DomainPrefix: "katl-20260606T120000Z-abc123",
		},
		Network: WorldNetwork{
			Backend:   NetworkLibvirt,
			Name:      "default",
			CIDR:      "10.77.0.0/24",
			Gateway:   "10.77.0.1",
			LeaseFile: "/tmp/katl-vmtest/20260606T120000Z-abc123/network/leases.json",
		},
		Capabilities: map[string]WorldStatus{
			"image-tool":           WorldStatusPassed,
			"kvm":                  WorldStatusPassed,
			"libvirt":              WorldStatusPassed,
			"libvirt-network":      WorldStatusPassed,
			"libvirt-storage-pool": WorldStatusPassed,
			"ovmf":                 WorldStatusPassed,
			"vsock":                WorldStatusPassed,
		},
	}
}
