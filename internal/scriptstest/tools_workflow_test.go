package scriptstest

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/katl-dev/katl/internal/workstationtest"
)

const (
	setupTools = ".github/actions/setup-tools"
	ciShell    = ".github/scripts/ci-shell"
	ciShellRun = "bash " + ciShell + " bash --noprofile --norc -eo pipefail {0}"
)

// Host services stay on the runner's packages; every command-line tool comes
// from the flake's ci shell.
var hostServicePackages = []string{
	"qemu-system-x86", "qemu-utils", "libvirt-daemon-system", "libvirt-clients", "ovmf",
	"acl", "dnsmasq-base",
}

type toolJob struct {
	Defaults struct {
		Run struct{ Shell string } `yaml:"run"`
	} `yaml:"defaults"`
	Steps []workflowStep `yaml:"steps"`
}

func readToolJobs(t *testing.T, name string) map[string]toolJob {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]toolJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return workflow.Jobs
}

func readActionSteps(t *testing.T, action string) []workflowStep {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), action, "action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Runs struct{ Steps []workflowStep } `yaml:"runs"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse %s: %v", action, err)
	}
	return file.Runs.Steps
}

// usesAction reports whether step uses the repository's action at dir. A run
// for an explicit source ref takes the action from the workflow's own
// revision, checked out under _build/workflow-actions.
func usesAction(step workflowStep, dir string) bool {
	action, _, _ := strings.Cut(step.Uses, "@")
	return action == "./"+dir || action == "./_build/workflow-actions/"+dir
}

func usesSetupTools(step workflowStep) bool {
	return usesAction(step, setupTools)
}

var aptInstall = regexp.MustCompile(`hosted-apt-install|\bapt-get\b|\bapt\s+install\b`)

// hostToolSetup describes a step that would give a job tools from outside the
// ci shell, or returns "".
func hostToolSetup(step workflowStep) string {
	action, _, _ := strings.Cut(step.Uses, "@")
	switch {
	case action == "actions/setup-go" || strings.HasSuffix(action, "/.github/actions/setup-go"):
		return "uses " + step.Uses + "; the ci shell provides Go"
	case aptInstall.MatchString(step.Run):
		return "installs packages with apt; the ci shell provides tools"
	}
	return ""
}

// The plan job sets up tools only when it computes the plan itself, and runs
// only the planner in the shell; TestVMPlanRunsPlannerInCIShell covers it.
var ciShellExceptions = []string{"hosted-vmtests.yml/plan"}

// ciShellJobs must set up the ci shell. Discovery checks every job that does,
// but a job that went back to setup-go would no longer be discovered.
var ciShellJobs = []string{
	"ci.yml/checks",
	"hosted-vmtests.yml/plan",
	"hosted-vmtests.yml/images",
	"hosted-vmtests.yml/scenarios",
	"kernel-qualification.yml/drivers",
	"production-vmtests.yml/qualification",
	"release-artifacts.yml/unit",
}

// Every job that sets up the ci shell takes its Go and test tools from it, so
// a test sees the versions it sees in the development shell.
func TestToolJobsUseCIShell(t *testing.T) {
	workflows, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, workflow := range workflows {
		for name, job := range readToolJobs(t, filepath.Base(workflow)) {
			id := filepath.Base(workflow) + "/" + name
			setup := slices.IndexFunc(job.Steps, usesSetupTools)
			if setup < 0 {
				continue
			}
			found = append(found, id)
			if slices.Contains(ciShellExceptions, id) {
				continue
			}
			t.Run(id, func(t *testing.T) {
				if job.Defaults.Run.Shell != ciShellRun {
					t.Errorf("default run shell = %q, want %q", job.Defaults.Run.Shell, ciShellRun)
				}
				for i, step := range job.Steps {
					if problem := hostToolSetup(step); problem != "" {
						t.Errorf("step %d %s", i, problem)
					}
					if step.Run != "" && i < setup {
						t.Errorf("step %d runs before %s", i, setupTools)
					}
					// A step that runs after a failure may follow a shell that
					// could not be realised, so it runs in plain bash; every other
					// step runs in the ci shell.
					always := strings.Contains(step.If, "always()")
					if step.Run != "" && always && step.Shell != "bash" {
						t.Errorf("step %d runs on failure with shell %q, want bash", i, step.Shell)
					}
					if step.Shell != "" && !always {
						t.Errorf("step %d overrides the ci shell with %q", i, step.Shell)
					}
				}
			})
		}
	}
	for _, id := range ciShellJobs {
		if !slices.Contains(found, id) {
			t.Errorf("%s does not use %s", id, setupTools)
		}
	}

	// The image jobs also run the mkosi builder action's steps.
	t.Run("mkosi-builder", func(t *testing.T) {
		for i, step := range readActionSteps(t, ".github/actions/mkosi-builder") {
			if problem := hostToolSetup(step); problem != "" {
				t.Errorf("step %d %s", i, problem)
			}
		}
	})
}

