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
	want := []string{"v1.36.0", "v1.36.1", "v1.36.2", "v1.36.3"}
	if !reflect.DeepEqual(supported.Versions, want) {
		t.Fatalf("versions = %v, want %v", supported.Versions, want)
	}
}

func TestDecodeSupportedVersionsRejectsInvalidPolicy(t *testing.T) {
	tests := []struct {
		name     string
		versions string
		extra    string
		want     string
	}{
		{name: "empty", versions: "", want: "must not be empty"},
		{name: "malformed", versions: `"1.36.0"`, want: "must look like"},
		{name: "duplicate", versions: `"v1.36.0", "v1.36.0"`, want: "duplicated"},
		{name: "unordered", versions: `"v1.36.1", "v1.36.0"`, want: "ordered from oldest to newest"},
		{name: "unknown field", versions: `"v1.36.0"`, extra: `, "unknown": true`, want: "unknown field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := `{
				"apiVersion": "katl.dev/v1alpha1",
				"kind": "KubernetesSupportedVersions",
				"versions": [` + test.versions + `]` + test.extra + `
			}`
			_, err := DecodeSupportedVersions([]byte(data))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeSupportedVersions() error = %v, want %q", err, test.want)
			}
		})
	}
}
