package vmtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/kernelmodule"
)

// Modules of these extensions initialise only when their hardware is present,
// and release qualification runs without it.
var deviceBoundExtensions = map[string]bool{"ghcr.io/katl-dev/katl/extensions/nvidia": true}

const releaseExtensionStore = "/var/lib/katl/test-artifacts/release-extensions"

// releaseExtensions holds a candidate image's release extensions composed for
// that image's own runtime, ready to merge on a node booted from it.
type releaseExtensions struct {
	kernel  string
	images  []string
	modules []releaseModule
}

type releaseModule struct {
	repository string
	kernelmodule.Module
}

// readReleaseExtensions reads what `katl-mkosi-artifacts
// compose-release-extensions` wrote for the candidate's install image. The
// composition needs mount privileges that a VM test does not have, so it runs
// before the test.
func readReleaseExtensions(dir string) (releaseExtensions, error) {
	var extensions releaseExtensions
	data, err := os.ReadFile(filepath.Join(dir, "modules.json"))
	if err != nil {
		return extensions, err
	}
	var contracts map[string]kernelmodule.Contract
	if err := json.Unmarshal(data, &contracts); err != nil {
		return extensions, fmt.Errorf("%s: %w", dir, err)
	}
	for _, repository := range slices.Sorted(maps.Keys(contracts)) {
		contract := contracts[repository]
		if err := contract.Validate(); err != nil {
			return extensions, fmt.Errorf("release extension %s: %w", repository, err)
		}
		if extensions.kernel != "" && extensions.kernel != contract.Target.Release {
			return extensions, fmt.Errorf("release extensions target kernels %q and %q", extensions.kernel, contract.Target.Release)
		}
		extensions.kernel = contract.Target.Release
		for _, module := range contract.Modules {
			extensions.modules = append(extensions.modules, releaseModule{repository: repository, Module: module})
		}
	}
	if len(extensions.modules) == 0 {
		return extensions, fmt.Errorf("%s lists no release extension modules", dir)
	}
	extensions.images, err = filepath.Glob(filepath.Join(dir, "*.raw"))
	if err != nil || len(extensions.images) == 0 {
		return extensions, fmt.Errorf("%s holds no composed images: %v", dir, err)
	}
	return extensions, nil
}

// loadReleaseExtensions merges the composed images through /run/extensions,
// where generation activation places a node's selected extensions, and loads
// every module they ship. It runs last on a disposable node: the merge and
// the loaded modules last until that node stops.
func loadReleaseExtensions(extensions releaseExtensions, run func(command string, stdin io.Reader) (string, error), logf func(string, ...any)) error {
	kernel, err := run("uname -r", nil)
	if err != nil || kernel != extensions.kernel {
		return fmt.Errorf("release extensions target kernel %q, running kernel is %q: %v", extensions.kernel, kernel, err)
	}
	// The images exceed a small node's /run, so they stay in writable state
	// and /run/extensions links to them, as for a generation's own images.
	if _, err := run("mkdir -p "+releaseExtensionStore+" /run/extensions", nil); err != nil {
		return err
	}
	for _, image := range extensions.images {
		file, err := os.Open(image)
		if err != nil {
			return err
		}
		stored := shellQuote(releaseExtensionStore + "/" + filepath.Base(image))
		activated := shellQuote("/run/extensions/" + filepath.Base(image))
		// Refuse to shadow an image the booted generation already activated.
		_, err = run("test ! -e "+activated+" && cat > "+stored+" && ln -s "+stored+" "+activated, file)
		file.Close()
		if err != nil {
			return fmt.Errorf("copy %s to the node: %w", filepath.Base(image), err)
		}
	}
	if output, err := run("systemd-sysext refresh 2>&1", nil); err != nil {
		return fmt.Errorf("merge release extensions: %v: %s", err, output)
	}

	// The production image has no test agent, so these checks run over SSH as
	// an independent oracle for kernelmodule.LoadRequired's rule: the selected
	// file has the contract's digest, modinfo resolves the module to it, and
	// the loaded module is that file.
	for _, module := range extensions.modules {
		name, path := shellQuote(module.Name), shellQuote("/"+module.Path)
		digest, err := run("sha256sum -- "+path, nil)
		if fields := strings.Fields(digest); err != nil || len(fields) == 0 || fields[0] != module.SHA256 {
			return fmt.Errorf("%s module %s file %s does not have its contract digest %s: %q: %v", module.repository, module.Name, module.Path, module.SHA256, digest, err)
		}
		providers, err := run("readlink -f \"$(modinfo -F filename -- "+name+")\" "+path, nil)
		if lines := strings.Split(providers, "\n"); err != nil || len(lines) != 2 || lines[0] != lines[1] {
			return fmt.Errorf("%s module %s does not resolve to its extension file %s: %q: %v", module.repository, module.Name, module.Path, providers, err)
		}
		// Ignore modprobe install commands, as node activation does.
		output, err := run("modprobe --ignore-install -- "+name+" 2>&1", nil)
		if err := moduleLoadFailure(module.repository, module.Name, output, err); err != nil {
			return err
		}
		if err != nil {
			logf("%s module %s stopped at the absent device: %s", module.repository, module.Name, output)
			continue
		}
		loaded := shellQuote("/sys/module/" + strings.ReplaceAll(module.Name, "-", "_") + "/srcversion")
		identity, err := run("cat "+loaded+" && modinfo -F srcversion -- "+path, nil)
		if lines := strings.Split(identity, "\n"); err != nil || len(lines) != 2 || lines[0] == "" || lines[0] != lines[1] {
			return fmt.Errorf("%s module %s loaded a provider other than %s: %q: %v", module.repository, module.Name, module.Path, identity, err)
		}
		logf("%s module %s loaded", module.repository, module.Name)
	}
	return nil
}

