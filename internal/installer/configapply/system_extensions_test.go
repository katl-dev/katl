package configapply

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestReleaseAuthority(t *testing.T) {
	root := generation.RootSelection{
		RuntimeVersion:        "1",
		Architecture:          "x86_64",
		Flavour:               "standard",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
	}
	target := extensionrelease.Target{
		Version:          "1",
		Architecture:     "x86_64",
		Flavour:          "standard",
		RuntimeInterface: "katl-runtime-1",
		Kernel: kernelmodule.Target{
			Release:       "6.12.0",
			RuntimeSHA256: root.RuntimeArtifactSHA256,
		},
	}
	pin := "sha256:" + strings.Repeat("b", 64)
	ref := "registry.example/driver@" + pin
	release := extensionrelease.Manifest{
		Target:     target,
		Extensions: map[string]string{"registry.example/driver": ref},
	}
	data := []byte("driver image")
	payload := manifest.SystemExtensionPayloadRef{
		Name:      "driver.raw",
		Role:      "systemd-sysext",
		MediaType: "application/vnd.katl.systemd-sysext.v1+raw",
		Digest:    digestPayload(data),
		SizeBytes: int64(len(data)),
	}
	desired := manifest.SystemExtension{
		Release:                    "registry.example/driver",
		ReleaseTarget:              &target,
		ResolvedBundle:             ref,
		OCIManifestDigest:          pin,
		BundleManifestDigest:       "sha256:" + strings.Repeat("c", 64),
		ArtifactVersion:            "v1",
		PayloadVersion:             "v1",
		Architecture:               "x86_64",
		SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
		Payloads:                   []manifest.SystemExtensionPayloadRef{payload},
	}
	materials := []SystemExtensionPayload{{
		Ref:  payload,
		Data: data,
	}}
	if err := ValidateSystemExtensionMaterials(root, &release, []manifest.SystemExtension{desired}, materials); err != nil {
		t.Fatal(err)
	}

	// Internally consistent caller metadata cannot override the node's release.
	for _, authoritative := range []*extensionrelease.Manifest{nil, {
		Target:     target,
		Extensions: map[string]string{"registry.example/driver": "registry.example/driver@sha256:" + strings.Repeat("d", 64)},
	}} {
		if err := ValidateSystemExtensionMaterials(root, authoritative, []manifest.SystemExtension{desired}, materials); err == nil {
			t.Fatal("accepted selection not advertised by the runtime")
		}
	}
}
