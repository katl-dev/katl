package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pin = "tag=fedora-99-0123456789abcdef\nimage=ghcr.io/katl-dev/katl/mkosi-builder@sha256:" + "cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34" + "\n"

func writeLock(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func args(lock string) []string {
	return []string{"propose", "--lock", lock, "--repository", "example/katl", "--base", strings.Repeat("a", 40)}
}

// Republishing the committed pin must not contact GitHub.
func TestProposeUnchanged(t *testing.T) {
	t.Chdir(t.TempDir())
	writeLock(t, "mkosi-builder.lock", pin)
	proposed := filepath.Join(t.TempDir(), "mkosi-builder.lock")
	writeLock(t, proposed, pin)
	t.Setenv("PATH", t.TempDir())

	var stdout bytes.Buffer
	if err := run(context.Background(), args(proposed), &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != `{"changed":false}` {
		t.Fatalf("result = %s", stdout.Bytes())
	}
}

// A new pin replaces only mkosi-builder.lock, with exactly the bytes the
// workflow wrote, on a branch named by those bytes so a rerun finds it.
func TestProposeNewPin(t *testing.T) {
	t.Chdir(t.TempDir())
	writeLock(t, "mkosi-builder.lock", strings.Replace(pin, "cd34", "ef56", 1))
	proposed := filepath.Join(t.TempDir(), "mkosi-builder.lock")
	writeLock(t, proposed, pin)

	bin, requests := t.TempDir(), t.TempDir()
	t.Setenv("FAKE_REQUESTS", requests)
	script := `#!/usr/bin/env bash
set -euo pipefail
case "$3:$4" in
 GET:*/git/ref/heads/automation/mkosi-builder-*) echo 'HTTP 404' >&2; exit 1 ;;
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

	var stdout bytes.Buffer
	if err := run(context.Background(), args(proposed), &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Changed        bool
		Branch, Commit string
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Commit != "new-commit" || !strings.HasPrefix(result.Branch, "automation/mkosi-builder-") {
		t.Fatalf("result = %+v", result)
	}

	data, err := os.ReadFile(filepath.Join(requests, "tree"))
	if err != nil {
		t.Fatal(err)
	}
	var tree struct {
		Tree []struct{ Path, Content string }
	}
	if err := json.Unmarshal(data, &tree); err != nil {
		t.Fatal(err)
	}
	if len(tree.Tree) != 1 || tree.Tree[0].Path != "mkosi-builder.lock" || tree.Tree[0].Content != pin {
		t.Fatalf("tree = %s", data)
	}
	data, err = os.ReadFile(filepath.Join(requests, "commit"))
	if err != nil {
		t.Fatal(err)
	}
	var commit struct{ Message string }
	if err := json.Unmarshal(data, &commit); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(commit.Message, "mkosi: pin the published builder image\n") || !strings.Contains(commit.Message, "\nRelease-Note: skip\n") {
		t.Fatalf("commit message = %q", commit.Message)
	}
}

// The branch is a function of the pin, so a rerun advances the same proposal
// and a different pin never reuses it.
func TestProposalBranch(t *testing.T) {
	t.Chdir(t.TempDir())
	branch := func(content string) string {
		t.Helper()
		bin := t.TempDir()
		script := `#!/usr/bin/env bash
case "$3:$4" in
 GET:*/git/ref/heads/*) echo 'HTTP 404' >&2; exit 1 ;;
 GET:*/git/commits/*) printf '{"tree":{"sha":"t"}}' ;;
 POST:*/git/trees | POST:*/git/commits) printf '{"sha":"s"}' ;;
esac
`
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		proposed := filepath.Join(t.TempDir(), "lock")
		writeLock(t, proposed, content)
		var stdout bytes.Buffer
		if err := run(context.Background(), args(proposed), &stdout, io.Discard); err != nil {
			t.Fatal(err)
		}
		var result struct{ Branch string }
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Branch
	}
	other := strings.Replace(pin, "cd34", "ef56", 1)
	if branch(pin) != branch(pin) || branch(pin) == branch(other) {
		t.Fatalf("branches: %q, %q", branch(pin), branch(other))
	}
}
