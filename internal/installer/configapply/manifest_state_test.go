package configapply

import (
	"errors"
	"os"
	"testing"
)

func TestGenerationManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := baseManifest()
	want.Node.ControlPlaneEndpoint = managedEndpoint("192.0.2.1")
	if err := WriteGenerationManifest(root, "generation-1", want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadGenerationManifest(root, "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Node.ControlPlaneEndpoint == nil || got.Node.ControlPlaneEndpoint.Advertisement == nil || got.Node.ControlPlaneEndpoint.Advertisement.BGP.Peers[0].Address != "192.0.2.1" {
		t.Fatalf("generation manifest endpoint = %#v", got.Node.ControlPlaneEndpoint)
	}
}

func TestReadGenerationManifestPreservesNotExist(t *testing.T) {
	_, err := ReadGenerationManifest(t.TempDir(), "generation-0")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadGenerationManifest() error = %v, want os.ErrNotExist", err)
	}
}
