package katlosimage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// These fields are the strict image-index decoder shipped in beta.14. Keep
// this snapshot independent of the current Index type so new fields cannot
// silently expand the compatibility gate.
type beta14Index struct {
	Flavour          string            `json:"flavour,omitempty"`
	APIVersion       string            `json:"apiVersion"`
	Kind             string            `json:"kind"`
	ImageRole        string            `json:"imageRole"`
	Format           string            `json:"format"`
	Version          string            `json:"version"`
	BuildID          string            `json:"buildID"`
	Architecture     string            `json:"architecture"`
	RuntimeInterface string            `json:"runtimeInterface"`
	CreatedAt        string            `json:"createdAt"`
	Components       []beta14Component `json:"components"`
}

type beta14Component struct {
	Name            string              `json:"name"`
	Role            string              `json:"role"`
	Path            string              `json:"path"`
	Format          string              `json:"format"`
	SizeBytes       int64               `json:"sizeBytes"`
	SHA256          string              `json:"sha256"`
	Version         string              `json:"version"`
	PayloadVersion  string              `json:"payloadVersion,omitempty"`
	Architecture    string              `json:"architecture"`
	Compatibility   beta14Compatibility `json:"compatibility"`
	SourceRepo      *beta14SourceRepo   `json:"sourceRepo,omitempty"`
	PackageVersions map[string]string   `json:"packageVersions,omitempty"`
	InstallTarget   beta14InstallTarget `json:"installTarget"`
}

type beta14Compatibility struct {
	RuntimeInterface  string            `json:"runtimeInterface"`
	Boot              json.RawMessage   `json:"boot,omitempty"`
	RuntimeRoot       beta14RuntimeRoot `json:"runtimeRoot,omitempty"`
	KernelCommandLine []string          `json:"kernelCommandLine,omitempty"`
}

type beta14RuntimeRoot struct {
	Interface      string `json:"interface,omitempty"`
	ArtifactPath   string `json:"artifactPath,omitempty"`
	ArtifactSHA256 string `json:"artifactSHA256,omitempty"`
}

type beta14SourceRepo struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
	Minor   string `json:"minor"`
}

type beta14InstallTarget struct {
	Kind         string `json:"kind"`
	Filesystem   string `json:"filesystem,omitempty"`
	MinSizeBytes int64  `json:"minSizeBytes,omitempty"`
	Filename     string `json:"filename,omitempty"`
	Name         string `json:"name,omitempty"`
}

func decodeBeta14Index(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var index beta14Index
	if err := decoder.Decode(&index); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func TestBeta14IndexRejectsAddedField(t *testing.T) {
	if err := decodeBeta14Index([]byte(`{"extensionRelease":{}}`)); err == nil || !strings.Contains(err.Error(), `unknown field "extensionRelease"`) {
		t.Fatalf("beta.14 decoder error = %v", err)
	}
}

func TestBuiltImageIndexReadableByBeta14(t *testing.T) {
	image := os.Getenv("KATL_TEST_KATLOS_IMAGE")
	if image == "" {
		t.Skip("set KATL_TEST_KATLOS_IMAGE to a built KatlOS image")
	}
	data, err := exec.Command("unsquashfs", "-cat", image, "katlos/image.json").Output()
	if err != nil {
		t.Fatalf("read built image index: %v", err)
	}
	if err := decodeBeta14Index(data); err != nil {
		t.Fatalf("beta.14 agent rejects built image index: %v", err)
	}
}
