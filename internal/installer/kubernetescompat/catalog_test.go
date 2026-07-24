package kubernetescompat

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
)

func TestResolveReturnsImmutableCompatibleBundle(t *testing.T) {
	entry, err := Resolve(Request{
		KubernetesVersion: "v1.36.1",
		Architecture:      "x86_64",
		RuntimeInterface:  "katl-runtime-1",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	image, err := kubernetesbundle.ParseImageReference(entry.Bundle)
	if err != nil {
		t.Fatalf("ParseImageReference() error = %v", err)
	}
	if image.PayloadVersion != "v1.36.1" || image.ArtifactVersion == "" || image.ManifestDigest == "" {
		t.Fatalf("image = %#v", image)
	}
}

func TestResolveRejectsUnavailableOrIncompatibleSelection(t *testing.T) {
	tests := []struct {
		name    string
		request Request
		want    string
	}{
		{name: "version", request: Request{KubernetesVersion: "v9.99.9"}, want: "not available"},
		{name: "architecture", request: Request{KubernetesVersion: "v1.36.1", Architecture: "aarch64"}, want: "not available for architecture"},
		{name: "runtime", request: Request{KubernetesVersion: "v1.36.1", RuntimeInterface: "katl-runtime-2"}, want: "not compatible"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Resolve(test.request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Resolve() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateRejectsMutableBundle(t *testing.T) {
	catalog := Catalog{
		APIVersion: APIVersion,
		Kind:       Kind,
		Entries: []Entry{{
			KubernetesVersion: "v1.36.1",
			Bundle:            "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1",
			Architectures:     []string{"x86_64"},
			RuntimeInterfaces: []string{"katl-runtime-1"},
		}},
	}
	if err := Validate(catalog); err == nil || !strings.Contains(err.Error(), "immutable OCI manifest digest") {
		t.Fatalf("Validate() error = %v", err)
	}
}
