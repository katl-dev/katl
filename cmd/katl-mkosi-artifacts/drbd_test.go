package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestKernelBuildDirectory(t *testing.T) {
	repo := t.TempDir()
	directory := t.TempDir()
	cfg, err := configFromEnv(map[string]string{"KATL_MKOSI_BUILD_DIR": directory}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeRoot != filepath.Join(directory, "katl-runtime-root.squashfs") || cfg.RuntimeUKI != filepath.Join(directory, "katl-runtime.efi") {
		t.Fatalf("runtime input paths did not honor the build directory: %s, %s", cfg.RuntimeRoot, cfg.RuntimeUKI)
	}
}

func TestPreparedKernelInputs(t *testing.T) {
	for _, test := range []struct {
		name    string
		release string
		missing string
	}{
		{
			name:    "matching inputs",
			release: "6.12.1-katl",
		},
		{
			name:    "different kernel",
			release: "6.12.2-katl",
		},
		{
			name:    "missing symbol versions",
			release: "6.12.1-katl",
			missing: "Module.symvers",
		},
		{
			name:    "unprepared headers",
			release: "6.12.1-katl",
			missing: "include/generated/autoconf.h",
		},
		{
			name:    "missing configuration",
			release: "6.12.1-katl",
			missing: ".config",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			for path, content := range map[string]string{
				"include/config/kernel.release": "6.12.1-katl\n",
				"include/generated/autoconf.h":  "#define CONFIG_MODVERSIONS 1\n",
				"Module.symvers":                "0x12345678\tmodule_layout\tvmlinux\tEXPORT_SYMBOL\n",
				".config":                       "CONFIG_MODVERSIONS=y\n",
			} {
				if path == test.missing {
					content = ""
				}
				full := filepath.Join(directory, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			err := validateKernelInputs(directory, test.release)
			valid := test.missing == "" && test.release == "6.12.1-katl"
			if (err == nil) != valid {
				t.Fatalf("validate inputs = %v, valid = %t", err, valid)
			}
		})
	}
}

func TestKernelSourceIntegrity(t *testing.T) {
	payload := "verified source archive"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, payload)
	}))
	defer server.Close()
	source := kernelSource{
		URL:    server.URL,
		SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(payload))),
	}
	destination := filepath.Join(t.TempDir(), "source.tar.gz")
	if err := os.WriteFile(destination, []byte("corrupt cached source"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := acquireKernelSource(server.Client(), source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "verified source archive" {
		t.Fatalf("cached source = %q, %v", data, err)
	}
	if err := acquireKernelSource(server.Client(), source, destination); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("verified cache requested source %d times", got)
	}

	source.SHA256 = strings.Repeat("a", 64)
	if err := acquireKernelSource(server.Client(), source, destination); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("unverified replacement = %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("checksum mismatch made %d new requests, want 1", got-1)
	}
	data, err = os.ReadFile(destination)
	if err != nil || string(data) != "verified source archive" {
		t.Fatalf("failed acquisition replaced verified source: %q, %v", data, err)
	}
}

func TestKernelSourceRetry(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		wantCalls int32
	}{
		{name: "timeout", wantCalls: 2},
		{name: "server error", status: http.StatusServiceUnavailable, wantCalls: 2},
		{name: "client error", status: http.StatusNotFound, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			const payload = "verified source archive"
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					if test.status == 0 {
						<-r.Context().Done()
					} else {
						w.WriteHeader(test.status)
					}
					return
				}
				fmt.Fprint(w, payload)
			}))
			defer server.Close()
			client := server.Client()
			client.Timeout = time.Second
			source := kernelSource{
				URL:    server.URL,
				SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(payload))),
			}
			destination := filepath.Join(t.TempDir(), "source.tar.gz")

			err := acquireKernelSource(client, source, destination)
			if test.status == http.StatusNotFound {
				if err == nil || !strings.Contains(err.Error(), "404") {
					t.Fatalf("fetch = %v, want 404", err)
				}
				if _, err := os.Stat(destination); !os.IsNotExist(err) {
					t.Fatalf("unavailable source was cached: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(destination)
				if err != nil || string(data) != payload {
					t.Fatalf("cached source = %q, %v", data, err)
				}
			}
			if got := requests.Load(); got != test.wantCalls {
				t.Fatalf("made %d requests, want %d", got, test.wantCalls)
			}
		})
	}
}
