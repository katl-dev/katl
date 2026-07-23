package kubernetesrelease

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var recipeRoots = []string{
	".github/workflows/kubernetes-bundles.yml",
	"Containerfile.mkosi",
	"cmd/katl-boot-health",
	"cmd/katl-console",
	"cmd/katl-generation-activate",
	"cmd/katl-kubernetes-release",
	"cmd/katl-mkosi-artifacts",
	"cmd/katl-publish-kubernetes-sysext",
	"cmd/katl-runtime-status",
	"cmd/katlc",
	"containers-policy.json",
	"go.mod",
	"go.sum",
	"internal",
	"mkosi.conf",
	"mkosi.profiles/kubernetes-sysext",
	"mkosi.profiles/runtime",
	"scripts/build-kubernetes-sysext",
	"scripts/check-kubernetes-sysext",
	"scripts/mkosi",
}

var recipeExcludedPaths = map[string]bool{
	"internal/installer/kubernetescompat/catalog.json":   true,
	"internal/kubernetesrelease/supported-versions.json": true,
}

func RecipeDigest(root string) (string, error) {
	paths, err := recipeFiles(root)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	for _, file := range paths {
		if _, err := io.WriteString(digest, file.mode); err != nil {
			return "", err
		}
		if _, err := digest.Write([]byte{0}); err != nil {
			return "", err
		}
		if _, err := io.WriteString(digest, file.path); err != nil {
			return "", err
		}
		if _, err := digest.Write([]byte{0}); err != nil {
			return "", err
		}
		if _, err := digest.Write(file.content); err != nil {
			return "", err
		}
		if _, err := digest.Write([]byte{0}); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil)), nil
}

func RefreshRecipe(root string, supported SupportedVersions) (SupportedVersions, bool, error) {
	digest, err := RecipeDigest(root)
	if err != nil {
		return SupportedVersions{}, false, err
	}
	if supported.RecipeDigest == digest {
		return supported, false, nil
	}
	supported.Versions = copyVersions(supported.Versions)
	supported.RecipeDigest = digest
	for index := range supported.Versions {
		supported.Versions[index].ArtifactRevision++
	}
	if err := validateSupportedVersions(supported); err != nil {
		return SupportedVersions{}, false, err
	}
	return supported, true, nil
}

type recipeFile struct {
	path    string
	mode    string
	content []byte
}

func recipeFiles(root string) ([]recipeFile, error) {
	args := append([]string{"-C", root, "ls-files", "-z", "--"}, recipeRoots...)
	output, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked Kubernetes bundle recipe inputs: %w", err)
	}
	var files []recipeFile
	for _, rawPath := range bytes.Split(output, []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		relative := filepath.ToSlash(string(rawPath))
		if recipePathExcluded(relative) {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect Kubernetes bundle recipe input %s: %w", relative, err)
		}
		file := recipeFile{path: relative}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			file.mode = "120000"
			target, err := os.Readlink(path)
			if err != nil {
				return nil, fmt.Errorf("read Kubernetes bundle recipe symlink %s: %w", relative, err)
			}
			file.content = []byte(target)
		case info.Mode().IsRegular():
			file.mode = "100644"
			if info.Mode().Perm()&0o111 != 0 {
				file.mode = "100755"
			}
			file.content, err = os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read Kubernetes bundle recipe input %s: %w", relative, err)
			}
		default:
			return nil, fmt.Errorf("unsupported Kubernetes bundle recipe input type %s", relative)
		}
		files = append(files, file)
	}
	sort.Slice(files, func(left, right int) bool {
		return files[left].path < files[right].path
	})
	if len(files) == 0 {
		return nil, fmt.Errorf("Kubernetes bundle recipe has no tracked inputs")
	}
	return files, nil
}

func recipePathExcluded(path string) bool {
	if recipeExcludedPaths[path] || strings.HasSuffix(path, "_test.go") {
		return true
	}
	for _, excluded := range []string{
		"/testdata/",
		"internal/resourcetest/",
		"internal/vmtest/",
	} {
		if strings.Contains(path, excluded) || strings.HasPrefix(path, strings.TrimPrefix(excluded, "/")) {
			return true
		}
	}
	return false
}
