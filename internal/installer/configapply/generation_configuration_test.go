package configapply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestGenerationConfigurationMaterializesInheritedInputs(t *testing.T) {
	root := t.TempDir()
	installed := manifestWithKubeadm()
	installed.Node.HostConfiguration = hostSysctlConfiguration("0")
	writeInstalledConfiguration(t, root, installed, "installed")
	writeKubeadmStateGeneration(t, root, "configured", "")
	writeKubeadmStateGeneration(t, root, "bootstrap", "configured")
	desired := manifestWithKubeadm()
	desired.Node.HostConfiguration = hostSysctlConfiguration("1")
	if err := WriteGenerationConfiguration(root, "configured", GenerationConfiguration{
		Manifest:       desired,
		KubeadmConfigs: map[string]kubeadmconfig.Plan{"control-plane": configurationKubeadm(t, "desired")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalGenerationConfiguration(root, "bootstrap"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inherited inputs counted as local: %v", err)
	}

	value, err := ReadGenerationConfiguration(root, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteGenerationConfiguration(root, "bootstrap", value); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "var/lib/katl/generations/configured")); err != nil {
		t.Fatal(err)
	}

	got, err := ReadLocalGenerationConfiguration(root, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if content := got.Manifest.Node.HostConfiguration.Sets["forwarding"].Files[0].Content; content == nil || *content != "net.ipv4.ip_forward = 1\n" {
		t.Fatalf("inherited host configuration was lost: %#v", content)
	}
	if content := string(got.KubeadmConfigs["control-plane"].Config.Content); !strings.Contains(content, "name: desired\n") {
		t.Fatalf("desired kubeadm input was replaced: %s", content)
	}
	if _, err := ReadGenerationConfiguration(root, "bootstrap"); err != nil {
		t.Fatalf("materialized configuration still depends on removed ancestor: %v", err)
	}
}

func TestGenerationConfigurationRejectsBrokenAncestry(t *testing.T) {
	for _, input := range []string{"manifest", "kubeadm"} {
		for _, failure := range []string{"missing", "cycle", "depth", "corrupt"} {
			t.Run(input+"/"+failure, func(t *testing.T) {
				root := t.TempDir()
				value := manifestWithKubeadm()
				writeInstalledConfiguration(t, root, value, "installed")
				previous := "missing"
				if failure == "cycle" {
					previous = "selected"
				}
				if failure == "depth" {
					previous = "ancestor-0"
					for i := 0; i < 256; i++ {
						writeKubeadmStateGeneration(t, root, fmt.Sprintf("ancestor-%d", i), fmt.Sprintf("ancestor-%d", i+1))
					}
				}
				writeKubeadmStateGeneration(t, root, "selected", previous)
				if input == "kubeadm" {
					if err := WriteGenerationManifest(root, "selected", value); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "corrupt" {
					path := filepath.Join(root, "var/lib/katl/generations/selected/manifest.json")
					if input == "kubeadm" {
						path = filepath.Join(root, "var/lib/katl/generations/selected/kubeadm-input/control-plane/config.yaml")
					}
					writeConfigurationFile(t, path, "[invalid")
				}

				_, err := ReadGenerationConfiguration(root, "selected")
				if err == nil || errors.Is(err, os.ErrNotExist) {
					t.Fatalf("broken %s ancestry permitted installation fallback: %v", input, err)
				}
			})
		}
	}
}

func TestGenerationConfigurationLegacyInputs(t *testing.T) {
	for _, source := range []string{"installed", "confext"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			writeKubeadmStateGeneration(t, root, "selected", "")
			writeInstalledConfiguration(t, root, manifestWithKubeadm(), "installed")
			if source == "confext" {
				writeConfigurationFile(t, filepath.Join(root, "var/lib/katl/generations/selected/confext/etc/katl/kubeadm/control-plane/config.yaml"), string(configurationKubeadm(t, "confext").Config.Content))
			}

			got, err := ReadGenerationConfiguration(root, "selected")
			if err != nil {
				t.Fatal(err)
			}
			if content := string(got.KubeadmConfigs["control-plane"].Config.Content); !strings.Contains(content, "name: "+source+"\n") {
				t.Fatalf("legacy input = %s, want %s", content, source)
			}
		})
	}
}

func TestLocalGenerationConfigurationRequiresSelectedInput(t *testing.T) {
	root := t.TempDir()
	if err := WriteGenerationManifest(root, "selected", manifestWithKubeadm()); err != nil {
		t.Fatal(err)
	}
	writeInstalledConfiguration(t, root, manifestWithKubeadm(), "installed")
	if _, err := ReadLocalGenerationConfiguration(root, "selected"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing selected local input = %v", err)
	}
	writeConfigurationFile(t, filepath.Join(root, "var/lib/katl/generations/selected/kubeadm-input/control-plane/config.yaml"), "[invalid")
	if _, err := ReadLocalGenerationConfiguration(root, "selected"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt selected local input = %v", err)
	}
}

func TestGenerationConfigurationSize(t *testing.T) {
	for _, withKubeadm := range []bool{false, true} {
		t.Run(fmt.Sprintf("kubeadm=%t", withKubeadm), func(t *testing.T) {
			value := GenerationConfiguration{Manifest: baseManifest()}
			value.Manifest.Node.HostConfiguration = hostSysctlConfiguration("1 # réseau <route>")
			if withKubeadm {
				value.Manifest.Node.Kubernetes.Kubeadm.ConfigRef = "control-plane"
				plan := configurationKubeadm(t, "desired")
				plan.Patches = []kubeadmconfig.File{{
					RenderPath: "/etc/katl/kubeadm/control-plane/patches/kube-apiserver+merge.yaml",
					Content:    []byte("metadata:\n  annotations:\n    katl.test/example: retained\n"),
					Mode:       0o600,
				}}
				value.KubeadmConfigs = map[string]kubeadmconfig.Plan{"control-plane": plan}
			}
			root := t.TempDir()
			size, err := value.SizeBytes()
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteGenerationConfiguration(root, "selected", value); err != nil {
				t.Fatal(err)
			}

			var written uint64
			if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				written += uint64(info.Size())
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if size != written || size == 0 {
				t.Fatalf("size = %d, independently observed file bytes = %d", size, written)
			}
		})
	}
}

func TestGenerationConfigurationSizeRejectsMissingInput(t *testing.T) {
	value := GenerationConfiguration{Manifest: manifestWithKubeadm()}
	if _, err := value.SizeBytes(); err == nil {
		t.Fatal("sized a configuration without its selected kubeadm input")
	}
}

func configurationKubeadm(t *testing.T, name string) kubeadmconfig.Plan {
	t.Helper()
	plan, err := kubeadmconfig.PlanFromRenderedFiles("control-plane", []kubeadmconfig.File{{
		RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml",
		Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nnodeRegistration:\n  name: " + name + "\n"),
		Mode:       0o644,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func writeInstalledConfiguration(t *testing.T, root string, value manifest.Manifest, kubeadmName string) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeConfigurationFile(t, filepath.Join(root, "var/lib/katl/install/manifest.json"), string(data))
	writeConfigurationFile(t, filepath.Join(root, "var/lib/katl/cluster/kubeadm/control-plane/config.yaml"), string(configurationKubeadm(t, kubeadmName).Config.Content))
}

func writeConfigurationFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
