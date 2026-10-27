package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKernelCaptureIsolation(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/kernel-update.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ Run string }
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var capture string
	for _, step := range workflow.Jobs["capture"].Steps {
		if strings.Contains(step.Run, "--kernel-release") {
			capture = step.Run
		}
	}
	if capture == "" {
		t.Fatal("missing candidate capture")
	}
	for _, flavour := range []string{"standard", "lts"} {
		t.Run(flavour, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
				t.Fatal(err)
			}
			for name, script := range map[string]string{
				"fedora-release": "#!/bin/sh\nprintf '44\\n'\n",
				"mkosi":          "#!/bin/sh\nprintf 'call\\n' >> capture-count\nprintf '%s\\0' \"$@\" > capture-args\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, "scripts", name), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("bash", "-euo", "pipefail", "-c", capture)
			command.Dir = dir
			command.Env = append(os.Environ(), "KATL_KERNEL_FLAVOUR="+flavour, "KERNEL_RELEASE=6.18.58-200.fc44.x86_64")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("capture: %v: %s", err, output)
			}
			count, err := os.ReadFile(filepath.Join(dir, "capture-count"))
			if err != nil || string(count) != "call\n" {
				t.Fatalf("capture ran for another flavour: %q, %v", count, err)
			}
			args, err := os.ReadFile(filepath.Join(dir, "capture-args"))
			if err != nil {
				t.Fatal(err)
			}
			for _, pair := range []string{
				"--flavour\x00" + flavour + "\x00",
				"--kernel-release\x006.18.58-200.fc44.x86_64\x00",
				"--output\x00_build/kernel-candidates/" + flavour + "\x00",
			} {
				if !strings.Contains(string(args), pair) {
					t.Fatalf("capture does not preserve selected inputs: %q", args)
				}
			}
		})
	}
}
