package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
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
	"./cmd/katl-vmtest-evidence",
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

type vmInputs struct {
	goFiles     map[string]bool
	linkTargets map[string]bool
}

func (in vmInputs) hasLinkTarget(name string) bool {
	for ; name != "."; name = path.Dir(name) {
		if in.linkTargets[name] {
			return true
		}
	}
	return in.linkTargets["."]
}

func sharedVMInputs(base string) (vmInputs, error) {
	if base == "" {
		return vmInputs{}, fmt.Errorf("--base is required to reduce VM coverage")
	}
	module, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return vmInputs{}, fmt.Errorf("find Go module: %w", err)
	}
	modFile := strings.TrimSuffix(string(module), "\n")
	if filepath.Base(modFile) != "go.mod" {
		return vmInputs{}, fmt.Errorf("cannot find go.mod")
	}
	targets := slices.Clone(hostedGoTargets)
	for _, g := range groups {
		if !slices.Contains(targets, g.Package) {
			targets = append(targets, g.Package)
		}
	}
	return vmInputsForTrees(filepath.Dir(modFile), base, targets)
}

func vmInputsForTrees(repo, base string, targets []string) (vmInputs, error) {
	inputs := vmInputs{goFiles: map[string]bool{}, linkTargets: map[string]bool{}}
	if err := inputs.addTree(repo, repo, "", targets); err != nil {
		return vmInputs{}, fmt.Errorf("candidate tree: %w", err)
	}

	snapshot, err := os.MkdirTemp("", "katl-vm-inputs-")
	if err != nil {
		return vmInputs{}, err
	}
	defer os.RemoveAll(snapshot)
	tree := filepath.Join(snapshot, "tree")
	index := filepath.Join(snapshot, "index")
	// Use a private index so deleted files can be inspected without changing
	// the checkout or applying archive export-ignore rules to build inputs.
	for _, args := range [][]string{
		{"read-tree", base},
		{"checkout-index", "--all", "--prefix=" + tree + string(filepath.Separator)},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
		if output, err := cmd.CombinedOutput(); err != nil {
			return vmInputs{}, fmt.Errorf("materialize tested base: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	if err := inputs.addTree(repo, tree, index, targets); err != nil {
		return vmInputs{}, fmt.Errorf("tested base: %w", err)
	}
	return inputs, nil
}

func (in vmInputs) addTree(repo, tree, index string, targets []string) error {
	goFiles := map[string]bool{}
	if err := addGoInputs(goFiles, tree, targets); err != nil {
		return err
	}
	for name := range goFiles {
		in.goFiles[name] = true
	}

	cmd := exec.Command("git", "ls-files", "--stage", "-z")
	cmd.Dir = repo
	if index != "" {
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
	}
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("list tracked input symlinks: %w", err)
	}
	links := map[string]bool{}
	var pending []string
	for entry := range strings.SplitSeq(string(output), "\x00") {
		if !strings.HasPrefix(entry, "120000 ") {
			continue
		}
		_, name, _ := strings.Cut(entry, "\t")
		links[name] = true
		// Unmapped non-doc paths may also be VM inputs. A whole producer
		// directory can be a link, so include those sources conservatively.
		if !documentationPath(name) || sharedVMPath(name) || goFiles[name] {
			pending = append(pending, name)
		}
	}

	// A link to a directory also makes links within that directory relevant.
	for i := 0; i < len(pending); i++ {
		name := pending[i]
		target, err := in.resolveInputLink(repo, tree, name, links)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		in.linkTargets[target] = true
		for link := range links {
			if (target == "." || strings.HasPrefix(link, target+"/")) && !slices.Contains(pending, link) {
				pending = append(pending, link)
			}
		}
	}
	return nil
}

func (in vmInputs) resolveInputLink(repo, tree, name string, links map[string]bool) (string, error) {
	// Resolve each component so an intermediate link remains an input even
	// when its final target is elsewhere. Guest-absolute links have no
	// repository target, unless they name a path under the checkout.
	dir, rest := path.Dir(name), path.Base(name)
	for followed := 0; rest != ""; {
		part, remaining, _ := strings.Cut(rest, "/")
		rest = remaining
		if part == "" || part == "." {
			continue
		}
		if part == ".." && dir == "." {
			return "", nil
		}
		candidate := path.Join(dir, part)
		if !links[candidate] {
			dir = candidate
			continue
		}
		followed++
		if followed > 255 {
			return "", fmt.Errorf("too many symlink traversals from %s", name)
		}
		if candidate != name {
			in.linkTargets[candidate] = true
		}
		target, err := os.Readlink(filepath.Join(tree, candidate))
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			target, err = filepath.Rel(repo, target)
			if err != nil || !filepath.IsLocal(target) {
				return "", nil
			}
			dir = "."
		}
		rest = target + "/" + rest
	}
	return dir, nil
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
