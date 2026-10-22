package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeInventory(t *testing.T) {
	sources := map[string]source{"containerd": {Version: "v2.3.7"}, "crun": {Version: "1.30.1"}}
	for _, tc := range []struct {
		name, inventory string
		valid           bool
	}{
		{"katl packages", "containerd\t0:2.3.7-1.katl.fc44.x86_64\ncrun\t0:1.30.1-1.katl.fc44.x86_64\n", true},
		{"Fedora containerd", "containerd\t0:2.3.7-1.fc44.x86_64\ncrun\t0:1.30.1-1.katl.fc44.x86_64\n", false},
		{"old crun", "containerd\t0:2.3.7-1.katl.fc44.x86_64\ncrun\t0:1.28-1.katl.fc44.x86_64\n", false},
		{"missing crun", "containerd\t0:2.3.7-1.katl.fc44.x86_64\n", false},
		{"duplicate crun", "containerd\t0:2.3.7-1.katl.fc44.x86_64\ncrun\t0:1.30.1-1.katl.fc44.x86_64\ncrun\t0:1.30.1-1.katl.fc44.x86_64\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "packages.tsv")
			if err := os.WriteFile(path, []byte(tc.inventory), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyInventory(path, sources, "44"); (err == nil) != tc.valid {
				t.Fatalf("verifyInventory = %v; valid = %v", err, tc.valid)
			}
		})
	}
}

func TestSourcePins(t *testing.T) {
	for _, version := range []string{"1.30", "1.30.1"} {
		t.Run(version, func(t *testing.T) {
			data, err := json.Marshal(source{Version: version, SHA256: strings.Repeat("a", 64)})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "source.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readSource(path, "crun"); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, tc := range []struct {
		name, pkg string
		source    source
	}{
		{"missing commit", "containerd", source{Version: "v2.3.7", GoVersion: "1.26.9"}},
		{"missing toolchain", "containerd", source{Version: "v2.3.7", Revision: strings.Repeat("a", 40)}},
		{"missing checksum", "crun", source{Version: "1.30.1"}},
		{"incomplete containerd version", "containerd", source{Version: "v2.3", Revision: strings.Repeat("a", 40), GoVersion: "1.26.9"}},
		{"crun prerelease", "crun", source{Version: "1.31-rc.1", SHA256: strings.Repeat("a", 64)}},
		{"release candidate", "crun", source{Version: "1.31.0-rc.1", SHA256: strings.Repeat("a", 64)}},
		{"RPM macro", "crun", source{Version: "%{lua:os.execute('false')}", SHA256: strings.Repeat("a", 64)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.source)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "source.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readSource(path, tc.pkg); err == nil {
				t.Fatal("accepted an invalid source pin")
			}
		})
	}
}

func TestSourceDownload(t *testing.T) {
	const abcSHA256 = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"verified", "abc", http.StatusOK, true},
		{"corrupt", "abd", http.StatusOK, false},
		{"not found", "abc", http.StatusNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "source.tar.zst")
			if err := download(server.URL, path, abcSHA256); (err == nil) != tc.valid {
				t.Fatalf("download = %v; valid = %v", err, tc.valid)
			}
			if tc.valid {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "abc" {
					t.Fatalf("downloaded %q, %v", data, err)
				}
			}
		})
	}
}

func TestRPMCacheIntegrity(t *testing.T) {
	directory := t.TempDir()
	record := buildRecord{Identity: "builder-and-recipe", Packages: map[string]string{
		"containerd.rpm": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		"crun.rpm":       "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	}}
	for name := range record.Packages {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("abc"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "build.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if !validCache(directory, record.Identity) {
		t.Fatal("rejected intact cached RPMs")
	}
	if validCache(directory, "different-builder") {
		t.Fatal("accepted RPMs from a different build")
	}
	if err := os.WriteFile(filepath.Join(directory, "crun.rpm"), []byte("abd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if validCache(directory, record.Identity) {
		t.Fatal("accepted a corrupted cached RPM")
	}
}
