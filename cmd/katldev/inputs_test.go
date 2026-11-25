package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/vmtest"
)

func TestInstallerVMConfigBootInputs(t *testing.T) {
	world := katldevWorld{World: vmtest.World{Network: vmtest.WorldNetwork{Name: "katl-w", Gateway: "10.200.4.1"}}}
	node := vmtest.Node{Name: "cp-1", MACAddress: "52:54:00:00:00:01"}

	t.Run("no inputs boots the plain installer", func(t *testing.T) {
		config := installerVMConfig(world, node, defaultInstallerOptions(), "/iso", "")
		if len(config.Boot.CommandLine) != 0 || config.PreseedDir != "" {
			t.Fatalf("config boot = %+v preseed = %q, want neither", config.Boot, config.PreseedDir)
		}
	})

	t.Run("command line expands the world gateway", func(t *testing.T) {
		opts := defaultInstallerOptions()
		opts.CommandLine = []string{"katl.bundle.url=http://{gateway}:8080/c.katlcfg", "katl.install.mode=auto"}
		config := installerVMConfig(world, node, opts, "/iso", "")
		got := strings.Join(config.Boot.CommandLine, " ")
		if got != "katl.bundle.url=http://10.200.4.1:8080/c.katlcfg katl.install.mode=auto" {
			t.Fatalf("command line = %q", got)
		}
		if config.Boot.ISO != "/iso" || !config.Boot.DiskFirst {
			t.Fatalf("boot = %+v, want the ISO to stay the boot medium", config.Boot)
		}
	})

	t.Run("preseed directory is attached as media", func(t *testing.T) {
		opts := defaultInstallerOptions()
		opts.PreseedDir = t.TempDir()
		dir, cleanup, err := installerPreseedDir(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if config := installerVMConfig(world, node, opts, "/iso", dir); config.PreseedDir != opts.PreseedDir {
			t.Fatalf("PreseedDir = %q, want %q", config.PreseedDir, opts.PreseedDir)
		}
		if _, err := os.Stat(opts.PreseedDir); err != nil {
			t.Fatalf("cleanup removed the operator's directory: %v", err)
		}
	})
}

func TestInstallerPreseedDirFromManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "images", "katlos.img"), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(manifest, []byte(`{"katlosImage":{"localRef":"images/katlos.img"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := defaultInstallerOptions()
	opts.Manifest = manifest

	dir, cleanup, err := installerPreseedDir(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"install-input.json", "install-manifest.json", "images/katlos.img"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("preseed media lacks %s: %v", name, err)
		}
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("generated preseed directory survived cleanup: %v", err)
	}
}

func TestInstallerOptionsRejectBadBootInputs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "m.json")
	if err := os.WriteFile(file, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*installerOptions)
		want   string
	}{
		{"both", func(o *installerOptions) { o.PreseedDir, o.Manifest = t.TempDir(), file }, "mutually exclusive"},
		{"missing dir", func(o *installerOptions) { o.PreseedDir = filepath.Join(t.TempDir(), "none") }, "--preseed-dir"},
		{"dir is file", func(o *installerOptions) { o.PreseedDir = file }, "--preseed-dir"},
		{"missing manifest", func(o *installerOptions) { o.Manifest = filepath.Join(t.TempDir(), "none") }, "--manifest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := defaultInstallerOptions()
			test.mutate(&opts)
			if err := validateInstallerOptions(opts); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateInstallerOptions() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestServeDirectoryServesFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c.katlcfg"), []byte("bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveDirectory(ctx, listener, dir) }()

	response, err := http.Get("http://" + listener.Addr().String() + "/c.katlcfg")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "bundle" {
		t.Fatalf("body = %q", body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveDirectory() = %v after cancel", err)
	}
}
