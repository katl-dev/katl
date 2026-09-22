package configapply

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestRetainedExtensionSelection(t *testing.T) {
	for _, role := range []string{systemextensionbundle.SysextRole, systemextensionbundle.ConfextRole} {
		t.Run(role, func(t *testing.T) {
			base, path := retainedExtensionBase(t, role)
			input := selectionDocument("release: registry.example/driver")
			fetch := func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
				t.Fatal("unchanged pinned selection contacted the registry")
				return systemextensionbundle.Resolved{}, nil
			}
			prepared, err := PrepareNodeConfigurationChange(context.Background(), input, base, fetch)
			if err != nil {
				t.Fatal(err)
			}
			if len(prepared.Request.SystemExtensionPayloads) != len(base.CurrentManifest.Node.SystemExtensions[0].Payloads) || string(prepared.Request.SystemExtensionPayloads[0].Data) != "retained driver image" {
				t.Fatal("operation did not retain the installed payload")
			}
			desired, err := DesiredManifest(prepared.Request)
			if err != nil || len(desired.Node.SystemExtensions) != 1 || desired.Node.SystemExtensions[0].ResolvedBundle != "registry.example/driver@sha256:"+strings.Repeat("a", 64) {
				t.Fatalf("resolved selection = %+v, %v", desired.Node.SystemExtensions, err)
			}

			// Execution reads the frozen operation, not the source artifact or the
			// previously installed file. A damaged file must still reject a new plan.
			if err := os.WriteFile(path, []byte("damaged driver image"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := PrepareNodeConfigurationChange(context.Background(), input, base, fetch); err == nil || !strings.Contains(err.Error(), "digest or size") {
				t.Fatalf("corrupt retained payload accepted: %v", err)
			}
			replayed, err := PrepareNodeConfigurationChange(context.Background(), string(prepared.Document), base, fetch)
			if err != nil || string(replayed.Request.SystemExtensionPayloads[0].Data) != "retained driver image" {
				t.Fatalf("frozen operation could not replay: %v", err)
			}
		})
	}
}

func TestSelectionUsesTargetRelease(t *testing.T) {
	base, _ := retainedExtensionBase(t, systemextensionbundle.SysextRole)
	base.CurrentRecord.ExtensionRelease.Extensions["registry.example/driver"] = "registry.example/driver@sha256:" + strings.Repeat("b", 64)
	var requested string
	_, err := PrepareNodeConfigurationChange(context.Background(), selectionDocument("release: registry.example/driver"), base, func(_ context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
		requested = request.Reference
		return systemextensionbundle.Resolved{}, fmt.Errorf("target artifact unavailable")
	})
	if err == nil || requested != "registry.example/driver@sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("target selection reused the installed digest: requested %q, %v", requested, err)
	}
}