// moduleLoadFailure returns nil for a successful load and for a device-bound
// module that stopped only because its device is absent. Loading resolves
// symbols and checks the module format before a module probes for devices, so
// that stop proves the module itself fits its runtime. Without a GPU that
// holds only for the root nvidia module: nvidia_uvm, nvidia_modeset and
// nvidia_drm stop when modprobe fails to load nvidia, before their own
// symbols are resolved, so their fit is not proven here. #927 tracks checking
// their symbols offline during composition.
func moduleLoadFailure(repository, module, output string, err error) error {
	if err == nil {
		return nil
	}
	if deviceBoundExtensions[repository] && onlyDeviceAbsent(output) {
		return nil
	}
	return fmt.Errorf("%s module %s failed to load: %v: %s", repository, module, err, output)
}

func onlyDeviceAbsent(output string) bool {
	absent := false
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, ": No such device") {
			return false
		}
		absent = true
	}
	return absent
}

func TestModuleLoadFailure(t *testing.T) {
	const nvidia, drbd = "ghcr.io/katl-dev/katl/extensions/nvidia", "ghcr.io/katl-dev/katl/extensions/drbd9"
	failed := errors.New("exit status 1")
	for _, test := range []struct {
		name, repository, output string
		err                      error
		accepted                 bool
	}{
		{"loaded", drbd, "", nil, true},
		{"device absent", nvidia, "modprobe: ERROR: could not insert 'nvidia': No such device", failed, true},
		{"device absent then unknown symbol", nvidia, "modprobe: ERROR: could not insert 'nvidia': No such device\nmodprobe: ERROR: could not insert 'nvidia_uvm': Unknown symbol in module", failed, false},
		{"unknown symbol", nvidia, "modprobe: ERROR: could not insert 'nvidia': Unknown symbol in module, or unknown parameter (see dmesg)", failed, false},
		{"module format", nvidia, "modprobe: ERROR: could not insert 'nvidia': Exec format error", failed, false},
		{"device absent without device binding", drbd, "modprobe: ERROR: could not insert 'drbd': No such device", failed, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := moduleLoadFailure(test.repository, "nvidia_uvm", test.output, test.err)
			if test.accepted {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "nvidia_uvm") || !strings.Contains(err.Error(), test.output) {
				t.Fatalf("error = %v, want the module and the loader output", err)
			}
		})
	}
}

func TestReadReleaseExtensions(t *testing.T) {
	contract := func(kernel, module string) kernelmodule.Contract {
		return kernelmodule.Contract{
			Target: kernelmodule.Target{Release: kernel, RuntimeSHA256: strings.Repeat("a", 64)},
			Modules: []kernelmodule.Module{{
				Name:   module,
				Path:   "usr/lib/modules/" + kernel + "/extra/" + module + ".ko",
				SHA256: strings.Repeat("b", 64),
			}},
		}
	}
	const drbd, nvidia = "ghcr.io/katl-dev/katl/extensions/drbd9", "ghcr.io/katl-dev/katl/extensions/nvidia"
	for _, test := range []struct {
		name      string
		contracts map[string]kernelmodule.Contract
		images    []string
		refusal   string
	}{
		{"composed", map[string]kernelmodule.Contract{drbd: contract("6.18.1", "drbd"), nvidia: contract("6.18.1", "nvidia")}, []string{"drbd9.raw", "katl-modules.raw"}, ""},
		{"not composed", nil, nil, "modules.json"},
		{"different kernels", map[string]kernelmodule.Contract{drbd: contract("6.18.1", "drbd"), nvidia: contract("6.18.2", "nvidia")}, []string{"drbd9.raw"}, "target kernels"},
		{"no modules", map[string]kernelmodule.Contract{}, []string{"drbd9.raw"}, "lists no release extension modules"},
		{"no images", map[string]kernelmodule.Contract{drbd: contract("6.18.1", "drbd")}, nil, "holds no composed images"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if test.contracts != nil {
				data, err := json.Marshal(test.contracts)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "modules.json"), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, image := range test.images {
				if err := os.WriteFile(filepath.Join(dir, image), []byte(image), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			extensions, err := readReleaseExtensions(dir)
			if test.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), test.refusal) {
					t.Fatalf("error = %v, want %q", err, test.refusal)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if extensions.kernel != "6.18.1" || len(extensions.images) != 2 {
				t.Fatalf("extensions = %+v", extensions)
			}
			loaded := map[string]string{}
			for _, module := range extensions.modules {
				loaded[module.Name] = module.repository
			}
			if len(loaded) != 2 || loaded["drbd"] != drbd || loaded["nvidia"] != nvidia {
				t.Fatalf("modules by extension = %v", loaded)
			}
		})
	}
}
