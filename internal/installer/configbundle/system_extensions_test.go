package configbundle

import (
	"context"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestPerNodeExtensionRelease(t *testing.T) {
	source := SourceConfig{}
	source.Spec.Defaults.SystemExtensions = supplied([]SourceSystemExtension{{
		Release: "registry.example/drbd9",
	}})
	source.Spec.Nodes = []SourceNode{{Name: "old"}, {Name: "new"}, {
		Name: "removed",
		SystemExtensions: supplied([]SourceSystemExtension{{
			Release: "registry.example/drbd9",
			State:   "absent",
		}}),
	}}
	oldPin, newPin := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	oldTarget := extensionrelease.Target{
		Version:          "2026.9.1",
		Architecture:     "x86_64",
		Flavour:          "standard",
		RuntimeInterface: "katl-runtime-1",
		Kernel: kernelmodule.Target{
			Release:       "6.12",
			RuntimeSHA256: strings.Repeat("c", 64),
		},
	}
	newTarget := oldTarget
	newTarget.Version, newTarget.Kernel.RuntimeSHA256 = "2026.9.2", strings.Repeat("d", 64)
	planning := PlanningInputs{ExtensionReleases: map[string]extensionrelease.Manifest{
		"old": {
			Target:     oldTarget,
			Extensions: map[string]string{"registry.example/drbd9": "registry.example/drbd9@" + oldPin},
		},
		"new": {
			Target:     newTarget,
			Extensions: map[string]string{"registry.example/drbd9": "registry.example/drbd9@" + newPin},
		},
	}}
	fetch := func(_ context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
		_, pin, _ := strings.Cut(request.Reference, "@")
		return systemextensionbundle.Resolved{
			Reference:            request.Reference,
			OCIManifestDigest:    pin,
			BundleManifestDigest: "sha256:" + strings.Repeat("e", 64),
			Bundle: systemextensionbundle.Bundle{
				ArtifactVersion:            "v1",
				PayloadVersion:             "v1",
				Architecture:               "x86_64",
				SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
			},
			Payloads: []systemextensionbundle.Payload{{Descriptor: systemextensionbundle.Descriptor{
				FileName:  "drbd.raw",
				Role:      "systemd-sysext",
				MediaType: systemextensionbundle.SysextMediaType,
				Digest:    pin,
				SizeBytes: 1,
			}}},
		}, nil
	}
	resolved, bundles, err := resolveSystemExtensionBundles(context.Background(), source, planning, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 2 {
		t.Fatalf("acquired %d bundles, want 2", len(bundles))
	}
	for i, pin := range []string{oldPin, newPin} {
		entries := lowerSystemExtensions(resolved.Spec.Nodes[i].SystemExtensions)
		if len(entries) != 1 || entries[0].OCIManifestDigest != pin || entries[0].Release != "registry.example/drbd9" {
			t.Fatalf("node %d selections = %#v", i, entries)
		}
	}
	if entries := lowerSystemExtensions(resolved.Spec.Nodes[2].SystemExtensions); len(entries) != 0 {
		t.Fatalf("removed default survived: %#v", entries)
	}
}

func TestRemovedDefaultDoesNotResolve(t *testing.T) {
	source := SourceConfig{}
	source.Spec.Defaults.SystemExtensions = supplied([]SourceSystemExtension{{
		Release: "registry.example/unavailable",
	}})
	source.Spec.Nodes = []SourceNode{{
		Name: "node",
		SystemExtensions: supplied([]SourceSystemExtension{{
			Release: "registry.example/unavailable",
			State:   manifest.SystemExtensionAbsent,
		}}),
	}}
	_, bundles, err := resolveSystemExtensionBundles(context.Background(), source, PlanningInputs{}, func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
		t.Fatal("downloaded a removed default")
		return systemextensionbundle.Resolved{}, nil
	})
	if err != nil || len(bundles) != 0 {
		t.Fatalf("removed default: bundles=%v err=%v", bundles, err)
	}
}

func TestUnselectedNodeDoesNotResolve(t *testing.T) {
	source := SourceConfig{}
	source.Spec.Nodes = []SourceNode{
		{Name: "selected"},
		{
			Name: "offline",
			SystemExtensions: supplied([]SourceSystemExtension{{
				Release: "registry.example/unavailable",
			}}),
		},
	}
	_, bundles, err := resolveSystemExtensionBundles(context.Background(), source, PlanningInputs{Nodes: []string{"selected"}}, func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
		t.Fatal("downloaded an extension for an unselected node")
		return systemextensionbundle.Resolved{}, nil
	})
	if err != nil || len(bundles) != 0 {
		t.Fatalf("selection scope: bundles=%v err=%v", bundles, err)
	}
}

func TestRepositoryLayering(t *testing.T) {
	base := supplied([]SourceSystemExtension{
		{Release: "registry.example/team-a/driver"},
		{Release: "other.example/team-a/driver"},
	})
	pin := "registry.example/team-a/driver@sha256:" + strings.Repeat("a", 64)
	merged, err := mergeSourceSystemExtensions(base, supplied([]SourceSystemExtension{{Bundle: pin}}))
	if err != nil {
		t.Fatal(err)
	}
	values, _ := merged.Get()
	if len(values) != 2 || values[0].Release != "other.example/team-a/driver" || values[1].Bundle != pin || values[1].Release != "" {
		t.Fatalf("repository override = %#v", values)
	}

	removed, err := mergeSourceSystemExtensions(merged, supplied([]SourceSystemExtension{{
		Release: "registry.example/team-a/driver",
		State:   manifest.SystemExtensionAbsent,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	values, _ = removed.Get()
	if len(values) != 1 || values[0].Release != "other.example/team-a/driver" {
		t.Fatalf("repository removal = %#v", values)
	}
}

func TestDuplicateRepository(t *testing.T) {
	for _, entries := range [][]SourceSystemExtension{
		{
			{Release: "registry.example/driver"},
			{Bundle: "registry.example/driver:v2"},
		},
		{
			{Bundle: "registry.example/driver@sha256:" + strings.Repeat("a", 64)},
			{Bundle: "registry.example/driver@sha256:" + strings.Repeat("b", 64)},
		},
	} {
		if err := validateSourceSystemExtensions("systemExtensions", supplied(entries)); err == nil {
			t.Fatalf("accepted two selections for one repository: %#v", entries)
		}
	}
}

func TestExtensionNameRejected(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "  defaults:\n", `  defaults:
    systemExtensions:
      - name: driver
        release: registry.example/driver
`, 1)
	if _, err := DecodeSource(strings.NewReader(source)); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("removed name field: %v", err)
	}
}
