package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/kernelrelease"
)

func capturedLock(t *testing.T, flavour string) (kernelrelease.Lock, string) {
	t.Helper()
	set := kernelrelease.Set{SchemaVersion: 1, FedoraRelease: "44", Architecture: "x86_64", Flavour: flavour, KernelRelease: "6.18.57-200.fc44.x86_64"}
	names, err := kernelrelease.Names(flavour)
	if err != nil {
		t.Fatal(err)
	}
	rpms := t.TempDir()
	for i, name := range names {
		file := name + "-" + set.KernelRelease + ".rpm"
		data := []byte("test " + name)
		role := "runtime"
		if i == len(names)-1 {
			role = "build"
		}
		blob := payloadbundle.DescribeBytes(data, role, kernelrelease.RPMMediaType, file)
		set.Packages = append(set.Packages, kernelrelease.Package{Name: name, NEVRA: name + "-0:" + set.KernelRelease, SourceURL: "https://source.invalid/" + file, Descriptor: blob.Descriptor})
		if err := os.WriteFile(filepath.Join(rpms, file), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	layout := t.TempDir()
	lock, err := kernelrelease.Export(context.Background(), set, rpms, layout, "registry.example/kernel-inputs")
	if err != nil {
		t.Fatal(err)
	}
	return lock, layout
}

func TestPrepareSelection(t *testing.T) {
	lock, layout := capturedLock(t, "lts")
	path := filepath.Join(t.TempDir(), "lock.json")
	if err := kernelrelease.Write(path, lock); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, flavour, fedora string }{
		{"wrong-flavour", "standard", "44"},
		{"wrong-fedora", "lts", "45"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := run(context.Background(), []string{"prepare", "--lock", path, "--layout", layout, "--flavour", test.flavour, "--fedora-release", test.fedora, "--cache", t.TempDir()}, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("mismatched selection = %v", err)
			}
		})
	}
	config := filepath.Join(t.TempDir(), "kernel.conf")
	var stdout bytes.Buffer
	err := run(context.Background(), []string{"prepare", "--lock", path, "--layout", layout, "--flavour", "lts", "--fedora-release", "44", "--cache", t.TempDir(), "--config", config, "--visible-directory", "/builder/kernel-rpms"}, &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Directory, Identity, KernelRelease string }
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.KernelRelease != "6.18.57-200.fc44.x86_64" {
		t.Fatalf("selected kernel = %q", result.KernelRelease)
	}
	contents, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "PackageDirectories=/builder/kernel-rpms/"+result.Identity) {
		t.Fatal("configuration does not use the verified set")
	}
	for _, pkg := range lock.Packages {
		actual, err := os.ReadFile(filepath.Join(result.Directory, pkg.FileName))
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != "test "+pkg.Name {
			t.Fatalf("unexpected prepared %s", pkg.Name)
		}
	}
}

func TestProposalUnchanged(t *testing.T) {
	t.Chdir(t.TempDir())
	candidates := filepath.Join(t.TempDir(), "candidates")
	for _, flavour := range []string{"standard", "lts"} {
		lock, _ := capturedLock(t, flavour)
		if err := kernelrelease.Write(filepath.Join(candidates, flavour, "lock.json"), lock); err != nil {
			t.Fatal(err)
		}
		if err := kernelrelease.Write(filepath.Join("kernels/fedora-44/x86_64", flavour+".json"), lock); err != nil {
			t.Fatal(err)
		}
	}
	// An unchanged dependency selection must not contact GitHub or create a new
	// branch merely because discovery ran again.
	t.Setenv("PATH", t.TempDir())
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"propose", "--candidates", candidates, "--repository", "example/katl", "--base", strings.Repeat("a", 40)}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != `{"changed":false}` {
		t.Fatalf("unchanged result = %s", stdout.Bytes())
	}
}

func TestCandidateArguments(t *testing.T) {
	for _, args := range [][]string{
		{"candidate", "--fedora-release", "../../44"},
		{"candidate", "--fedora-release", "44", "--kernel-release", "../../kernel"},
		{"candidate", "--fedora-release", "44", "--architecture", "aarch64"},
	} {
		if err := run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("invalid candidate arguments accepted: %v", args)
		}
	}
}

