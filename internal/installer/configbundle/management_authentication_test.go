package configbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/managementidentity"
)

const managementAuthenticationRequired = "spec.managementAuthentication is required: to keep managing nodes installed with trusted-network management, add `managementAuthentication: trusted-network` under spec"

func minimalSource(authenticationLine string) string {
	return `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
spec:
` + authenticationLine + `  kubernetes:
    version: v1.36.1
  defaults:
    access:
      ssh:
        authorizedKeys:
          - ` + testSSHKey + `
  nodes:
    - name: cp-1
      controlPlane: true
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-cp-root
      management:
        address: 192.0.2.11
`
}

func TestSourceManagementAuthentication(t *testing.T) {
	tests := []struct {
		name      string
		mode      managementidentity.Authentication
		reference string
		want      managementidentity.Authentication
		wantErr   string
	}{
		{name: "explicit trusted-network", mode: managementidentity.TrustedNetwork, want: managementidentity.TrustedNetwork},
		{name: "explicit mtls", mode: managementidentity.MutualTLS, want: managementidentity.MutualTLS},
		{name: "explicit trusted-network beside reference", mode: managementidentity.TrustedNetwork, reference: "secrets.yaml", want: managementidentity.TrustedNetwork},
		{name: "reference implies mtls", reference: "secrets.yaml", want: managementidentity.MutualTLS},
		{name: "neither is refused", wantErr: managementAuthenticationRequired},
		{name: "unknown mode is refused", mode: "open", wantErr: "open"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := SourceConfig{Spec: SourceSpec{ManagementAuthentication: tt.mode, ManagementIdentity: tt.reference}}
			got, err := source.ManagementAuthentication()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || got != "" {
					t.Fatalf("ManagementAuthentication() = %q, %v; want no mode and error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ManagementAuthentication() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestEmptyManagementAuthenticationIsRefused(t *testing.T) {
	path := writeSource(t, minimalSource(""))

	err := ValidateSourceFile(path)
	var issues *validationErrors
	if !errors.As(err, &issues) || len(issues.issues) != 1 || issues.issues[0].Path != "spec.managementAuthentication" || !strings.Contains(issues.issues[0].Message, managementAuthenticationRequired) {
		t.Fatalf("ValidateSourceFile() = %v", err)
	}

	archive, _, err := BuildArchive(BuildRequest{SourcePath: path})
	if err == nil || !strings.Contains(err.Error(), managementAuthenticationRequired) || len(archive) != 0 {
		t.Fatalf("BuildArchive() = %d bytes, %v", len(archive), err)
	}

	explicit := writeSource(t, minimalSource("  managementAuthentication: trusted-network\n"))
	if err := ValidateSourceFile(explicit); err != nil {
		t.Fatalf("ValidateSourceFile(explicit) error = %v", err)
	}
}

func TestReadBundleRefusesUnrecordedManagementAuthentication(t *testing.T) {
	archive, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, minimalSource("  managementAuthentication: trusted-network\n"))})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	if bundle, err := ReadBundle(bytes.NewReader(archive), ""); err != nil || bundle.Authentication != managementidentity.TrustedNetwork {
		t.Fatalf("ReadBundle() = %q, %v", bundle.Authentication, err)
	}

	stripped := replaceNormalizedSource(t, archive, func(source *SourceConfig) { source.Spec.ManagementAuthentication = "" })
	bundle, err := ReadBundle(bytes.NewReader(stripped), "")
	if err == nil || !strings.Contains(err.Error(), "rebuild it with `katlctl config bundle`") || bundle.Authentication != "" {
		t.Fatalf("ReadBundle() = %q, %v", bundle.Authentication, err)
	}
}

// replaceNormalizedSource rewrites the bundle's normalized source blob and
// every digest that refers to it.
func replaceNormalizedSource(t *testing.T, archive []byte, edit func(*SourceConfig)) []byte {
	t.Helper()
	files := readTarFiles(t, archive)
	var index struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(files["index.json"], &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("index = %v, %v", index, err)
	}
	manifestPath := "blobs/sha256/" + strings.TrimPrefix(index.Manifests[0].Digest, "sha256:")
	var bundle BundleManifest
	if err := json.Unmarshal(files[manifestPath], &bundle); err != nil {
		t.Fatal(err)
	}
	oldPath := "blobs/sha256/" + strings.TrimPrefix(bundle.Source.NormalizedConfig.Digest, "sha256:")
	source, err := DecodeSource(bytes.NewReader(files[oldPath]))
	if err != nil {
		t.Fatal(err)
	}
	edit(&source)
	normalized, err := marshalCanonical(source)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Source.NormalizedConfig.Digest = digestBytes(normalized)
	bundle.Source.NormalizedConfig.SizeBytes = len(normalized)
	manifest, err := marshalCanonical(bundle)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := digestBytes(manifest)
	members := []member{{descriptor: Descriptor{Digest: manifestDigest}, data: manifest}, {descriptor: Descriptor{Digest: bundle.Source.NormalizedConfig.Digest}, data: normalized}}
	for name, data := range files {
		if strings.HasPrefix(name, "blobs/sha256/") && name != manifestPath && name != oldPath {
			members = append(members, member{descriptor: Descriptor{Digest: "sha256:" + strings.TrimPrefix(name, "blobs/sha256/")}, data: data})
		}
	}
	out, err := writeOCIArchive(manifestDigest, members)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
