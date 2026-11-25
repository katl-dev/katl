package vmtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWritePreseedDir(t *testing.T) {
	root := t.TempDir()
	manifest := []byte(`{"katlosImage":{"localRef":"images/katlos.img"}}`)
	for path, content := range map[string]string{
		"images/katlos.img":                 "image",
		"kubeadm/etc/note.txt": "file",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("manifest install carries its local image", func(t *testing.T) {
		dir := t.TempDir()
		if err := WritePreseedDir(dir, PreseedInput{Manifest: manifest, ManifestRoot: root}); err != nil {
			t.Fatal(err)
		}
		var input map[string]string
		data, err := os.ReadFile(filepath.Join(dir, "install-input.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &input); err != nil {
			t.Fatal(err)
		}
		if input["installMode"] != "auto" || input["manifestPath"] != "/run/katl/preseed/install-manifest.json" {
			t.Fatalf("install-input.json = %v", input)
		}
		for path, want := range map[string]string{
			"install-manifest.json":             string(manifest),
			"images/katlos.img":                 "image",
			"kubeadm/etc/note.txt": "file",
		} {
			got, err := os.ReadFile(filepath.Join(dir, path))
			if err != nil || string(got) != want {
				t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
			}
		}
	})

	t.Run("bundle install needs a node and omits the manifest", func(t *testing.T) {
		bundle := filepath.Join(root, "c.katlcfg")
		if err := os.WriteFile(bundle, []byte("bundle"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := WritePreseedDir(t.TempDir(), PreseedInput{ConfigBundle: bundle}); err == nil {
			t.Fatal("WritePreseedDir() accepted a bundle without a node")
		}
		dir := t.TempDir()
		if err := WritePreseedDir(dir, PreseedInput{ConfigBundle: bundle, SelectedNode: "cp-1", ManifestRoot: root}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "config.katlcfg")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "install-manifest.json")); err == nil {
			t.Fatal("bundle preseed contains a manifest")
		}
	})

	t.Run("localRef escaping the manifest directory is refused", func(t *testing.T) {
		bad := []byte(`{"katlosImage":{"localRef":"../escape.img"}}`)
		if err := WritePreseedDir(t.TempDir(), PreseedInput{Manifest: bad, ManifestRoot: root}); err == nil {
			t.Fatal("WritePreseedDir() accepted an escaping localRef")
		}
	})
}
