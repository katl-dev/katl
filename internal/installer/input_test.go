package installer

import (
	"strings"
	"testing"
)

func TestDiscoverBootInputPrecedence(t *testing.T) {
	input, err := DiscoverBootInput(BootInputRequest{
		KernelCmdline: "katl.manifest.url=https://kernel.example/manifest.json katl.manifest.sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa katl.node=kernel-node katl.install.mode=auto katl.artifact-base-url=https://kernel.example/artifacts/",
		Files: []BootInputFile{
			inputFile(InputSourceLocalFile, `{"manifestURL":"https://local.example/manifest.json","manifestSHA256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","nodeName":"local-node","installMode":"manual"}`),
			inputFile(InputSourceEmbeddedMedia, `{"manifestURL":"https://embedded.example/manifest.json","nodeName":"embedded-node"}`),
			inputFile(InputSourceEtcKatl, `{"manifestURL":"https://etc.example/manifest.json","nodeName":"etc-node"}`),
			inputFile(InputSourceRunKatl, `{"manifestURL":"https://run.example/manifest.json","nodeName":"run-node"}`),
		},
		Manifest: []byte(`{
  "node": {"identity": {"hostname": "manifest-node"}},
  "katlosImage": {
    "url": "https://manifest.example/artifacts/katlos-install.squashfs"
  }
}`),
	})
	if err != nil {
		t.Fatalf("DiscoverBootInput() error = %v", err)
	}

	if input.ManifestURL != "https://kernel.example/manifest.json" {
		t.Fatalf("manifest URL = %q", input.ManifestURL)
	}
	if input.ManifestSHA256 != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("manifest sha256 = %q", input.ManifestSHA256)
	}
	if input.NodeName != "kernel-node" {
		t.Fatalf("node name = %q", input.NodeName)
	}
	if input.InstallMode != "auto" {
		t.Fatalf("install mode = %q", input.InstallMode)
	}
	if input.ArtifactBaseURL != "https://kernel.example/artifacts/" {
		t.Fatalf("artifact base URL = %q", input.ArtifactBaseURL)
	}
	if input.Action != InstallActionRun || !input.CanMutateDisks() {
		t.Fatalf("action = %q, can mutate = %t; want runnable autoinstall", input.Action, input.CanMutateDisks())
	}
	if input.SelectedSources["manifestURL"] != InputSourceKernelCmdline {
		t.Fatalf("manifest URL source = %q", input.SelectedSources["manifestURL"])
	}
	if input.SelectedSources["manifestSHA256"] != InputSourceKernelCmdline {
		t.Fatalf("manifest sha256 source = %q", input.SelectedSources["manifestSHA256"])
	}
	if !logsContain(input.Logs, "selected manifestURL from kernel-cmdline") {
		t.Fatalf("logs do not report selected kernel source: %#v", input.Logs)
	}
}

func TestDiscoverBootInputURLWithoutDigestDoesNotMutateDisks(t *testing.T) {
	input, err := DiscoverBootInput(BootInputRequest{
		KernelCmdline: "katl.manifest.url=https://kernel.example/manifest.json katl.install.mode=auto",
	})
	if err != nil {
		t.Fatalf("DiscoverBootInput() error = %v", err)
	}
	if input.Action != InstallActionRun {
		t.Fatalf("action = %q, want run", input.Action)
	}
	if input.CanMutateDisks() {
		t.Fatalf("manifest URL without digest must not allow disk mutation")
	}
}

func TestDiscoverBootInputDerivesDefaultsFromYAMLManifest(t *testing.T) {
	input, err := DiscoverBootInput(BootInputRequest{
		Manifest: []byte(`node:
  identity:
    hostname: manifest-node
katlosImage:
  url: https://manifest.example/artifacts/katlos-install.squashfs
`),
	})
	if err != nil {
		t.Fatalf("DiscoverBootInput() error = %v", err)
	}

	if input.NodeName != "manifest-node" {
		t.Fatalf("node name = %q", input.NodeName)
	}
	if input.ArtifactBaseURL != "https://manifest.example/artifacts/" {
		t.Fatalf("artifact base URL = %q", input.ArtifactBaseURL)
	}
}

