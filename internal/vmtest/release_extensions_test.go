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
func readReleaseExtensions(t *testing.T, dir string) releaseExtensions {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "modules.json"))
	if err != nil {
		t.Fatalf("compose the candidate's release extensions with katl-mkosi-artifacts compose-release-extensions: %v", err)
	}
	var contracts map[string]kernelmodule.Contract
	if err := json.Unmarshal(data, &contracts); err != nil {
		t.Fatal(err)
	}
	var extensions releaseExtensions
	for _, repository := range slices.Sorted(maps.Keys(contracts)) {
		contract := contracts[repository]
		if err := contract.Validate(); err != nil {
			t.Fatalf("release extension %s: %v", repository, err)
		}
		if extensions.kernel != "" && extensions.kernel != contract.Target.Release {
			t.Fatalf("release extensions target kernels %q and %q", extensions.kernel, contract.Target.Release)
		}
		extensions.kernel = contract.Target.Release
		for _, module := range contract.Modules {
			extensions.modules = append(extensions.modules, releaseModule{repository: repository, Module: module})
		}
	}
	if len(extensions.modules) == 0 {
		t.Fatalf("%s lists no release extension modules", dir)
	}
	extensions.images, err = filepath.Glob(filepath.Join(dir, "*.raw"))
	if err != nil || len(extensions.images) == 0 {
		t.Fatalf("%s holds no composed images: %v", dir, err)
	}
	return extensions
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

	for _, module := range extensions.modules {
		name, path := shellQuote(module.Name), shellQuote("/"+module.Path)
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
// that stop still proves the module fits its runtime. A module that depends on
// such a module stops at the same point.
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
