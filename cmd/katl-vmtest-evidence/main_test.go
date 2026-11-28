package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactEvidencePEM(t *testing.T) {
	for _, label := range []string{"RSA PRIVATE KEY", "EC PRIVATE KEY", "OPENSSH PRIVATE KEY", "PRIVATE KEY", "ENCRYPTED PRIVATE KEY"} {
		t.Run(label, func(t *testing.T) {
			key := "-----BEGIN " + label + "-----\nsynthetic-private-data\n-----END " + label + "-----"
			plain := "before\n" + key + "\nbetween\n" + key + "\nafter\n"
			if got, want := redactEvidence(plain), "before\n[REDACTED PRIVATE KEY]\nbetween\n[REDACTED PRIVATE KEY]\nafter\n"; got != want {
				t.Fatalf("plain text = %q, want %q", got, want)
			}

			quoted, err := json.Marshal("before\n" + key + "\nafter")
			if err != nil {
				t.Fatal(err)
			}
			input := `{"output":` + string(quoted) + `,"certificate":"-----BEGIN CERTIFICATE-----\npublic\n-----END CERTIFICATE-----"}`
			want := `{"output":"before\n[REDACTED PRIVATE KEY]\nafter","certificate":"-----BEGIN CERTIFICATE-----\npublic\n-----END CERTIFICATE-----"}`
			if got := redactEvidence(input); got != want {
				t.Fatalf("JSON = %q, want %q", got, want)
			}
		})
	}
}

func TestRedactEvidenceJSONFields(t *testing.T) {
	input := `{"management":{"serverPrivateKey": "escaped \"quote\" and \\ slash"}, "nodes":[{"privateKey":"raw","clientPrivateKey":"raw","server_private_key":"raw","client-key-data":"raw"}],"serverPrivateKeyPath":"/path/to/key"}`
	want := `{"management":{"serverPrivateKey": "[REDACTED PRIVATE KEY]"}, "nodes":[{"privateKey":"[REDACTED PRIVATE KEY]","clientPrivateKey":"[REDACTED PRIVATE KEY]","server_private_key":"[REDACTED PRIVATE KEY]","client-key-data":"[REDACTED PRIVATE KEY]"}],"serverPrivateKeyPath":"/path/to/key"}`
	if got := redactEvidence(input); got != want {
		t.Fatalf("JSON = %q, want %q", got, want)
	}
}

func TestCollectEvidence(t *testing.T) {
	root := t.TempDir()
	const key = "-----BEGIN PRIVATE KEY-----\nsynthetic-private-data\n-----END PRIVATE KEY-----"
	const manifest = `production-vmtest/scenarios/install/manifests/install-manifest.json`
	sources := map[string]string{
		manifest: `{"management":{"serverPrivateKey":"-----BEGIN PRIVATE KEY-----\nsynthetic-private-data\n-----END PRIVATE KEY-----"},"node":"vm"}`,
		"production-vmtest/install-network-evidence/journal": "network diagnostic\n" + key,
		"production-vmtest/guest.qcow2":                      "VM disk data",
		"hosted-evidence/qemu.log":                           "QEMU diagnostic\n" + key,
		"hosted-evidence/source.txt":                         "build revision\n",
	}
	for name, content := range sources {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, manifest), filepath.Join(root, "hosted-evidence", "symlink.json")); err != nil {
		t.Fatal(err)
	}

	upload := filepath.Join(root, "upload")
	if err := collectEvidence(filepath.Join(root, "hosted-evidence"), filepath.Join(root, "production-vmtest"), upload); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		manifest: `{"management":{"serverPrivateKey":"[REDACTED PRIVATE KEY]"},"node":"vm"}`,
		"production-vmtest/install-network-evidence/journal": "network diagnostic\n[REDACTED PRIVATE KEY]",
		"hosted-evidence/qemu.log":                           "QEMU diagnostic\n[REDACTED PRIVATE KEY]",
		"hosted-evidence/source.txt":                         "build revision\n",
	}
	if err := fs.WalkDir(os.DirFS(upload), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(filepath.Join(upload, path))
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "PRIVATE KEY-----") {
			t.Errorf("retained evidence %s contains a PEM private key", path)
		}
		expected, ok := want[path]
		if !ok || string(data) != expected {
			t.Errorf("unexpected retained evidence at %s", path)
		}
		delete(want, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(want) != 0 {
		t.Errorf("missing evidence: %v", want)
	}

	for path, expected := range sources {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(data) != expected {
			t.Errorf("live input changed at %s, read error: %v", path, err)
		}
	}
}