func TestDiscoverBootInputRunOverridesEtcAndManifest(t *testing.T) {
	input, err := DiscoverBootInput(BootInputRequest{
		Files: []BootInputFile{
			inputFile(InputSourceEtcKatl, `{"manifestPath":"/etc/katl/install.json","nodeName":"etc-node"}`),
			inputFile(InputSourceRunKatl, `{"manifestPath":"/run/katl/install.json","nodeName":"run-node"}`),
		},
		Manifest: []byte(`{"node":{"identity":{"hostname":"manifest-node"}}}`),
	})
	if err != nil {
		t.Fatalf("DiscoverBootInput() error = %v", err)
	}

	if input.ManifestPath != "/run/katl/install.json" {
		t.Fatalf("manifest path = %q", input.ManifestPath)
	}
	if input.NodeName != "run-node" {
		t.Fatalf("node name = %q", input.NodeName)
	}
	if input.SelectedSources["nodeName"] != InputSourceRunKatl {
		t.Fatalf("node source = %q", input.SelectedSources["nodeName"])
	}
}

func TestDiscoverBootInputKeepsExplicitManifestPathAtSameSource(t *testing.T) {
	input, err := DiscoverBootInput(BootInputRequest{
		Files: []BootInputFile{
			inputFile(InputSourceRunKatl, `{"manifestPath":"/run/katl/preseed/install-manifest.json","installMode":"auto"}`),
			inputFile(InputSourceRunKatl, `{"manifestPath":"/run/katl/install-manifest.json"}`),
		},
	})
	if err != nil {
		t.Fatalf("DiscoverBootInput() error = %v", err)
	}
	if input.ManifestPath != "/run/katl/preseed/install-manifest.json" {
		t.Fatalf("manifest path = %q", input.ManifestPath)
	}
	if input.InstallMode != "auto" || input.Action != InstallActionRun {
		t.Fatalf("install mode/action = %q/%q", input.InstallMode, input.Action)
	}
}

func TestDiscoverBootInputMissingManifestWaitsForConfig(t *testing.T) {
	input, err := DiscoverBootInput(BootInputRequest{
		KernelCmdline: "katl.node=lab-node-01",
	})
	if err != nil {
		t.Fatalf("DiscoverBootInput() error = %v", err)
	}

	if input.Action != InstallActionWaitForConfig || !input.WaitForConfig {
		t.Fatalf("action = %q wait = %t, want wait-for-config", input.Action, input.WaitForConfig)
	}
	if input.CanMutateDisks() {
		t.Fatalf("missing manifest input must not allow disk mutation")
	}
}

func TestDiscoverBootInputHoldForDebugDoesNotMutateDisks(t *testing.T) {
	input, err := DiscoverBootInput(BootInputRequest{
		KernelCmdline: "katl.manifest=/run/katl/install.json katl.install.mode=auto katl.hold-for-debug",
	})
	if err != nil {
		t.Fatalf("DiscoverBootInput() error = %v", err)
	}

	if input.Action != InstallActionHoldForDebug {
		t.Fatalf("action = %q, want hold-for-debug", input.Action)
	}
	if input.CanMutateDisks() {
		t.Fatalf("hold-for-debug must not allow disk mutation")
	}
}

func TestDiscoverBootInputRejectsInvalidKernelBoolean(t *testing.T) {
	_, err := DiscoverBootInput(BootInputRequest{
		KernelCmdline: "katl.hold-for-debug=maybe",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported boolean") {
		t.Fatalf("DiscoverBootInput() error = %v, want invalid boolean", err)
	}
}

func inputFile(source InputSource, content string) BootInputFile {
	return BootInputFile{
		Source:  source,
		Path:    string(source) + "/install-input.json",
		Content: []byte(content),
	}
}

func logsContain(logs []string, want string) bool {
	for _, log := range logs {
		if log == want {
			return true
		}
	}
	return false
}