// setup-tools resolves its local actions, scripts and shell in the source
// checkout, even when it comes from the workflow's revision. A source ref that
// predates the ci shell fails with guidance before anything resolves there,
// instead of with a missing action, script or flake attribute.
func TestSetupToolsRejectsSourceWithoutCIShell(t *testing.T) {
	const predates = "::error::source ref predates the flake's ci shell; dispatch this workflow from that ref's branch"
	steps := readActionSteps(t, setupTools)
	check := slices.IndexFunc(steps, func(step workflowStep) bool { return strings.Contains(step.Run, predates) })
	if check != 0 {
		t.Fatalf("setup-tools checks the source at step %d, want 0", check)
	}
	bash := workstationtest.Tool(t, "bash")

	for _, tc := range []struct {
		name    string
		files   []string
		wantErr bool
	}{
		{"current", []string{ciShell, "flake.nix"}, false},
		{"no entry script", []string{"flake.nix"}, true},
		{"no flake", []string{ciShell}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			for _, file := range tc.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(work, file)), 0o755); err != nil {
					t.Fatal(err)
				}
				writeExecutable(t, filepath.Join(work, file), "#!/bin/sh\n")
			}

			// An empty PATH shows the check runs before Nix is installed.
			cmd := exec.Command(bash, "--noprofile", "--norc", "-eo", "pipefail", "-c", steps[check].Run)
			cmd.Dir = work
			cmd.Env = []string{"PATH=" + t.TempDir()}
			output, err := cmd.CombinedOutput()
			if gotErr := err != nil; gotErr != tc.wantErr || strings.Contains(string(output), predates) != tc.wantErr {
				t.Fatalf("check failed = %v, want %v\n%s", gotErr, tc.wantErr, output)
			}
		})
	}
}

// The plan job sets up tools only when it computes the plan itself, and then
// runs the planner in the ci shell.
func TestVMPlanRunsPlannerInCIShell(t *testing.T) {
	job := readToolJobs(t, "hosted-vmtests.yml")["plan"]
	if !slices.ContainsFunc(job.Steps, usesSetupTools) {
		t.Fatalf("plan job does not use %s", setupTools)
	}
	var planner int
	for _, step := range job.Steps {
		for _, line := range strings.Split(step.Run, "\n") {
			if !strings.Contains(line, "go run ") {
				continue
			}
			planner++
			if !strings.HasPrefix(strings.TrimSpace(line), ciShell+" go run ") {
				t.Errorf("%q runs Go outside the ci shell", strings.TrimSpace(line))
			}
		}
	}
	if planner == 0 {
		t.Fatal("plan job runs no Go command")
	}
}

