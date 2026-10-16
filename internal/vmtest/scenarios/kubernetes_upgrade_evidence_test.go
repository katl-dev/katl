package scenarios

import (
	"testing"

	"github.com/katl-dev/katl/internal/installer/operation"
)

func TestUpgradeBundleEvidence(t *testing.T) {
	const (
		repository = "ghcr.io/katl-dev/kubernetes"
		pin        = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		otherPin   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)
	for _, tc := range []struct {
		name      string
		requested string
		recorded  string
		wantError bool
	}{
		{"pinned", repository + "@" + pin, repository + "@" + pin, false},
		{"canonicalized", repository + ":v1@" + pin, repository + "@" + pin, false},
		{"decorated", repository + "@" + pin, repository + ":v1@" + pin, false},
		{"tag resolved", repository + ":v1", repository + "@" + pin, false},
		{"different repository", repository + "@" + pin, "ghcr.io/other/kubernetes@" + pin, true},
		{"different pin", repository + "@" + pin, repository + "@" + otherPin, true},
		{"unpinned", repository + ":v1", repository + ":v1", true},
		{"malformed", repository + ":v1", "not a reference", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := &operation.KubernetesSysextUpdate{
				KubernetesBundleRef:  tc.recorded,
				BundleManifestDigest: otherPin,
				TargetSysextSHA256:   "extension-digest",
			}
			if err := verifyUpgradeBundle(tc.requested, update); (err != nil) != tc.wantError {
				t.Fatalf("verifyUpgradeBundle() error = %v, want error %v", err, tc.wantError)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		update *operation.KubernetesSysextUpdate
	}{
		{"missing update", nil},
		{"missing bundle digest", &operation.KubernetesSysextUpdate{KubernetesBundleRef: repository + "@" + pin, TargetSysextSHA256: "extension-digest"}},
		{"missing extension digest", &operation.KubernetesSysextUpdate{KubernetesBundleRef: repository + "@" + pin, BundleManifestDigest: otherPin}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyUpgradeBundle(repository+"@"+pin, tc.update); err == nil {
				t.Fatal("accepted missing digest evidence")
			}
		})
	}
}
