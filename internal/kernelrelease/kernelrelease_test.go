package kernelrelease

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
)

func fixture(t *testing.T, flavour string) (Set, string) {
	t.Helper()
	set := Set{SchemaVersion: 1, FedoraRelease: "44", Architecture: "x86_64", Flavour: flavour, KernelRelease: "6.18.57-200.fc44.x86_64"}
	names, err := Names(flavour)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for i, name := range names {
		role := "runtime"
		if i == len(names)-1 {
			role = "build"
		}
		file := name + "-" + set.KernelRelease + ".rpm"
		data := []byte("retained " + name)
		blob := payloadbundle.DescribeBytes(data, role, RPMMediaType, file)
		set.Packages = append(set.Packages, Package{Name: name, NEVRA: name + "-0:" + set.KernelRelease, SourceURL: "https://removed.invalid/" + file, Descriptor: blob.Descriptor})
		if err := os.WriteFile(filepath.Join(directory, file), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return set, directory
}

func TestRetainedInputs(t *testing.T) {
	ctx := context.Background()
	set, source := fixture(t, "lts")
	layout := t.TempDir()
	lock, err := Export(ctx, set, source, layout, "registry.example/katl/kernel-inputs")
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := Export(ctx, set, source, t.TempDir(), "registry.example/katl/kernel-inputs")
	if err != nil || repeated.Reference != lock.Reference {
		t.Fatalf("repeat capture changed identity: %v", err)
	}
	fetched, err := fetch(ctx, lock, layout)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Manifest.Annotations["org.opencontainers.image.created"] != "1970-01-01T00:00:00Z" {
		t.Fatal("capture identity depends on wall-clock time")
	}
	// The upstream URLs do not exist and the original RPMs are gone. A fresh
	// build must be possible from the retained closure alone.
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	directory, err := Prepare(ctx, lock, cache, layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range set.Packages {
		actual, err := os.ReadFile(filepath.Join(directory, pkg.FileName))
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != "retained "+pkg.Name {
			t.Fatalf("unexpected retained bytes for %s", pkg.Name)
		}
	}
	// A verified cache remains sufficient when transport is unavailable.
	if err := os.RemoveAll(layout); err != nil {
		t.Fatal(err)
	}
	if repeated, err := Prepare(ctx, lock, cache, layout); err != nil || repeated != directory {
		t.Fatalf("repeat prepare = %q, %v", repeated, err)
	}
	path := filepath.Join(directory, set.Packages[0].FileName)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered "+set.Packages[0].Name), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(ctx, lock, cache, layout); err == nil {
		t.Fatal("corrupted RPM cache accepted")
	}
}

func TestKernelIdentity(t *testing.T) {
	set, directory := fixture(t, "standard")
	before, err := set.Digest()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, set.Packages[0].FileName)
	stamp := time.Unix(1, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	data := []byte("different kernel bytes")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	pkg := &set.Packages[0]
	pkg.Descriptor = payloadbundle.DescribeBytes(data, pkg.Role, RPMMediaType, pkg.FileName).Descriptor
	after, err := set.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("same-NEVRA package byte change retained its identity")
	}
	config, err := set.Config("runtime", directory)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config, "CacheKey=katl-runtime-kernel-v1-"+after) || strings.Contains(config, before) {
		t.Fatal("mkosi cache identity does not bind the changed RPM bytes")
	}
}

func TestKernelConfiguration(t *testing.T) {
	set, _ := fixture(t, "lts")
	for _, profile := range []string{"runtime", "installer-image"} {
		t.Run(profile, func(t *testing.T) {
			config, err := set.Config(profile, "/retained/rpms")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"core", "modules", "modules-core"} {
				if !strings.Contains(config, "/repository/kernel-longterm-"+name+"-6.18.57-200.fc44.x86_64.rpm") {
					t.Fatalf("missing explicit %s RPM", name)
				}
			}
			devel := "BuildPackages=/repository/kernel-longterm-devel-6.18.57-200.fc44.x86_64.rpm"
			if strings.Contains(config, devel) != (profile == "runtime") {
				t.Fatal("devel RPM must be confined to the runtime build overlay")
			}
			if strings.Contains(config, "kernel-headers") || strings.Contains(config, "Repositories=") {
				t.Fatal("kernel configuration changes userspace resolution")
			}
		})
	}
}

func TestKernelLock(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Set)
	}{
		{"missing-devel", func(set *Set) { set.Packages = set.Packages[:3] }},
		{"different-devel", func(set *Set) { set.Packages[3].NEVRA = "kernel-devel-0:7.2.9-200.fc44.x86_64" }},
		{"mixed-flavour", func(set *Set) { set.Flavour = "lts" }},
		{"different-fedora", func(set *Set) { set.FedoraRelease = "45" }},
		{"unsafe-path", func(set *Set) { set.Packages[0].FileName = "../kernel.rpm" }},
		{"missing-digest", func(set *Set) { set.Packages[0].Digest = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			set, _ := fixture(t, "standard")
			test.mutate(&set)
			if err := set.Validate(); err == nil {
				t.Fatal("invalid kernel inputs accepted")
			}
		})
	}
}

func TestRetainedLockMismatch(t *testing.T) {
	ctx := context.Background()
	set, source := fixture(t, "standard")
	layout := t.TempDir()
	lock, err := Export(ctx, set, source, layout, "registry.example/katl/kernel-inputs")
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the manifest pin while substituting another plausible lock.
	data, _ := json.Marshal(lock)
	var changed Lock
	if err := json.Unmarshal(data, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Packages[0].SourceURL = "https://another.invalid/kernel.rpm"
	if _, err := Prepare(ctx, changed, t.TempDir(), layout); err == nil {
		t.Fatal("retained artifact accepted with a different reviewed lock")
	}
	path := filepath.Join(layout, "blobs", "sha256", strings.TrimPrefix(set.Packages[0].Digest, "sha256:"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(ctx, lock, t.TempDir(), layout); err == nil {
		t.Fatal("incomplete retained closure accepted")
	}
}
