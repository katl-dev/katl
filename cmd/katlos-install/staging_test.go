package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestParseMemInfo(t *testing.T) {
	const meminfo = "MemTotal:        1909452 kB\nMemFree:         1013396 kB\nMemAvailable:    1301744 kB\n"

	got, err := parseMemInfo(strings.NewReader(meminfo), "MemAvailable")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1301744*1024 {
		t.Fatalf("MemAvailable = %d bytes, want %d", got, 1301744*1024)
	}

	for _, malformed := range []string{"MemTotal: 1 kB\n", "MemAvailable: 12 MB\n", "MemAvailable: many kB\n"} {
		if _, err := parseMemInfo(strings.NewReader(malformed), "MemAvailable"); err == nil {
			t.Errorf("parseMemInfo(%q) succeeded, want an error", malformed)
		}
	}
}

func TestStagingAvailableBytesIsBoundedByMemory(t *testing.T) {
	meminfo := writeMemInfo(t, "MemTotal: 1048576 kB\nMemAvailable: 1024 kB\n")
	resolver := memoryStagedResolver{memInfo: meminfo}

	got, err := resolver.availableBytes(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got > 1<<20 {
		t.Fatalf("availableBytes = %d, want at most the 1 MiB of available memory", got)
	}
}

// The refusal happens before the body is stored and names the RAM the
// machine needs, never less than the documented minimum.
func TestMemoryStagedResolverRefusal(t *testing.T) {
	for _, test := range []struct {
		name                   string
		imageMiB, availableMiB uint64
		meminfo                string
		want                   []string
	}{{
		// 500 + 256 reserve against 600 available on a 1 GiB machine.
		name: "below minimum", imageMiB: 500, availableMiB: 600,
		meminfo: "MemTotal: 1048576 kB\nMemAvailable: 614400 kB\n",
		want:    []string{"needs 756 MiB free, but 600 MiB is available", "at least 2048 MiB of RAM", "no disk changes were made"},
	}, {
		// 3072 + 256 against 2048 available on a 4 GiB machine: 1280 MiB more.
		name: "above minimum", imageMiB: 3072, availableMiB: 2048,
		meminfo: "MemTotal: 4194304 kB\nMemAvailable: 2097152 kB\n",
		want:    []string{"needs 3328 MiB free, but 2048 MiB is available", "at least 5376 MiB of RAM"},
	}} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", strconv.FormatUint(test.imageMiB<<20, 10))
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			resolver := newMemoryStagedResolver(katlosimage.Resolver{Commands: installer.NewExecCommandRunner()})
			resolver.memInfo = writeMemInfo(t, test.meminfo)
			resolver.WorkDir = t.TempDir()
			resolver.AvailableBytes = func(string) (uint64, error) { return test.availableMiB << 20, nil }
			_, err := resolver.ResolveKatlosImage(context.Background(), manifest.KatlosImage{URL: server.URL + "/katlos.squashfs"})

			var short *katlosimage.InsufficientStorageError
			if !errors.As(err, &short) {
				t.Fatalf("ResolveKatlosImage() error = %v, want a staging refusal", err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
			downloads, _ := filepath.Glob(filepath.Join(resolver.WorkDir, "acquisitions", "*", "downloads", "*"))
			if len(downloads) > 0 {
				t.Fatalf("refusal left staged downloads: %v", downloads)
			}
		})
	}
}

func writeMemInfo(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