// The VM host action installs only host services, and grants user namespaces
// to the Bubblewrap that the ci shell resolves.
func TestHostedVMTestInstallsOnlyHostServices(t *testing.T) {
	var installs, hermetic int
	for _, step := range readActionSteps(t, ".github/actions/hosted-vmtest") {
		script := strings.ReplaceAll(step.Run, "\\\n", " ")
		for _, line := range strings.Split(script, "\n") {
			fields := strings.Fields(line)
			if i := slices.Index(fields, ".github/scripts/hosted-apt-install"); i >= 0 {
				installs++
				for _, name := range fields[i+1:] {
					if !slices.Contains(hostServicePackages, name) {
						t.Errorf("hosted-vmtest installs %s with apt; tools come from the ci shell", name)
					}
				}
			}
			if strings.Contains(line, "hosted-hermetic-setup") {
				hermetic++
				if strings.TrimSpace(line) != ciShell+" .github/scripts/hosted-hermetic-setup" {
					t.Errorf("%q must run hosted-hermetic-setup through %s", strings.TrimSpace(line), ciShell)
				}
			}
		}
	}
	if installs == 0 || hermetic == 0 {
		t.Fatalf("found %d apt installs and %d hermetic setups, want at least one each", installs, hermetic)
	}
}

// ci-shell selects the flake's ci shell from the checkout that holds it, from
// any working directory, and disables remote builders.
func TestCIShellRunsCommandInFlakeShell(t *testing.T) {
	root := repoRoot(t)
	record := filepath.Join(t.TempDir(), "nix-args")
	output := runCIShell(t, record, "printf", "%s\\n", "go", "test ./...")

	want := []string{"develop", "--option", "builders", "", root + "#ci"}
	got := strings.Split(strings.TrimSuffix(string(mustReadFile(t, record)), "\n"), "\n")
	if !slices.Equal(got, want) {
		t.Fatalf("nix arguments = %q, want %q", got, want)
	}
	if output != "go\ntest ./...\n" {
		t.Fatalf("command output = %q, want its arguments unchanged", output)
	}
}

// The development shell exports SOURCE_DATE_EPOCH, but production build runs
// without it, so a command in the ci shell builds in the release's mode
// whether the variable comes from the shell or from the job.
func TestCIShellDropsSourceDateEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	output := runCIShell(t, filepath.Join(t.TempDir(), "nix-args"), "sh", "-c", `printf %s "${SOURCE_DATE_EPOCH-unset}"`)
	if output != "unset" {
		t.Fatalf("SOURCE_DATE_EPOCH in the ci shell = %q, want unset", output)
	}
}

// runCIShell runs a command through ci-shell with a nix that records its
// arguments before -c and, like nix develop, exports SOURCE_DATE_EPOCH to the
// command after it. It returns the command's output.
func runCIShell(t *testing.T, record string, command ...string) string {
	t.Helper()
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "nix"), `#!/bin/sh
: > "`+record+`"
while [ "$#" -gt 0 ] && [ "$1" != -c ]; do
    printf '%s\n' "$1" >> "`+record+`"
    shift
done
shift
SOURCE_DATE_EPOCH=315532800 exec "$@"
`)

	cmd := exec.Command(filepath.Join(repoRoot(t), ciShell), command...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("ci-shell: %v", err)
	}
	return string(output)
}

