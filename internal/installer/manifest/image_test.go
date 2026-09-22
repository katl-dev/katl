package manifest

import (
	"testing"

	"github.com/katl-dev/katl/internal/extensionrelease"
)

func TestImageEquality(t *testing.T) {
	image := KatlosImage{
		LocalRef: "images/katlos.squashfs",
		SHA256:   "image-digest",
		ExtensionRelease: &extensionrelease.Manifest{
			Target: extensionrelease.Target{
				Version: "2026.9.2",
			},
			Extensions: map[string]string{"registry.example/drbd9": "driver-digest"},
		},
	}
	other := KatlosImage{
		LocalRef: "images/katlos.squashfs",
		SHA256:   "image-digest",
		ExtensionRelease: &extensionrelease.Manifest{
			Target: extensionrelease.Target{
				Version: "2026.9.2",
			},
			Extensions: map[string]string{"registry.example/drbd9": "driver-digest"},
		},
	}
	if !image.Equal(other) {
		t.Fatal("independently allocated selections must be equal")
	}

	other.ExtensionRelease.Extensions["registry.example/drbd9"] = "different-driver"
	if image.Equal(other) {
		t.Fatal("different driver mappings must not be equal")
	}
	other.ExtensionRelease.Extensions["registry.example/drbd9"] = "driver-digest"
	other.LocalRef = "different/image.squashfs"
	if image.Equal(other) {
		t.Fatal("different image locations must not be equal")
	}
}
