package scriptstest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type kernelWorkflow struct {
	Jobs map[string]kernelWorkflowJob
}

type kernelWorkflowJob struct {
	If    string
	Needs any
	Steps []struct {
		Uses string
		Run  string
		With map[string]string
	}
}

func readKernelWorkflow(t *testing.T) kernelWorkflow {
	t.Helper()
	var workflow kernelWorkflow
	if err := yaml.Unmarshal(mustReadFile(t, filepath.Join(repoRoot(t), ".github/workflows/kernel-update.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func kernelCommand(t *testing.T, workflow kernelWorkflow, command string) (string, int) {
	t.Helper()
	for name, job := range workflow.Jobs {
		for i, step := range job.Steps {
			if strings.Contains(step.Run, "./cmd/katl-kernel-release "+command) {
				return name, i
			}
		}
	}
	t.Fatalf("missing kernel %s command", command)
	return "", 0
}

func TestKernelCaptureRelease(t *testing.T) {
	workflow := readKernelWorkflow(t)
	name, step := kernelCommand(t, workflow, "candidate")
	script := workflow.Jobs[name].Steps[step].Run

	for _, tc := range []struct {
		name, release, flavour, target, matches string
		fail                                    bool
	}{
		{name: "default", flavour: "standard", target: "44", matches: "true"},
		{name: "explicit current", release: "44", flavour: "lts", target: "44", matches: "true"},
		{name: "future standard", release: "45", flavour: "standard", target: "45", matches: "false"},
		{name: "future lts", release: "45", flavour: "lts", target: "45", matches: "false"},
		{name: "failed capture", release: "45", flavour: "standard", target: "45", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scripts := filepath.Join(dir, "scripts")
			if err := os.Mkdir(scripts, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "mkosi.conf"), []byte("[Distribution]\nRelease=44\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scripts, "fedora-release"), mustReadFile(t, filepath.Join(repoRoot(t), "scripts/fedora-release")), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFakeExecutable(t, scripts, "mkosi", `printf '%s\n' "$@" > "$CAPTURE_ARGS"
exit "$CAPTURE_EXIT"
`)
			output := filepath.Join(dir, "output")
			args := filepath.Join(dir, "args")
			if err := os.WriteFile(output, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			status := "0"
			if tc.fail {
				status = "1"
			}
			cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "KATL_REPO_ROOT="+dir, "FEDORA_RELEASE="+tc.release,
				"KATL_FLAVOUR="+tc.flavour, "KERNEL_RELEASE=", "GITHUB_OUTPUT="+output,
				"CAPTURE_ARGS="+args, "CAPTURE_EXIT="+status)
			log, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("capture failed=%t, want %t: %v: %s", err != nil, tc.fail, err, log)
			}

			arguments := string(mustReadFile(t, args))
			if !strings.Contains(arguments, "--fedora-release\n"+tc.target+"\n") || !strings.Contains(arguments, "--flavour\n"+tc.flavour+"\n") {
				t.Fatalf("wrong capture selection: %s", arguments)
			}
			result := string(mustReadFile(t, output))
			if tc.fail {
				if result != "" {
					t.Fatalf("failed capture exposed usable inputs: %s", result)
				}
				return
			}
			outputs := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(result), "\n") {
				key, value, _ := strings.Cut(line, "=")
				outputs[key] = value
			}
			if outputs["fedora-release"] != tc.target || outputs["matches-base"] != tc.matches {
				t.Fatalf("capture selection = %q", result)
			}
		})
	}
}

func TestKernelLockHandoff(t *testing.T) {
	workflow := readKernelWorkflow(t)
	name, step := kernelCommand(t, workflow, "verify")
	script := workflow.Jobs[name].Steps[step].Run
	for _, flavour := range []string{"standard", "lts"} {
		for _, fail := range []bool{false, true} {
			name := flavour + "/success"
			if fail {
				name = flavour + "/failed verification"
			}
			t.Run(name, func(t *testing.T) {
				dir, bin := t.TempDir(), t.TempDir()
				for _, captured := range []string{"standard", "lts"} {
					path := filepath.Join(dir, "_build/kernel-candidates", captured)
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
					data, err := json.Marshal(map[string]string{"flavour": captured, "reference": "registry.example/" + captured})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(path, "lock.json"), data, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				writeFakeExecutable(t, bin, "go", `printf '%s\n' "$@" > "$VERIFY_ARGS"
exit "$VERIFY_EXIT"
`)
				status := "0"
				if fail {
					status = "1"
				}
				args := filepath.Join(dir, "args")
				cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
					"KATL_FLAVOUR="+flavour, "VERIFY_ARGS="+args, "VERIFY_EXIT="+status)
				log, err := cmd.CombinedOutput()
				if (err != nil) != fail {
					t.Fatalf("verify failed=%t, want %t: %v: %s", err != nil, fail, err, log)
				}
				arguments := string(mustReadFile(t, args))
				selected := "_build/kernel-candidates/" + flavour + "/lock.json"
				if !strings.Contains(arguments, "--lock\n"+selected+"\n") && !strings.Contains(arguments, "--lock="+selected+"\n") {
					t.Fatalf("verification did not select the handed-off lock: %s", arguments)
				}
				if strings.Contains(arguments, "--layout") {
					t.Fatal("verification can reuse the private capture instead of fetching public bytes")
				}

				lock := filepath.Join(dir, "_build/kernel-locks", flavour+".json")
				if fail {
					if _, err := os.Stat(lock); !os.IsNotExist(err) {
						t.Fatalf("failed verification exposed a lock artifact: %v", err)
					}
					return
				}
				var actual struct{ Flavour, Reference string }
				if err := json.Unmarshal(mustReadFile(t, lock), &actual); err != nil {
					t.Fatal(err)
				}
				if actual.Flavour != flavour || actual.Reference != "registry.example/"+flavour {
					t.Fatalf("handoff selected another capture: %+v", actual)
				}
			})
		}
	}
}

func TestKernelUpdateOrder(t *testing.T) {
	workflow := readKernelWorkflow(t)
	capture, _ := kernelCommand(t, workflow, "candidate")
	publish, _ := kernelCommand(t, workflow, "publish")
	verify, verifyStep := kernelCommand(t, workflow, "verify")
	propose, _ := kernelCommand(t, workflow, "propose")
	var depends func(string, string) bool
	visiting := map[string]bool{}
	depends = func(job, ancestor string) bool {
		if visiting[job] {
			t.Fatalf("workflow dependency cycle at %s", job)
		}
		visiting[job] = true
		defer delete(visiting, job)

		var names []string
		switch needs := workflow.Jobs[job].Needs.(type) {
		case string:
			names = append(names, needs)
		case []any:
			for _, name := range needs {
				names = append(names, name.(string))
			}
		}
		for _, name := range names {
			if name == ancestor || depends(name, ancestor) {
				return true
			}
		}
		return false
	}
	if !depends(publish, capture) || depends(publish, propose) || strings.Contains(workflow.Jobs[publish].If, "changed") {
		t.Fatal("unchanged and future inputs must reach idempotent publication without a proposal")
	}
	if !depends(verify, publish) || !depends(propose, verify) {
		t.Fatal("publication and anonymous verification must precede proposal creation")
	}
	if !strings.Contains(workflow.Jobs[capture].If, "github.ref == 'refs/heads/main'") {
		t.Fatal("privileged preparation can start outside trusted main")
	}
	if !strings.Contains(workflow.Jobs[propose].If, "needs."+capture+".outputs.matches-base == 'true'") {
		t.Fatal("a different Fedora base can create a qualification proposal")
	}
	verifiedLock := false
	for name, job := range workflow.Jobs {
		for i, step := range job.Steps {
			if name == verify && strings.HasPrefix(step.Uses, "docker/login-action@") {
				t.Fatal("anonymous verification shares registry credentials")
			}
			if strings.HasPrefix(step.Uses, "actions/create-github-app-token@") && name != propose {
				if !depends(name, propose) || !strings.Contains(job.If, "needs."+propose+".outputs.changed == 'true'") {
					t.Fatalf("future or unchanged preparation can mint a PR token in %s", name)
				}
			}
			if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && strings.HasPrefix(step.With["name"], "kernel-lock-") {
				verifiedLock = true
				if name != verify || i <= verifyStep {
					t.Fatal("lock artifact is available before anonymous verification")
				}
			}
		}
	}
	if !verifiedLock {
		t.Fatal("verified locks have no Actions handoff")
	}
}