// hosted-vmtest-run re-enters the job user through sudo to refresh its
// groups. sudo replaces PATH with its secure_path, so the VM runner would
// otherwise see the host's tools instead of the ci shell's.
func TestHostedVMTestRunKeepsCallerPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root runs scripts/vmtest-run without sudo")
	}
	dir := t.TempDir()
	securePath := filepath.Join(dir, "secure")
	sudoDir := filepath.Join(dir, "sudo")
	shellDir := filepath.Join(dir, "shell")
	work := filepath.Join(dir, "work")
	for _, path := range []string{securePath, sudoDir, shellDir, filepath.Join(work, "scripts")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := workstationtest.Tool(t, "env")
	if err := os.Symlink(env, filepath.Join(securePath, "env")); err != nil {
		t.Fatal(err)
	}
	// Model sudo's secure_path: options are accepted and PATH is replaced
	// whether or not the environment is otherwise preserved.
	writeExecutable(t, filepath.Join(sudoDir, "sudo"), `#!/bin/sh
while [ "$#" -gt 0 ]; do
    case "$1" in
        --preserve-env|--user=*) shift ;;
        *) break ;;
    esac
done
PATH=`+securePath+` exec "$@"
`)
	tool := filepath.Join(shellDir, "katl-shell-tool")
	writeExecutable(t, tool, "#!/bin/sh\n")
	report := filepath.Join(dir, "report")
	writeExecutable(t, filepath.Join(work, "scripts", "vmtest-run"),
		"#!/usr/bin/env bash\nprintf '%s\\n' \"$(command -v katl-shell-tool)\" \"$*\" > \""+report+"\"\n")

	var hostDirs []string
	for _, name := range []string{"bash", "id"} {
		hostDirs = append(hostDirs, filepath.Dir(workstationtest.Tool(t, name)))
	}
	cmd := exec.Command(filepath.Join(repoRoot(t), ".github", "scripts", "hosted-vmtest-run"), "./internal/vmtest", "-run", "^TestX$")
	cmd.Dir = work
	path := strings.Join(append([]string{sudoDir, shellDir}, hostDirs...), string(os.PathListSeparator))
	cmd.Env = append(os.Environ(), "PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hosted-vmtest-run: %v\n%s", err, output)
	}

	got := strings.Split(strings.TrimSuffix(string(mustReadFile(t, report)), "\n"), "\n")
	want := []string{tool, "./internal/vmtest -run ^TestX$"}
	if !slices.Equal(got, want) {
		t.Fatalf("vmtest-run under sudo saw %q, want %q", got, want)
	}
}

var nixCommand = regexp.MustCompile(`(^|[\s;&|(])nix\s+(build|develop|shell|run|copy|eval|flake|path-info|profile|store)\b`)

type ciStep struct {
	path string
	workflowStep
}

