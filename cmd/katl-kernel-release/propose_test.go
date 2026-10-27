package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/kernelrelease"
)

func TestProposalIsolation(t *testing.T) {
	for _, flavour := range []string{"standard", "lts"} {
		t.Run(flavour, func(t *testing.T) {
			t.Chdir(t.TempDir())
			lock, _ := capturedLock(t, flavour)
			candidates := t.TempDir()
			if err := kernelrelease.Write(filepath.Join(candidates, flavour, "lock.json"), lock); err != nil {
				t.Fatal(err)
			}
			requests := t.TempDir()
			t.Setenv("REQUESTS", requests)
			bin := t.TempDir()
			script := `#!/usr/bin/env bash
set -euo pipefail
case "$3:$4" in
  GET:*/git/ref/heads/*) echo 'HTTP 404' >&2; exit 1 ;;
  GET:*/git/commits/*) printf '{"tree":{"sha":"base-tree"}}' ;;
  POST:*/git/trees) cat > "$REQUESTS/tree"; printf '{"sha":"proposal-tree"}' ;;
  POST:*/git/commits) cat > "$REQUESTS/commit"; printf '{"sha":"proposal-commit"}' ;;
  POST:*/git/refs) cat > "$REQUESTS/ref"; printf '{}' ;;
  *) echo 'unexpected GitHub request' >&2; exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			var output bytes.Buffer
			err := run(context.Background(), []string{"propose", "--flavour", flavour, "--candidates", candidates, "--repository", "example/katl", "--base", strings.Repeat("a", 40)}, &output, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Changed bool
				Branch  string
				Commit  string
			}
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Changed || result.Commit != "proposal-commit" || !regexp.MustCompile(`^automation/kernels-`+flavour+`-[a-f0-9]{16}$`).MatchString(result.Branch) {
				t.Fatalf("proposal = %s", output.Bytes())
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
				t.Fatalf("proposal changed another flavour: %s", data)
			}
			var selected kernelrelease.Lock
			if err := json.Unmarshal([]byte(tree.Tree[0].Content), &selected); err != nil {
				t.Fatal(err)
			}
			if selected.Flavour != flavour || selected.Reference != lock.Reference || selected.KernelRelease != lock.KernelRelease {
				t.Fatalf("proposal changed captured inputs: %+v", selected)
			}
		})
	}
}

func TestProposalRequiresFlavour(t *testing.T) {
	for _, flavour := range []string{"", "both", "../lts"} {
		err := run(context.Background(), []string{"propose", "--flavour", flavour, "--repository", "example/katl", "--base", strings.Repeat("a", 40)}, io.Discard, io.Discard)
		if err == nil {
			t.Fatalf("accepted flavour %q", flavour)
		}
	}
}