func TestProposalResume(t *testing.T) {
	t.Chdir(t.TempDir())
	candidates := filepath.Join(t.TempDir(), "candidates")
	locks := map[string]kernelrelease.Lock{}
	for _, flavour := range []string{"standard", "lts"} {
		lock, _ := capturedLock(t, flavour)
		locks[flavour] = lock
		if err := kernelrelease.Write(filepath.Join(candidates, flavour, "lock.json"), lock); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(lock)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := json.Marshal(map[string]string{"content": base64.StdEncoding.EncodeToString(data)})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("FAKE_"+strings.ToUpper(flavour), string(reply))
	}
	bin := t.TempDir()
	merged := filepath.Join(t.TempDir(), "merged")
	request := filepath.Join(t.TempDir(), "merge-request")
	t.Setenv("FAKE_MERGED", merged)
	t.Setenv("FAKE_MERGE_REQUEST", request)
	// An existing proposal may merge main, but must not create another commit,
	// replace its ref, or force-update history. Return different pre/post-merge
	// heads so qualification cannot accidentally use stale evidence.
	script := `#!/usr/bin/env bash
set -eu
case "$3:$4" in
 GET:*/git/ref/heads/automation/kernels-*)
   head=before-merge
   [[ ! -e "$FAKE_MERGED" ]] || head=after-merge
   printf '{"object":{"sha":"%s"}}' "$head"
   ;;
 POST:*/merges)
   cat > "$FAKE_MERGE_REQUEST"
   : > "$FAKE_MERGED"
   printf '{"sha":"after-merge"}'
   ;;
 GET:*/contents/*/standard.json?ref=after-merge) printf '%s' "$FAKE_STANDARD" ;;
 GET:*/contents/*/lts.json?ref=after-merge) printf '%s' "$FAKE_LTS" ;;
 *) echo 'unexpected GitHub mutation or stale proposal head' >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	args := []string{"propose", "--candidates", candidates, "--repository", "example/katl", "--base", strings.Repeat("a", 40)}
	var stdout bytes.Buffer
	if err := run(context.Background(), args, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result struct{ Branch, Commit string }
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Commit != "after-merge" {
		t.Fatalf("qualification commit = %q", result.Commit)
	}
	data, err := os.ReadFile(request)
	if err != nil {
		t.Fatal(err)
	}
	var merge struct{ Base, Head string }
	if err := json.Unmarshal(data, &merge); err != nil {
		t.Fatal(err)
	}
	if merge.Base != result.Branch || merge.Head != strings.Repeat("a", 40) {
		t.Fatal("proposal did not merge the selected main commit into its existing branch")
	}

	changed := locks["standard"]
	changed.Packages[0].Digest = "sha256:" + strings.Repeat("b", 64)
	data, err = json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := json.Marshal(map[string]string{"content": base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_STANDARD", string(reply))
	if err := run(context.Background(), args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "different kernel inputs") {
		t.Fatalf("changed existing proposal accepted: %v", err)
	}
}

func TestDiscoveryVersionGuard(t *testing.T) {
	lock, _ := capturedLock(t, "lts")
	path := filepath.Join(t.TempDir(), "lock.json")
	if err := kernelrelease.Write(path, lock); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		comparison string
		reject     bool
	}{{"-1", true}, {"0", false}, {"1", false}, {"invalid", true}} {
		t.Run(test.comparison, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "rpm"), []byte("#!/bin/sh\nprintf '%s\\n' '"+test.comparison+"'\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			err := rejectKernelDowngrade(context.Background(), path, "6.18.58-200.fc44.x86_64")
			if (err != nil) != test.reject {
				t.Fatalf("RPM comparison %s: %v", test.comparison, err)
			}
		})
	}
}

func TestRPMVersionOrdering(t *testing.T) {
	if _, err := exec.LookPath("rpm"); err != nil {
		t.Skip("run inside scripts/mkosi box to exercise native RPM ordering")
	}
	lock, _ := capturedLock(t, "lts")
	path := filepath.Join(t.TempDir(), "lock.json")
	if err := kernelrelease.Write(path, lock); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		release string
		older   bool
	}{
		{"6.18.9-200.fc44.x86_64", true},
		{"6.18.57-200.fc44.x86_64", false},
		{"6.18.100-200.fc44.x86_64", false},
	} {
		err := rejectKernelDowngrade(context.Background(), path, test.release)
		if test.older {
			if err == nil || !strings.Contains(err.Error(), "older than committed") {
				t.Fatalf("accepted automatic downgrade %s: %v", test.release, err)
			}
		} else if err != nil {
			t.Fatalf("rejected %s: %v", test.release, err)
		}
	}
}
