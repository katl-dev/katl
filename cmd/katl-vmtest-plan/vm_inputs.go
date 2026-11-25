package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Hosted VM producer, image hooks, runner helpers, and binaries built by the
// scenarios. The VM test packages themselves come from the group plan.
var hostedGoTargets = []string{
	"./cmd/katl-api-proxy",
	"./cmd/katl-boot-health",
	"./cmd/katl-console",
	"./cmd/katl-devclean",
	"./cmd/katl-endpoint-advertiser",
	"./cmd/katl-generation-activate",
	"./cmd/katl-host-config-activate",
	"./cmd/katl-kernel-release",
	"./cmd/katl-kubelet-startup",
	"./cmd/katl-kubernetes-metadata",
	"./cmd/katl-kubernetes-release",
	"./cmd/katl-mkosi-artifacts",
	"./cmd/katl-publish-kubernetes-sysext",
	"./cmd/katl-resource-lock",
	"./cmd/katl-runtime-components",
	"./cmd/katl-runtime-rpms",
	"./cmd/katl-ssh-host-keys",
	"./cmd/katl-system-disk-generator",
	"./cmd/katl-vmtest-agent",
	"./cmd/katl-vmtest-health",
	"./cmd/katl-vmtest-plan",
	"./cmd/katl-vmtest-ran",
	"./cmd/katl-vmtest-release-networks",
	"./cmd/katlc",
	"./cmd/katlctl",
	"./cmd/katlos-install",
	"./internal/vmtest/testcmd/...",
	"./internal/vmtest/testdata/service-security",
}

func sharedGoInputs(base string) (map[string]bool, error) {
	module, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return nil, fmt.Errorf("find Go module: %w", err)
	}
	modFile := strings.TrimSuffix(string(module), "\n")
	if filepath.Base(modFile) != "go.mod" {
		return nil, fmt.Errorf("cannot find go.mod")
	}
	targets := slices.Clone(hostedGoTargets)
	for _, g := range groups {
		if !slices.Contains(targets, g.Package) {
			targets = append(targets, g.Package)
		}
	}
	return goInputsForTrees(filepath.Dir(modFile), base, targets)
}

func goInputsForTrees(repo, base string, targets []string) (map[string]bool, error) {
	inputs := map[string]bool{}
	if err := addGoInputs(inputs, repo, targets); err != nil {
		return nil, fmt.Errorf("candidate tree: %w", err)
	}
	if base == "" {
		return inputs, nil
	}

	snapshot, err := os.MkdirTemp("", "katl-vm-inputs-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(snapshot)
	tree := filepath.Join(snapshot, "tree")
	// Use a private index so deleted files can be inspected without changing
	// the checkout or applying archive export-ignore rules to build inputs.
	for _, args := range [][]string{
		{"read-tree", base},
		{"checkout-index", "--all", "--prefix=" + tree + string(filepath.Separator)},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(snapshot, "index"))
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("materialize tested base: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	if err := addGoInputs(inputs, tree, targets); err != nil {
		return nil, fmt.Errorf("tested base: %w", err)
	}
	return inputs, nil
}

func addGoInputs(inputs map[string]bool, repo string, targets []string) error {
	// The hosted test runner and image builder can select different Go files.
	for _, cgo := range []string{"1", "0"} {
		cmd := exec.Command("go", append([]string{"list", "-deps", "-test", "-buildvcs=false", "-json"}, targets...)...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED="+cgo, "GOWORK=off")
		cmd.Stderr = os.Stderr
		output, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("go list (CGO_ENABLED=%s): %w", cgo, err)
		}

		decoder := json.NewDecoder(bytes.NewReader(output))
		for {
			var pkg struct {
				Dir                                          string
				Module                                       *struct{ Main bool }
				GoFiles, CgoFiles, TestGoFiles, XTestGoFiles []string
				IgnoredGoFiles                               []string
				EmbedFiles, TestEmbedFiles, XTestEmbedFiles  []string
				CFiles, CXXFiles, MFiles, HFiles, FFiles     []string
				SFiles, SwigFiles, SwigCXXFiles, SysoFiles   []string
			}
			if err := decoder.Decode(&pkg); err == io.EOF {
				break
			} else if err != nil {
				return err
			}
			if pkg.Module == nil || !pkg.Module.Main || pkg.Dir == "" {
				continue
			}
			dir, err := filepath.Rel(repo, pkg.Dir)
			if err != nil || !filepath.IsLocal(dir) {
				return fmt.Errorf("Go package directory outside the repository: %q", pkg.Dir)
			}
			// go list expands //go:embed patterns against this tree, including
			// test embeds and files matched by directory or wildcard patterns.
			for _, files := range [][]string{
				pkg.GoFiles, pkg.CgoFiles, pkg.TestGoFiles, pkg.XTestGoFiles, pkg.IgnoredGoFiles,
				pkg.EmbedFiles, pkg.TestEmbedFiles, pkg.XTestEmbedFiles,
				pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles, pkg.SwigFiles, pkg.SwigCXXFiles, pkg.SysoFiles,
			} {
				for _, name := range files {
					inputs[filepath.ToSlash(filepath.Join(dir, name))] = true
				}
			}
		}
	}
	return nil
}
