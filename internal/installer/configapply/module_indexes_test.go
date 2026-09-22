package configapply

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestApplyRemovesModuleIndexes(t *testing.T) {
	for _, keepKubernetes := range []bool{false, true} {
		name := "last-extension"
		if keepKubernetes {
			name = "retain-kubernetes"
		}
		t.Run(name, func(t *testing.T) {
			testRemoveModuleIndexes(t, keepKubernetes)
		})
	}
}

func testRemoveModuleIndexes(t *testing.T, keepKubernetes bool) {
	t.Helper()
	root := t.TempDir()
	request := trustedBundleRequest(root, TrustedBundleRequest{})
	if !keepKubernetes {
		request.CurrentRecord.Sysexts = nil
	}
	target := kernelmodule.Target{
		Release:       "6.12.1",
		RuntimeSHA256: request.CurrentRecord.Root.RuntimeArtifactSHA256,
	}
	request.CurrentRecord.ExtensionRelease = &extensionrelease.Manifest{
		Target: extensionrelease.Target{
			Version:          request.CurrentRecord.RuntimeVersion,
			Architecture:     request.CurrentRecord.Root.Architecture,
			Flavour:          "standard",
			RuntimeInterface: request.CurrentRecord.Root.RuntimeInterface,
			Kernel:           target,
		},
	}
	payload := manifest.SystemExtensionPayloadRef{
		Name:      "driver.raw",
		Role:      "systemd-sysext",
		MediaType: "application/vnd.katl.sysext.raw.v1",
		Digest:    "sha256:" + strings.Repeat("c", 64),
		SizeBytes: 100,
	}
	contract := &kernelmodule.Contract{
		Target: target,
		Modules: []kernelmodule.Module{{
			Name:   "example",
			Path:   "usr/lib/modules/6.12.1/extra/example.ko",
			SHA256: strings.Repeat("d", 64),
		}},
	}
	request.CurrentManifest.Node.SystemExtensions = []manifest.SystemExtension{{
		Bundle:                     "registry.example/driver:v1",
		OCIManifestDigest:          "sha256:" + strings.Repeat("a", 64),
		BundleManifestDigest:       "sha256:" + strings.Repeat("b", 64),
		ArtifactVersion:            "1",
		PayloadVersion:             "1",
		Architecture:               request.CurrentRecord.Root.Architecture,
		SupportedRuntimeInterfaces: []string{request.CurrentRecord.Root.RuntimeInterface},
		Kernel:                     contract,
		Payloads:                   []manifest.SystemExtensionPayloadRef{payload},
	}}
	for _, name := range []string{"driver", kernelmodule.IndexExtensionName} {
		ref := generation.ExtensionRef{
			Name:            name,
			Path:            filepath.Join(generation.GenerationRecordsDir, request.CurrentRecord.GenerationID, "sysext", name+".raw"),
			ActivationPath:  generation.DefaultExtensionsActivationDir + "/" + name + ".raw",
			SHA256:          strings.Repeat("c", 64),
			ArtifactVersion: "1",
			PayloadVersion:  "1",
			Architecture:    request.CurrentRecord.Root.Architecture,
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: []string{request.CurrentRecord.Root.RuntimeInterface},
			},
		}
		if name == "driver" {
			ref.Compatibility.Kernel = contract
		} else {
			ref.Compatibility.ModuleIndexes = &kernelmodule.IndexSelection{
				Target:        target,
				ModulesSHA256: strings.Repeat("d", 64),
			}
		}
		request.CurrentRecord.Sysexts = append(request.CurrentRecord.Sysexts, ref)
	}
	absent := []manifest.SystemExtension{}
	request.ClusterDefaults.SystemExtensions = &absent

	result, err := ApplyTrustedBundle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	persisted, _, err := generation.ReadGeneration(root, request.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range persisted.Sysexts {
		if ref.Compatibility.Kernel != nil || ref.Compatibility.ModuleIndexes != nil {
			t.Fatalf("removed driver retained kernel assets: %+v", ref)
		}
	}
	if keepKubernetes && (len(persisted.Sysexts) != 1 || persisted.Sysexts[0].Name != "kubernetes") {
		t.Fatalf("removal changed unrelated extensions: %+v", persisted.Sysexts)
	}
	if !keepKubernetes && len(persisted.Sysexts) != 0 {
		t.Fatalf("last extension removal retained extensions: %+v", persisted.Sysexts)
	}

	request.CurrentRecord = result.Plan.GenerationRecord
	request.CurrentManifest = result.Manifest
	request.GenerationID = "removal-repeat"
	request.DesiredVersion = "3"
	if _, err := ApplyTrustedBundle(context.Background(), request); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("repeat removal = %v, want no changes", err)
	}
}
