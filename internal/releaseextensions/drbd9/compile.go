package drbd9

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/katl-dev/katl/internal/kernelmodule"
	"github.com/katl-dev/katl/internal/releaseextensions"
)

type Input struct {
	Target      kernelmodule.Target
	Version     string
	Sources     map[string]string
	Destination string
	Work        string
	Stdout      io.Writer
	Stderr      io.Writer
}

func Compile(input Input) (kernelmodule.Contract, error) {
	if len(input.Sources) != 2 || input.Sources["source"] == "" || input.Sources["headers"] == "" {
		return kernelmodule.Contract{}, fmt.Errorf("DRBD recipe requires source and headers archives")
	}
	for _, archive := range []struct {
		file string
		dir  string
	}{
		{input.Sources["source"], input.Work},
		{input.Sources["headers"], filepath.Join(input.Work, "drbd/drbd-headers")},
	} {
		if err := os.MkdirAll(archive.dir, 0o755); err != nil {
			return kernelmodule.Contract{}, err
		}
		// Verified archives are extracted only into this private build tree.
		command := exec.Command("tar", "--extract", "--gzip", "--file", archive.file, "--directory", archive.dir, "--strip-components=1", "--no-same-owner", "--no-same-permissions")
		command.Stdout, command.Stderr = input.Stdout, input.Stderr
		if err := command.Run(); err != nil {
			return kernelmodule.Contract{}, err
		}
	}
	checksum := strings.TrimSuffix(filepath.Base(input.Sources["source"]), ".tar.gz")
	if err := os.WriteFile(filepath.Join(input.Work, "drbd/.drbd_git_revision"), []byte("source-sha256: "+checksum+"\n"), 0o644); err != nil {
		return kernelmodule.Contract{}, err
	}
	kdir := filepath.Join("/usr/src/kernels", input.Target.Release)
	command := exec.Command("make", "-C", input.Work, fmt.Sprintf("-j%d", min(runtime.NumCPU(), 8)), "KVER="+input.Target.Release, "KDIR="+kdir, "SPAAS=false", "WANT_DRBD_REPRODUCIBLE_BUILD=1", "SOURCE_DATE_EPOCH=315532800", "module")
	command.Stdout, command.Stderr = input.Stdout, input.Stderr
	if err := command.Run(); err != nil {
		return kernelmodule.Contract{}, fmt.Errorf("compile DRBD: %w", err)
	}
	moduleDir, err := filepath.EvalSymlinks(filepath.Join(input.Work, "drbd/build-current"))
	if err != nil {
		return kernelmodule.Contract{}, err
	}
	contract, err := installModules(moduleDir, input.Destination, input.Target, input.Version)
	if err != nil {
		return contract, err
	}
	if err := releaseextensions.CopyRegular(filepath.Join(input.Work, "COPYING"), filepath.Join(input.Destination, "usr/share/licenses/katl-drbd9/COPYING"), 0o644); err != nil {
		return contract, err
	}
	return contract, nil
}

func installModules(source, destination string, target kernelmodule.Target, version string) (kernelmodule.Contract, error) {
	contract := kernelmodule.Contract{Target: target}
	found := map[string]bool{}
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".ko") {
			return err
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("built module must be a regular file: %s", path)
		}
		name := strings.TrimSuffix(entry.Name(), ".ko")
		if name != "drbd" && !strings.HasPrefix(name, "drbd_transport_") {
			return fmt.Errorf("unexpected DRBD build module %q", name)
		}
		if found[name] {
			return fmt.Errorf("duplicate built module %q", name)
		}
		found[name] = true
		for field, expected := range map[string]string{"name": strings.ReplaceAll(name, "-", "_"), "vermagic": target.Release} {
			value, err := exec.Command("modinfo", "-F", field, path).Output()
			parts := strings.Fields(string(value))
			if err != nil || len(parts) == 0 || parts[0] != expected {
				return fmt.Errorf("built module %s has unexpected %s: %s", name, field, value)
			}
		}
		if name == "drbd" {
			value, err := exec.Command("modinfo", "-F", "version", path).Output()
			if err != nil || strings.TrimSpace(string(value)) != version {
				return fmt.Errorf("built DRBD version does not match recipe %s", version)
			}
		}
		relative := filepath.Join("usr/lib/modules", target.Release, "extra/katl", entry.Name())
		output := filepath.Join(destination, relative)
		if err := releaseextensions.CopyRegular(path, output, 0o644); err != nil {
			return err
		}
		digest, err := releaseextensions.SHA256(output)
		if err != nil {
			return err
		}
		module := kernelmodule.Module{
			Name: name, Path: relative, SHA256: digest,
			Required: name == "drbd" || name == "drbd_transport_tcp",
		}
		if name == "drbd" {
			module.Replaces = "drbd"
		}
		contract.Modules = append(contract.Modules, module)
		return nil
	})
	if err != nil {
		return contract, err
	}
	if !found["drbd"] || !found["drbd_transport_tcp"] {
		return contract, fmt.Errorf("DRBD build must supply drbd and drbd_transport_tcp")
	}
	return contract, contract.Validate()
}