func TestRemovedSelectionNeedsNoArtifact(t *testing.T) {
	base, path := retainedExtensionBase(t, systemextensionbundle.SysextRole)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareNodeConfigurationChange(context.Background(), selectionDocument("release: registry.example/driver\n      state: absent"), base, func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
		t.Fatal("removal attempted artifact acquisition")
		return systemextensionbundle.Resolved{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	desired, err := DesiredManifest(prepared.Request)
	if err != nil || len(desired.Node.SystemExtensions) != 0 || len(prepared.Request.SystemExtensionPayloads) != 0 {
		t.Fatalf("removal retained extension state: %+v, %v", desired.Node.SystemExtensions, err)
	}
}

func TestPreparedSelectionFreezesMutableTag(t *testing.T) {
	base, _ := retainedExtensionBase(t, systemextensionbundle.SysextRole)
	data := []byte("replacement driver image")
	pin := "sha256:" + strings.Repeat("b", 64)
	prepared, err := PrepareNodeConfigurationChange(context.Background(), selectionDocument("bundle: registry.example/driver:stable"), base, func(_ context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
		if request.Reference != "registry.example/driver:stable" {
			t.Fatalf("preparation resolved %q", request.Reference)
		}
		return systemextensionbundle.Resolved{
			OCIManifestDigest:    pin,
			BundleManifestDigest: "sha256:" + strings.Repeat("c", 64),
			Bundle: systemextensionbundle.Bundle{
				ArtifactVersion:            "2",
				PayloadVersion:             "2",
				Architecture:               "x86_64",
				SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
			},
			Payloads: []systemextensionbundle.Payload{{
				Descriptor: systemextensionbundle.Descriptor{
					FileName:  "driver.raw",
					Role:      systemextensionbundle.SysextRole,
					MediaType: systemextensionbundle.SysextMediaType,
					Digest:    fmt.Sprintf("sha256:%x", sha256.Sum256(data)),
					SizeBytes: int64(len(data)),
				},
				Data: data,
			}},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The registry can move the tag after acceptance. Execution consumes the
	// prepared document and must not ask the registry what the tag means now.
	replayed, err := PrepareNodeConfigurationChange(context.Background(), string(prepared.Document), base, func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
		t.Fatal("execution re-resolved the mutable tag")
		return systemextensionbundle.Resolved{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	desired, err := DesiredManifest(replayed.Request)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired.Node.SystemExtensions) != 1 || desired.Node.SystemExtensions[0].Bundle != "registry.example/driver:stable" || desired.Node.SystemExtensions[0].ResolvedBundle != "registry.example/driver@"+pin {
		t.Fatalf("execution changed the accepted selection: %+v", desired.Node.SystemExtensions)
	}
	if len(replayed.Request.SystemExtensionPayloads) != 1 || string(replayed.Request.SystemExtensionPayloads[0].Data) != "replacement driver image" {
		t.Fatal("execution did not retain the acquired payload")
	}
}

func selectionDocument(selection string) string {
	return `apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: next-boot
spec:
  systemExtensionSelections:
    - ` + selection + "\n"
}

func retainedExtensionBase(t *testing.T, role string) (TrustedBundleRequest, string) {
	t.Helper()
	base := trustedBundleRequest(t.TempDir(), TrustedBundleRequest{})
	target := extensionrelease.Target{
		Version:          base.CurrentRecord.Root.RuntimeVersion,
		Architecture:     base.CurrentRecord.Root.Architecture,
		Flavour:          "standard",
		RuntimeInterface: base.CurrentRecord.Root.RuntimeInterface,
		Kernel: kernelmodule.Target{
			Release:       "6.12.1",
			RuntimeSHA256: base.CurrentRecord.Root.RuntimeArtifactSHA256,
		},
	}
	pin := "sha256:" + strings.Repeat("a", 64)
	base.CurrentRecord.ExtensionRelease = &extensionrelease.Manifest{
		Target: target,
		Extensions: map[string]string{
			"registry.example/driver": "registry.example/driver@" + pin,
		},
	}
	data := []byte("retained driver image")
	payload := manifest.SystemExtensionPayloadRef{
		Name:      "driver.raw",
		Role:      role,
		MediaType: systemextensionbundle.SysextMediaType,
		Digest:    fmt.Sprintf("sha256:%x", sha256.Sum256(data)),
		SizeBytes: int64(len(data)),
	}
	if role == systemextensionbundle.ConfextRole {
		payload.Role = systemextensionbundle.SysextRole
	}
	base.CurrentManifest.Node.SystemExtensions = []manifest.SystemExtension{{
		Release:                    "registry.example/driver",
		ResolvedBundle:             "registry.example/driver@" + pin,
		ReleaseTarget:              &target,
		State:                      manifest.SystemExtensionPresent,
		OCIManifestDigest:          pin,
		BundleManifestDigest:       "sha256:" + strings.Repeat("c", 64),
		ArtifactVersion:            "1",
		PayloadVersion:             "1",
		Architecture:               target.Architecture,
		SupportedRuntimeInterfaces: []string{target.RuntimeInterface},
		Payloads:                   []manifest.SystemExtensionPayloadRef{payload},
	}}
	ref := generation.ExtensionRef{
		Name:   "registry.example/driver#driver.raw",
		Path:   "/var/lib/katl/generations/installed/sysext/driver.raw",
		SHA256: strings.TrimPrefix(payload.Digest, "sha256:"),
	}
	base.CurrentRecord.Sysexts = append(base.CurrentRecord.Sysexts, ref)
	path := filepath.Join(base.Root, ref.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if role == systemextensionbundle.ConfextRole {
		data := []byte("retained driver configuration")
		payload := manifest.SystemExtensionPayloadRef{
			Name:      "driver-config.raw",
			Role:      systemextensionbundle.ConfextRole,
			MediaType: systemextensionbundle.ConfextMediaType,
			Digest:    fmt.Sprintf("sha256:%x", sha256.Sum256(data)),
			SizeBytes: int64(len(data)),
		}
		base.CurrentManifest.Node.SystemExtensions[0].Payloads = append(base.CurrentManifest.Node.SystemExtensions[0].Payloads, payload)
		ref := generation.ExtensionRef{
			Name:   "registry.example/driver#driver-config.raw",
			Path:   "/var/lib/katl/generations/installed/bundled-confext/driver-config.raw",
			SHA256: strings.TrimPrefix(payload.Digest, "sha256:"),
		}
		base.CurrentRecord.BundledConfexts = append(base.CurrentRecord.BundledConfexts, ref)
		path = filepath.Join(base.Root, ref.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return base, path
}
