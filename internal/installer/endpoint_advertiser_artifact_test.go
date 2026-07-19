package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/katlosimage"
)

func TestEndpointAdvertiserArtifactRoundTrip(t *testing.T) {
	payload, contents := writeInstallPayload(t)
	root := t.TempDir()
	artifact, err := endpointAdvertiserArtifact(payload)
	if err != nil {
		t.Fatal(err)
	}
	target := rootedInstallerPath(root, EndpointAdvertiserArtifactPath)
	if err := copyVerifiedComponent(payload.ComponentPath(payload.EndpointAdvertiser), target, payload.EndpointAdvertiser); err != nil {
		t.Fatal(err)
	}
	if err := writeEndpointAdvertiserArtifact(root, artifact); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEndpointAdvertiserArtifact(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Extension.Name != katlosimage.EndpointAdvertiserName || got.Extension.SHA256 != payload.EndpointAdvertiser.SHA256 || got.SizeBytes != int64(len(contents.endpoint)) {
		t.Fatalf("artifact = %#v", got)
	}
}

func TestEndpointAdvertiserArtifactRejectsTamperedPayload(t *testing.T) {
	payload, _ := writeInstallPayload(t)
	root := t.TempDir()
	artifact, err := endpointAdvertiserArtifact(payload)
	if err != nil {
		t.Fatal(err)
	}
	target := rootedInstallerPath(root, EndpointAdvertiserArtifactPath)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(strings.Repeat("x", int(artifact.SizeBytes))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeEndpointAdvertiserArtifact(root, artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEndpointAdvertiserArtifact(root); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("ReadEndpointAdvertiserArtifact() error = %v, want digest mismatch", err)
	}
}