// ciSteps returns every step of every workflow and composite action.
func ciSteps(t *testing.T) []ciStep {
	t.Helper()
	root := os.DirFS(repoRoot(t))
	var steps []ciStep
	for _, dir := range []string{".github/workflows", ".github/actions"} {
		err := fs.WalkDir(root, dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ext := filepath.Ext(path); entry.IsDir() || (ext != ".yml" && ext != ".yaml") {
				return nil
			}
			data, err := fs.ReadFile(root, path)
			if err != nil {
				return err
			}
			var file struct {
				Jobs map[string]struct{ Steps []workflowStep } `yaml:"jobs"`
				Runs struct{ Steps []workflowStep }            `yaml:"runs"`
			}
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			fileSteps := file.Runs.Steps
			for _, job := range file.Jobs {
				fileSteps = append(fileSteps, job.Steps...)
			}
			for _, step := range fileSteps {
				steps = append(steps, ciStep{path, step})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return steps
}

// Hosted runners have no remote builders, and a builders setting inherited
// from elsewhere would send work to a machine the job cannot reach.
func TestWorkflowNixCommandsDisableBuilders(t *testing.T) {
	var commands int
	for _, step := range ciSteps(t) {
		for _, line := range strings.Split(step.Run, "\n") {
			for _, match := range nixCommand.FindAllStringIndex(line, -1) {
				commands++
				if !strings.HasPrefix(strings.TrimLeft(line[match[1]:], " "), "--option builders ''") {
					t.Errorf("%s: %q must pass --option builders '' right after the subcommand", step.path, strings.TrimSpace(line))
				}
			}
		}
	}
	if commands == 0 {
		t.Fatal("no nix commands found in workflows or actions")
	}
}

// The install-nix action owns CI's Nix configuration, so a job that installs
// Nix another way would run without its substituter and network timeouts.
func TestWorkflowsInstallNixThroughOwner(t *testing.T) {
	const owner = ".github/actions/install-nix/action.yml"
	var installs int
	for _, step := range ciSteps(t) {
		action, _, _ := strings.Cut(step.Uses, "@")
		if action != "cachix/install-nix-action" {
			continue
		}
		installs++
		if step.path != owner {
			t.Errorf("%s uses %s; install Nix with ./.github/actions/install-nix", step.path, action)
		}
	}
	if installs != 1 {
		t.Errorf("found %d cachix/install-nix-action steps; want exactly one, in %s", installs, owner)
	}
}

// The diagnostics step stages VM evidence with a Go program, which must build
// with the ci shell's Go like every other Go command in these jobs. The step
// also runs after a failed setup, when the shell may be missing or
// unrealised; it must then succeed without downloading the shell or staging
// evidence, so the job keeps the outcome of the step that failed.
func TestDiagnosticsStageEvidenceInCIShell(t *testing.T) {
	bash := workstationtest.Tool(t, "bash")
	root := repoRoot(t)

	for _, tc := range []struct {
		name       string
		nix        bool
		realised   bool
		wantStaged bool
	}{
		{name: "shell realised", nix: true, realised: true, wantStaged: true},
		{name: "shell not realised", nix: true},
		{name: "nix not installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			scripts := filepath.Join(work, ".github", "scripts")
			if err := os.MkdirAll(scripts, 0o755); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(scripts, "ci-shell"), string(mustReadFile(t, filepath.Join(root, ciShell))))

			bin := t.TempDir()
			for _, tool := range []string{"bash", "cat", "cp", "df", "dirname", "env", "mkdir", "timeout", "true"} {
				if err := os.Symlink(workstationtest.Tool(t, tool), filepath.Join(bin, tool)); err != nil {
					t.Fatal(err)
				}
			}
			writeExecutable(t, filepath.Join(bin, "sudo"), "#!/bin/sh\nexit 1\n")
			goCalls := filepath.Join(work, "go-calls")
			writeExecutable(t, filepath.Join(bin, "go"), `#!/bin/sh
printf '%s %s\n' "${IN_CI_SHELL:-host}" "$*" >> "`+goCalls+`"
`)
			// Like nix develop, the fake runs the command in the shell once it is
			// realised; before that it would download the shell's closure, unless
			// substitution is disabled.
			fetched := filepath.Join(work, "fetched")
			if tc.nix {
				writeExecutable(t, filepath.Join(bin, "nix"), `#!/bin/sh
if [ "$REALISED" != 1 ]; then
    case "$NIX_CONFIG" in
        *"substitute = false"*) echo 'cannot build the shell without substitution' >&2 ;;
        *) : > "`+fetched+`" ;;
    esac
    exit 1
fi
while [ "$1" != -c ]; do shift; done
shift
IN_CI_SHELL=ci-shell exec "$@"
`)
			}

			output := filepath.Join(work, "github-output")
			cmd := exec.Command(bash, filepath.Join(root, ".github", "scripts", "hosted-vmtest-diagnostics"))
			cmd.Dir = work
			cmd.Env = []string{"PATH=" + bin, "GITHUB_OUTPUT=" + output, "KATL_VMTEST_RUN_DIR=" + filepath.Join(work, "run")}
			if tc.realised {
				cmd.Env = append(cmd.Env, "REALISED=1")
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("diagnostics failed: %v\n%s", err, out)
			}

			var calls []string
			if data, err := os.ReadFile(goCalls); err == nil {
				calls = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			}
			// A step that stages nothing writes no outputs file.
			outputs, _ := os.ReadFile(output)
			staged := strings.Contains(string(outputs), "upload-dir=")
			if tc.wantStaged {
				if want := []string{"ci-shell run ./cmd/katl-vmtest-evidence _build/hosted-evidence " + filepath.Join(work, "run") + " _build/vmtest-upload"}; !slices.Equal(calls, want) {
					t.Errorf("go calls = %q, want %q", calls, want)
				}
				if !staged {
					t.Error("no upload-dir output for staged evidence")
				}
				return
			}
			if len(calls) != 0 || staged {
				t.Errorf("staged evidence without the ci shell: go calls %q, upload-dir output %v", calls, staged)
			}
			if !strings.Contains(string(out), "::warning title=VM evidence::") {
				t.Errorf("no warning that evidence was not staged:\n%s", out)
			}
			if _, err := os.Stat(fetched); err == nil {
				t.Error("diagnostics tried to download the ci shell")
			}
		})
	}
}
