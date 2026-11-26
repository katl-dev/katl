package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The roots are the Go entry points in the driver's image builds and hosted
// journey. Go resolves their source files and imports for each build mode.
var driverGuestPackages = []string{
	"./cmd/katl-api-proxy",
	"./cmd/katl-boot-health",
	"./cmd/katl-console",
	"./cmd/katl-endpoint-advertiser",
	"./cmd/katl-generation-activate",
	"./cmd/katl-host-config-activate",
	"./cmd/katl-kubelet-startup",
	"./cmd/katl-ssh-host-keys",
	"./cmd/katl-system-disk-generator",
	"./cmd/katl-vmtest-agent",
	"./cmd/katlc",
	"./cmd/katlos-install",
}

var driverHostPackages = []string{
	"./cmd/katl-kernel-release",
	"./cmd/katl-mkosi-artifacts",
	"./cmd/katl-resource-lock",
	"./cmd/katl-runtime-components",
	"./cmd/katl-runtime-rpms",
	"./cmd/katl-vmtest-agent",
	"./cmd/katl-vmtest-ran",
	"./cmd/katlctl",
}

const driverTestPackage = "./internal/vmtest"

type driverGoModule struct {
	Path, Version, Dir string
	Replace            *driverGoModule
}

type driverGoPackage struct {
	Dir, ImportPath, Name string
	Standard              bool
	Module                *driverGoModule
	GoFiles, CgoFiles, CFiles, CXXFiles, MFiles, HFiles, FFiles, SFiles,
	SwigFiles, SwigCXXFiles, SysoFiles, EmbedFiles []string
}

// The empty value marks a repository path whose bytes and mode come from Git.
func driverGoInputs(root string) (map[string]string, error) {
	files := map[string]string{}
	for _, build := range []struct {
		cgo      string
		tests    bool
		packages []string
	}{
		{"0", false, driverGuestPackages},
		{"1", false, driverHostPackages},
		{"1", true, []string{driverTestPackage}},
	} {
		args := []string{"list", "-buildvcs=false", "-mod=readonly", "-deps", "-json"}
		if build.tests {
			args = append(args, "-test")
		}
		command := exec.Command("go", append(args, build.packages...)...)
		command.Dir = root
		command.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "GOAMD64=v1",
			"CGO_ENABLED="+build.cgo, "GOFLAGS=", "GOEXPERIMENT=", "GOWORK=off")
		output, err := command.Output()
		if err != nil {
			if failure, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("list Go driver inputs: %w: %s", err, failure.Stderr)
			}
			return nil, fmt.Errorf("list Go driver inputs: %w", err)
		}

		decoder := json.NewDecoder(bytes.NewReader(output))
		for {
			var pkg driverGoPackage
			err := decoder.Decode(&pkg)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("decode Go driver inputs: %w", err)
			}
			// The test binary's generated main lives in the Go build cache.
			// The test variants of its imported packages contain the sources.
			if pkg.Standard || (build.tests && pkg.Name == "main" && strings.HasSuffix(pkg.ImportPath, ".test")) {
				continue
			}
			if err := pkg.addFiles(root, files); err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

func (pkg driverGoPackage) addFiles(root string, files map[string]string) error {
	dir, err := filepath.Rel(root, pkg.Dir)
	if err != nil {
		return err
	}
	local := dir != ".." && !strings.HasPrefix(dir, "../")
	if !local {
		module := pkg.Module
		if module != nil && module.Replace != nil {
			module = module.Replace
		}
		if module == nil || module.Version == "" {
			return fmt.Errorf("Go driver package %q is outside the repository and has no module version", pkg.ImportPath)
		}
		relative, err := filepath.Rel(module.Dir, pkg.Dir)
		if err != nil {
			return err
		}
		dir = filepath.Join("module", module.Path+"@"+module.Version, relative)
	}
	for _, names := range [][]string{
		pkg.GoFiles, pkg.CgoFiles, pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles,
		pkg.SFiles, pkg.SwigFiles, pkg.SwigCXXFiles, pkg.SysoFiles, pkg.EmbedFiles,
	} {
		for _, name := range names {
			key := filepath.Join(dir, name)
			if _, ok := files[key]; ok {
				continue
			}
			if local {
				files[key] = ""
				continue
			}
			contents, err := os.ReadFile(filepath.Join(pkg.Dir, name))
			if err != nil {
				return err
			}
			files[key] = fmt.Sprintf("%x", sha256.Sum256(contents))
		}
	}
	return nil
}
