package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/kernelrelease"
)

func TestProposalFlavour(t *testing.T) {
	for _, flavour := range []string{"standard", "lts"} {
		t.Run(flavour, func(t *testing.T) {
			t.Chdir(t.TempDir())
			candidates := t.TempDir()
			lock, _ := capturedLock(t, flavour)
			if err := kernelrelease.Write(filepath.Join(candidates, flavour, "lock.json"), lock); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			requests := t.TempDir()
			t.Setenv("FAKE_REQUESTS", requests)
			// Inspect the proposed tree independently of the command's success
			// signal. Any API call outside proposal creation fails the test.
			script := `#!/usr/bin/env bash
set -euo pipefail
case "$3:$4" in
 GET:*/git/ref/heads/automation/kernels-*) echo 'HTTP 404' >&2; exit 1 ;;
 GET:*/git/commits/*) printf '{"tree":{"sha":"base-tree"}}' ;;
 POST:*/git/trees) cat > "$FAKE_REQUESTS/tree"; printf '{"sha":"new-tree"}' ;;
 POST:*/git/commits) cat > "$FAKE_REQUESTS/commit"; printf '{"sha":"new-commit"}' ;;
 POST:*/git/refs) cat > "$FAKE_REQUESTS/ref" ;;
 *) echo 'unexpected GitHub operation' >&2; exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			args := []string{"propose", "--flavour", flavour, "--candidates", candidates, "--repository", "example/katl", "--base", strings.Repeat("a", 40)}
			var stdout bytes.Buffer
			if err := run(context.Background(), args, &stdout, io.Discard); err != nil {
				t.Fatal(err)
			}
			type proposal struct {
				Changed        bool
				Branch, Commit string
			}
			var result proposal
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Changed || result.Commit != "new-commit" || !strings.HasPrefix(result.Branch, "automation/kernels-"+flavour+"-") {
				t.Fatalf("proposal = %+v", result)
			}

			data, err := os.ReadFile(filepath.Join(requests, "tree"))
			if err != nil {
				t.Fatal(err)
			}
			var tree struct {
				BaseTree string `json:"base_tree"`
				Tree     []struct{ Path, Content string }
			}
			if err := json.Unmarshal(data, &tree); err != nil {
				t.Fatal(err)
			}
			if tree.BaseTree != "base-tree" || len(tree.Tree) != 1 || tree.Tree[0].Path != "kernels/fedora-44/x86_64/"+flavour+".json" {
				t.Fatalf("proposal changes more than the selected flavour: %s", data)
			}
			var proposed kernelrelease.Lock
			if err := json.Unmarshal([]byte(tree.Tree[0].Content), &proposed); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(proposed, lock) {
				t.Fatalf("proposed lock = %+v", proposed)
			}

			data, err = os.ReadFile(filepath.Join(requests, "ref"))
			if err != nil {
				t.Fatal(err)
			}
			var ref struct{ Ref, SHA string }
			if err := json.Unmarshal(data, &ref); err != nil {
				t.Fatal(err)
			}
			if ref.Ref != "refs/heads/"+result.Branch || ref.SHA != "new-commit" {
				t.Fatalf("published ref = %+v", ref)
			}

			data, err = os.ReadFile(filepath.Join(requests, "commit"))
			if err != nil {
				t.Fatal(err)
			}
			var commit struct{ Message string }
			if err := json.Unmarshal(data, &commit); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(commit.Message, "kernel: update retained "+flavour+" kernel inputs\n") {
				t.Fatalf("commit message = %q", commit.Message)
			}

			// The other flavour may change or fail capture without changing this
			// proposal's identity or requiring a usable candidate for that flavour.
			other := "standard"
			if flavour == other {
				other = "lts"
			}
			if err := os.MkdirAll(filepath.Join(candidates, other), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(candidates, other, "lock.json"), []byte("failed capture"), 0o644); err != nil {
				t.Fatal(err)
			}
			var repeated bytes.Buffer
			if err := run(context.Background(), args, &repeated, io.Discard); err != nil {
				t.Fatal(err)
			}
			var repeatedResult proposal
			if err := json.Unmarshal(repeated.Bytes(), &repeatedResult); err != nil {
				t.Fatal(err)
			}
			if repeatedResult != result {
				t.Fatalf("other flavour changed the proposal: %s", repeated.Bytes())
			}
		})
	}
}

func TestProposalSelection(t *testing.T) {
	t.Chdir(t.TempDir())
	candidates := t.TempDir()
	lock, _ := capturedLock(t, "lts")
	if err := kernelrelease.Write(filepath.Join(candidates, "standard", "lock.json"), lock); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	for _, flavour := range []string{"", "unknown", "../lts", "standard"} {
		t.Run(flavour, func(t *testing.T) {
			args := []string{"propose", "--flavour", flavour, "--candidates", candidates, "--repository", "example/katl", "--base", strings.Repeat("a", 40)}
			err := run(context.Background(), args, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "flavour") {
				t.Fatalf("invalid proposal selection = %v", err)
			}
		})
	}
}
