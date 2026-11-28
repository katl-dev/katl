package scriptstest

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	vmtestRunDirEnv     = "KATL_VMTEST_RUN_DIR"
	workspacePrefix     = "${{ github.workspace }}/"
	redactedUploadPath  = "${{ steps.diagnostics.outputs.upload-dir }}"
	hostDiagnosticsPath = "_build/hosted-evidence"
)

type uploadJob struct {
	Env   map[string]string `yaml:"env"`
	Steps []workflowStep    `yaml:"steps"`
}

func readUploadJobs(t *testing.T, name string) map[string]uploadJob {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]uploadJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return workflow.Jobs
}

// unredactedUploads lists upload paths in VM test jobs that could reach the
// raw run directory or host diagnostics instead of the redacted staging
// directory.
func unredactedUploads(jobs map[string]uploadJob) []string {
	var problems []string
	for jobName, job := range jobs {
		runDir := job.Env[vmtestRunDirEnv]
		if runDir == "" {
			continue
		}
		protected := []string{strings.TrimPrefix(runDir, workspacePrefix), hostDiagnosticsPath}

		for i, step := range job.Steps {
			if !strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
				continue
			}
			for _, line := range strings.Split(step.With["path"], "\n") {
				line = strings.TrimSpace(line)
				if line != redactedUploadPath && uploadReaches(line, protected) {
					problems = append(problems, fmt.Sprintf("job %s step %d uploads %q", jobName, i, line))
				}
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func uploadReaches(pattern string, protected []string) bool {
	if pattern == "" || strings.HasPrefix(pattern, "!") {
		return false
	}
	pattern = strings.TrimPrefix(strings.TrimPrefix(pattern, workspacePrefix), "./")
	// Any other expression may resolve to a protected directory.
	if strings.Contains(pattern, "${{") || strings.Contains(pattern, vmtestRunDirEnv) {
		return true
	}

	prefix := pattern
	globbed := false
	if i := strings.IndexAny(pattern, "*?[{"); i >= 0 {
		prefix, globbed = pattern[:i], true
	}
	prefix = strings.TrimSuffix(prefix, "/")

	for _, dir := range protected {
		switch {
		case prefix == "" || prefix == "." || prefix == dir:
			return true
		case strings.HasPrefix(prefix, dir+"/") || strings.HasPrefix(dir, prefix+"/"):
			return true
		case globbed && strings.HasPrefix(dir, prefix):
			return true
		}
	}
	return false
}

// Raw VM run directories and host diagnostics can hold private keys, so VM
// test jobs upload only the staging directory that the diagnostics step has
// redacted.
func TestVMEvidenceUploadsRedacted(t *testing.T) {
	for _, name := range workflowNames(t) {
		jobs := readUploadJobs(t, name)
		for _, problem := range unredactedUploads(jobs) {
			t.Errorf("%s: %s", name, problem)
		}

		for jobName, job := range jobs {
			runDir := job.Env[vmtestRunDirEnv]
			if runDir == "" {
				continue
			}

			redactedUploads := 0
			for i, step := range job.Steps {
				if step.With["path"] != redactedUploadPath {
					continue
				}
				redactedUploads++

				for _, raw := range []string{
					strings.TrimPrefix(runDir, workspacePrefix) + "/**/*.json",
					hostDiagnosticsPath + "/",
				} {
					steps := slices.Clone(job.Steps)
					steps[i].With = maps.Clone(step.With)
					steps[i].With["path"] = raw
					mutated := map[string]uploadJob{jobName: {Env: job.Env, Steps: steps}}
					if len(unredactedUploads(mutated)) == 0 {
						t.Errorf("%s job %s: guard accepts raw upload path %q", name, jobName, raw)
					}
				}
			}
			if redactedUploads == 0 {
				t.Errorf("%s job %s sets %s but uploads no redacted evidence", name, jobName, vmtestRunDirEnv)
			}
		}
	}
}
