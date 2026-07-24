package kubernetesrelease

import (
	"reflect"
	"strings"
	"testing"
)

func TestDefaultSupportedVersions(t *testing.T) {
	supported, err := DefaultSupportedVersions()
	if err != nil {
		t.Fatalf("DefaultSupportedVersions() error = %v", err)
	}
	var got []string
	for _, version := range supported.Versions {
		got = append(got, version.PayloadVersion)
	}
	want := []string{"v1.36.0", "v1.36.1", "v1.36.2", "v1.36.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}
	if got := supported.Versions[3].ArtifactVersion(); got != "v1.36.3-katl.7" {
		t.Fatalf("artifact version = %q", got)
	}
}

func TestSupportedVersionsSelect(t *testing.T) {
	supported, err := DefaultSupportedVersions()
	if err != nil {
		t.Fatal(err)
	}
	selected, err := supported.Select("v1.36.2")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if len(selected) != 1 || selected[0].PayloadVersion != "v1.36.2" {
		t.Fatalf("selected = %#v", selected)
	}
	if _, err := supported.Select("v1.35.0"); err == nil || !strings.Contains(err.Error(), "not declared as supported") {
		t.Fatalf("Select() error = %v", err)
	}
}

func TestSupportedVersionsChangedSince(t *testing.T) {
	supported, err := DefaultSupportedVersions()
	if err != nil {
		t.Fatal(err)
	}
	previous := supported
	previous.Versions = copyVersions(supported.Versions[:3])
	previous.Versions[1].ArtifactRevision--

	changed, err := supported.ChangedSince(previous)
	if err != nil {
		t.Fatalf("ChangedSince() error = %v", err)
	}
	var got []string
	for _, version := range changed {
		got = append(got, version.PayloadVersion)
	}
	want := []string{"v1.36.1", "v1.36.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed = %v, want %v", got, want)
	}
}

func TestSupportedVersionsChangedSinceRequiresRevision(t *testing.T) {
	supported, err := DefaultSupportedVersions()
	if err != nil {
		t.Fatal(err)
	}
	previous := supported
	previous.Versions = copyVersions(supported.Versions)
	previous.Versions[0].Packages.Kubeadm = "0:1.36.0-previous"

	if _, err := supported.ChangedSince(previous); err == nil || !strings.Contains(err.Error(), "without advancing artifactRevision") {
		t.Fatalf("ChangedSince() error = %v", err)
	}
}

func TestDecodeSupportedVersionsRejectsInvalidPolicy(t *testing.T) {
	validVersion := `{
		"payloadVersion": "v1.36.0",
		"artifactRevision": 1,
		"packages": {
			"kubeadm": "0:1.36.0-1",
			"kubelet": "0:1.36.0-1",
			"kubectl": "0:1.36.0-1",
			"criTools": "0:1.36.0-1"
		}
	}`
	nextVersion := strings.ReplaceAll(validVersion, "1.36.0", "1.36.1")
	tests := []struct {
		name     string
		versions string
		digest   string
		extra    string
		want     string
	}{
		{name: "empty", versions: "", want: "must not be empty"},
		{name: "malformed", versions: strings.Replace(validVersion, "v1.36.0", "1.36.0", 1), want: "must look like"},
		{name: "duplicate", versions: validVersion + ", " + validVersion, want: "duplicated"},
		{name: "unordered", versions: nextVersion + ", " + validVersion, want: "ordered from oldest to newest"},
		{name: "zero revision", versions: strings.Replace(validVersion, `"artifactRevision": 1`, `"artifactRevision": 0`, 1), want: "at least 1"},
		{name: "package mismatch", versions: strings.Replace(validVersion, `"kubeadm": "0:1.36.0-1"`, `"kubeadm": "0:1.36.1-1"`, 1), want: "does not match its payload"},
		{name: "bad digest", versions: validVersion, digest: "sha256:nope", want: "recipeDigest"},
		{name: "unknown field", versions: validVersion, extra: `, "unknown": true`, want: "unknown field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			digest := test.digest
			if digest == "" {
				digest = "sha256:" + strings.Repeat("a", 64)
			}
			data := `{
				"apiVersion": "katl.dev/v1alpha1",
				"kind": "KubernetesSupportedVersions",
				"recipeDigest": "` + digest + `",
				"versions": [` + test.versions + `]` + test.extra + `
			}`
			_, err := DecodeSupportedVersions([]byte(data))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeSupportedVersions() error = %v, want %q", err, test.want)
			}
		})
	}
}
