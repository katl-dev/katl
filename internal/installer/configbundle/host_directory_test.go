package configbundle

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDirectoryBundle(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "network", "10-extra.network.d", "50-address.conf"), "[Network]\nAddress=192.0.2.10/24\n")
	writeFile(t, filepath.Join(root, "network", "10-extra.network"), "[Match]\nName=extra0\n[Network]\nDHCP=no\n")
	config := strings.Replace(validSourceConfig(), "      fileSets:\n", "      fileSets:\n        extra-network:\n          directory: network\n          destination: /etc/systemd/network\n", 1)
	sourcePath := filepath.Join(root, "cluster.yaml")
	writeFile(t, sourcePath, config)
	if err := ValidateSourceFile(sourcePath); err != nil {
		t.Fatal(err)
	}

	archive, initial, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range selected.NodeMaterial.NativeEtcFiles {
		if strings.Contains(file.Path, "10-extra.network") {
			got[file.Path] = file.Content
		}
	}
	want := map[string]string{
		"/etc/systemd/network/10-extra.network":                   "[Match]\nName=extra0\n[Network]\nDHCP=no\n",
		"/etc/systemd/network/10-extra.network.d/50-address.conf": "[Network]\nAddress=192.0.2.10/24\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("native files = %#v, want %#v", got, want)
	}
	set := selected.NodeMaterial.InstallManifest.Node.HostConfiguration.Sets["extra-network"]
	for _, file := range set.Files {
		if file.Mode != 0o644 || file.Source != "" {
			t.Fatalf("file is not self-contained with fixed permissions: %#v", file)
		}
	}

	// Workstation permissions must not change the installed configuration.
	if err := os.Chmod(filepath.Join(root, "network", "10-extra.network"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, repeated, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Manifest.Source.SourceDigest != initial.Manifest.Source.SourceDigest {
		t.Fatal("source digest depends on workstation permissions")
	}

	if err := os.Remove(filepath.Join(root, "network", "10-extra.network.d", "50-address.conf")); err != nil {
		t.Fatal(err)
	}
	archive, removed, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	if removed.Manifest.Source.SourceDigest == initial.Manifest.Source.SourceDigest {
		t.Fatal("removing a file did not change desired configuration")
	}
	selected, err = ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatal(err)
	}
	files := selected.NodeMaterial.InstallManifest.Node.HostConfiguration.Sets["extra-network"].Files
	if len(files) != 1 || files[0].Path != "/etc/systemd/network/10-extra.network" {
		t.Fatalf("removed file remains in bundle: %#v", files)
	}
}

func TestDirectoryValidation(t *testing.T) {
	for _, tc := range []struct{ name, declaration, want string }{
		{"missing destination", "directory: network", "specified together"},
		{"missing directory", "destination: /etc/systemd/network", "specified together"},
		{"relative destination", "directory: network\n          destination: etc/systemd/network", "absolute path"},
		{"unclean destination", "directory: network\n          destination: /etc/systemd/../network", "normalized"},
		{"absent", "directory: network\n          destination: /etc/systemd/network\n          state: absent", "state present"},
		{"mixed", "directory: network\n          destination: /etc/systemd/network\n          files:\n            - path: /etc/example\n              content: test", "either directory or files"},
		{"escape", "directory: ../network\n          destination: /etc/systemd/network", "non-escaping"},
		{"missing", "directory: missing\n          destination: /etc/systemd/network", "no such file"},
		{"not directory", "directory: network/10-common.network\n          destination: /etc/systemd/network", "must be a directory"},
		{"empty", "directory: empty\n          destination: /etc/systemd/network", "contains no files"},
		{"collision", "directory: network\n          destination: /etc/systemd/network", "already owned"},
		{"reserved path", "directory: network\n          destination: /etc/tmpfiles.d", "Katl-owned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "network", "10-common.network"), "[Match]\nName=extra0\n[Network]\nDHCP=no\n")
			if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
			config := strings.Replace(validSourceConfig(), "      fileSets:\n", "      fileSets:\n        extra:\n          "+tc.declaration+"\n", 1)
			sourcePath := filepath.Join(root, "cluster.yaml")
			writeFile(t, sourcePath, config)
			for _, check := range []struct {
				name string
				run  func() error
			}{
				{"validate", func() error { return ValidateSourceFile(sourcePath) }},
				{"build", func() error { _, _, err := BuildArchive(BuildRequest{SourcePath: sourcePath}); return err }},
			} {
				t.Run(check.name, func(t *testing.T) {
					if err := check.run(); err == nil || !strings.Contains(err.Error(), tc.want) {
						t.Fatalf("error = %v, want %q", err, tc.want)
					}
				})
			}
		})
	}
}

func TestDirectorySymlinks(t *testing.T) {
	for _, name := range []string{"root", "ancestor", "file", "subdirectory"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "real", "10-test.network"), "[Match]\nName=test0\n")
			directory := "network"
			switch name {
			case "root":
				if err := os.Symlink("real", filepath.Join(root, "network")); err != nil {
					t.Fatal(err)
				}
			case "ancestor":
				if err := os.Symlink(".", filepath.Join(root, "network")); err != nil {
					t.Fatal(err)
				}
				directory = "network/real"
			default:
				if err := os.Mkdir(filepath.Join(root, "network"), 0o755); err != nil {
					t.Fatal(err)
				}
				target := "../real"
				if name == "file" {
					target += "/10-test.network"
				}
				if err := os.Symlink(target, filepath.Join(root, "network", "link")); err != nil {
					t.Fatal(err)
				}
			}
			config := strings.Replace(validSourceConfig(), "      fileSets:\n", "      fileSets:\n        extra:\n          directory: "+directory+"\n          destination: /etc/systemd/network\n", 1)
			sourcePath := filepath.Join(root, "cluster.yaml")
			writeFile(t, sourcePath, config)
			if err := ValidateSourceFile(sourcePath); err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("error = %v, want symlink rejection", err)
			}
		})
	}
}

func TestDirectoryInheritance(t *testing.T) {
	for _, tc := range []struct{ name, nodeSet, path, content string }{
		{"inherited", "", "/etc/modules-load.d/shared.conf", "br_netfilter\n"},
		{"replaced", "directory: node\n            destination: /etc/modules-load.d", "/etc/modules-load.d/node.conf", "tcp_bbr\n"},
		{"absent", "state: absent", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "shared", "shared.conf"), "br_netfilter\n")
			writeFile(t, filepath.Join(root, "node", "node.conf"), "tcp_bbr\n")
			config := strings.Replace(validSourceConfig(), "      fileSets:\n", "      fileSets:\n        modules:\n          directory: shared\n          destination: /etc/modules-load.d\n", 1)
			if tc.nodeSet != "" {
				config = strings.Replace(config, "    - name: cp-1\n", "    - name: cp-1\n      hostConfiguration:\n        fileSets:\n          modules:\n            "+tc.nodeSet+"\n", 1)
			}
			sourcePath := filepath.Join(root, "cluster.yaml")
			writeFile(t, sourcePath, config)
			archive, _, err := BuildArchive(BuildRequest{SourcePath: sourcePath})
			if err != nil {
				t.Fatal(err)
			}
			selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, file := range selected.NodeMaterial.NativeEtcFiles {
				if strings.HasPrefix(file.Path, "/etc/modules-load.d/") {
					got[file.Path] = file.Content
				}
			}
			want := map[string]string{}
			if tc.path != "" {
				want[tc.path] = tc.content
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("module files = %#v, want %#v", got, want)
			}
		})
	}
}
