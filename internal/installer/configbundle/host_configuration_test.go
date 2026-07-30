package configbundle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestResolveHostConfigurationSourcesEmbedsContent(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "files")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "forwarding.conf"), []byte("net.ipv4.ip_forward = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileSets := map[string]SourceHostConfigurationFileSet{
		"forwarding": {Files: []manifest.HostConfigurationFile{{
			Path:   "/etc/sysctl.d/80-forwarding.conf",
			Source: "files/forwarding.conf",
		}}},
	}
	source := SourceConfig{Spec: SourceSpec{Defaults: SourceNodeLayer{
		HostConfiguration: SourceHostConfiguration{FileSets: supplied(fileSets)},
	}}}
	resolved, err := resolveHostConfigurationSources(root, source)
	if err != nil {
		t.Fatalf("resolveHostConfigurationSources() error = %v", err)
	}
	resolvedFileSets, _ := resolved.Spec.Defaults.HostConfiguration.FileSets.Get()
	file := resolvedFileSets["forwarding"].Files[0]
	if file.Source != "" || file.Content == nil || *file.Content != "net.ipv4.ip_forward = 1\n" {
		t.Fatalf("resolved file = %#v", file)
	}
	if err := manifest.ValidateHostConfiguration(lowerHostConfiguration(resolved.Spec.Defaults.HostConfiguration), false); err != nil {
		t.Fatalf("resolved config is not self-contained: %v", err)
	}
}

func TestBuildArchiveCarriesExternalHostConfigurationIntoNodeMaterial(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "files", "storage.conf"), "br_netfilter\n")
	source := strings.Replace(validSourceConfig(), `    hostConfiguration:
      fileSets:
`, `    hostConfiguration:
      sysfs:
        - path: /sys/module/printk/parameters/time
          value: N
      fileSets:
        storage-modules:
          files:
            - path: /etc/modules-load.d/80-storage.conf
              source: files/storage.conf
`, 1)
	sourcePath := filepath.Join(root, "cluster.yaml")
	writeFile(t, sourcePath, source)
	archive, _, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{
		NodeName:                "cp-1",
		AllowMissingKatlosImage: true,
	})
	if err != nil {
		t.Fatalf("ReadSelectedNode() error = %v", err)
	}
	set := selected.NodeMaterial.InstallManifest.Node.HostConfiguration.Sets["storage-modules"]
	if len(set.Files) != 1 || set.Files[0].Source != "" || set.Files[0].Content == nil || *set.Files[0].Content != "br_netfilter\n" {
		t.Fatalf("compiled host configuration = %#v", set)
	}
	if got := selected.NodeMaterial.InstallManifest.Node.HostConfiguration.Sysfs; len(got) != 1 || got[0].Name != "/sys/module/printk/parameters/time" || got[0].Value != "N" {
		t.Fatalf("compiled sysfs configuration = %#v", got)
	}
	foundModules := false
	foundSysfs := false
	for _, file := range selected.NodeMaterial.NativeEtcFiles {
		if file.Path == "/etc/modules-load.d/80-storage.conf" && file.Content == "br_netfilter\n" {
			foundModules = true
		}
		if file.Path == manifest.HostConfigurationSysfsTmpfilesPath {
			foundSysfs = true
		}
	}
	if !foundModules {
		t.Fatalf("node native files do not contain storage module config: %#v", selected.NodeMaterial.NativeEtcFiles)
	}
	if !foundSysfs {
		t.Fatalf("node native files do not contain generated sysfs config: %#v", selected.NodeMaterial.NativeEtcFiles)
	}
}

func TestReadHostConfigurationSourceRejectsEscapesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.conf")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.conf")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		source string
		want   string
	}{
		{source: "../outside.conf", want: "non-escaping"},
		{source: outside, want: "relative"},
		{source: "linked.conf", want: "symbolic link"},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			_, err := readHostConfigurationSource(root, tt.source)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("readHostConfigurationSource() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestReadHostConfigurationSourceAcceptsExplicitCurrentDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "routing.conf"), "router id 192.0.2.1;\n")
	data, err := readHostConfigurationSource(root, "./routing.conf")
	if err != nil {
		t.Fatalf("readHostConfigurationSource() error = %v", err)
	}
	if string(data) != "router id 192.0.2.1;\n" {
		t.Fatalf("content = %q", data)
	}
}
