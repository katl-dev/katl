package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type regressionAPI map[string]any

func (a regressionAPI) call(name string, args ...string) ([]byte, error) {
	value, ok := a["repos/"+repository+"/"+strings.TrimPrefix(args[1], "repos/"+repository+"/")]
	if name != "gh" || args[0] != "api" || !ok {
		return nil, fmt.Errorf("unavailable: %q", args)
	}
	if err, isError := value.(error); isError {
		return nil, err
	}
	return json.Marshal(value)
}

func runList(runs ...map[string]any) map[string]any {
	return map[string]any{"workflow_runs": runs}
}

func ciRun(overrides map[string]any) map[string]any {
	run := map[string]any{
		"id": 7, "head_sha": testCommit, "head_branch": "gh-readonly-queue/main/pr-9-" + testCommit,
		"event": "merge_group", "path": ciWorkflowPath, "status": "completed", "conclusion": "success",
		"html_url": "https://example.test/ci/7",
	}
	for key, value := range overrides {
		run[key] = value
	}
	return run
}

func rcRun(tag string, overrides map[string]any) map[string]any {
	run := map[string]any{
		"id": 8, "head_sha": testCommit, "head_branch": tag, "event": "push", "path": workflow,
		"status": "completed", "conclusion": "success", "html_url": "https://example.test/rc/8",
	}
	for key, value := range overrides {
		run[key] = value
	}
	return run
}

func vmJob(flavour, called, conclusion string, candidate bool) map[string]any {
	step := "skipped"
	if candidate {
		step = "success"
	}
	return map[string]any{
		"name": "VM (" + flavour + ") / " + called, "conclusion": conclusion,
		"steps": []map[string]any{{"name": "Resolve Kubernetes candidate", "conclusion": step}},
	}
}

// vmJobs lists a passing plan, images and scenario job per flavour, then
// applies the override to the job list.
func vmJobs(change func([]map[string]any) []map[string]any) map[string]any {
	var jobs []map[string]any
	for _, flavour := range []string{"standard", "lts"} {
		jobs = append(jobs,
			vmJob(flavour, "plan", "success", false),
			vmJob(flavour, "images", "success", false),
			vmJob(flavour, "VM / install", "success", false))
	}
	if change != nil {
		jobs = change(jobs)
	}
	return map[string]any{"total_count": len(jobs), "jobs": jobs}
}

func without(prefix string) func([]map[string]any) []map[string]any {
	return func(jobs []map[string]any) []map[string]any {
		var kept []map[string]any
		for _, job := range jobs {
			if !strings.HasPrefix(job["name"].(string), prefix) {
				kept = append(kept, job)
			}
		}
		return kept
	}
}

func replaced(name string, job map[string]any) func([]map[string]any) []map[string]any {
	return func(jobs []map[string]any) []map[string]any {
		for i := range jobs {
			if jobs[i]["name"] == name {
				jobs[i] = job
			}
		}
		return jobs
	}
}

const (
	ciRuns      = "repos/" + repository + "/actions/workflows/ci.yml/runs?head_sha=" + testCommit + "&event=merge_group&status=success&per_page=100"
	releaseRuns = "repos/" + repository + "/actions/workflows/release-artifacts.yml/runs?head_sha=" + testCommit + "&event=push&status=success&per_page=100"
	mainCompare = "repos/" + repository + "/compare/main..." + testCommit
	ciJobs      = "repos/" + repository + "/actions/runs/7/jobs?filter=latest&per_page=100"
)

func TestRegression(t *testing.T) {
	merged := regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: vmJobs(nil), mainCompare: map[string]string{"status": "behind"}}
	withCandidate := func(api regressionAPI, runs ...map[string]any) regressionAPI {
		out := regressionAPI{releaseRuns: runList(runs...)}
		for key, value := range api {
			out[key] = value
		}
		return out
	}

	tests := []struct {
		name    string
		version string
		api     regressionAPI
		want    string
		wantErr string
	}{
		{"rc after merge-queue CI", "2026.11.0-rc.1", merged, "skip", ""},
		{"rc without CI run", "2026.11.0-rc.1", regressionAPI{ciRuns: runList()}, "run", ""},
		{"rc with unreadable API", "2026.11.0-rc.1", regressionAPI{}, "run", ""},
		{"rc with failed API", "2026.11.0-rc.1", regressionAPI{ciRuns: fmt.Errorf("rate limited")}, "run", ""},
		{"rc with a push run only", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(map[string]any{"event": "push"}))}, "run", ""},
		{"rc with another commit's run", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(map[string]any{"head_sha": strings.Repeat("a", 40)}))}, "run", ""},
		{"rc with another workflow's run", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(map[string]any{"path": ".github/workflows/other.yml"}))}, "run", ""},
		{"rc with a release-branch group", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(map[string]any{"head_branch": "gh-readonly-queue/release/2026.11/pr-9"})), ciJobs: vmJobs(nil), mainCompare: map[string]string{"status": "behind"}}, "run", ""},
		{"rc with an unfinished run", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(map[string]any{"status": "in_progress", "conclusion": nil}))}, "run", ""},
		{"rc whose group never merged", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: vmJobs(nil), mainCompare: map[string]string{"status": "ahead"}}, "run", ""},
		{"rc after docs-only merge-queue CI", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: map[string]any{"total_count": 2, "jobs": []map[string]any{{"name": "Plan", "conclusion": "success"}, {"name": "VM (standard)", "conclusion": "skipped"}}}, mainCompare: map[string]string{"status": "behind"}}, "run", ""},
		{"rc with a skipped flavour", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: vmJobs(without("VM (lts)")), mainCompare: map[string]string{"status": "behind"}}, "run", ""},
		{"rc with a failed scenario", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: vmJobs(replaced("VM (lts) / VM / install", vmJob("lts", "VM / install", "failure", false))), mainCompare: map[string]string{"status": "behind"}}, "run", ""},
		{"rc with a skipped scenario", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: vmJobs(replaced("VM (standard) / VM / install", vmJob("standard", "VM / install", "skipped", false))), mainCompare: map[string]string{"status": "behind"}}, "run", ""},
		{"rc with a presubmit Kubernetes candidate", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: vmJobs(replaced("VM (standard) / images", vmJob("standard", "images", "success", true))), mainCompare: map[string]string{"status": "behind"}}, "run", ""},
		{"rc without the candidate step recorded", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: vmJobs(replaced("VM (standard) / images", map[string]any{"name": "VM (standard) / images", "conclusion": "success"})), mainCompare: map[string]string{"status": "behind"}}, "run", ""},
		{"rc with an unreadable job list", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: fmt.Errorf("rate limited"), mainCompare: map[string]string{"status": "behind"}}, "run", ""},
		{"rc with a truncated job list", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: map[string]any{"total_count": 150, "jobs": vmJobs(nil)["jobs"]}, mainCompare: map[string]string{"status": "behind"}}, "run", ""},
		{"rc at main itself", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: vmJobs(nil), mainCompare: map[string]string{"status": "identical"}}, "skip", ""},
		{"rc diverged from main", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: vmJobs(nil), mainCompare: map[string]string{"status": "diverged"}}, "run", ""},
		{"rc with a failed compare", "2026.11.0-rc.1", regressionAPI{ciRuns: runList(ciRun(nil)), ciJobs: vmJobs(nil), mainCompare: fmt.Errorf("not found")}, "run", ""},
		{"beta never skips", "2026.11.0-beta.1", merged, "run", ""},
		{"dev never skips", "2026.11.1-dev.0", merged, "run", ""},
		{"final after rc and CI", "2026.11.0", withCandidate(merged, rcRun("v2026.11.0-rc.2", nil)), "skip", ""},
		{"final after rc without CI", "2026.11.0", withCandidate(regressionAPI{ciRuns: runList()}, rcRun("v2026.11.0-rc.1", nil)), "run", ""},
		{"final without rc run", "2026.11.0", withCandidate(merged), "", "tag a candidate"},
		{"final with another version's rc", "2026.11.0", withCandidate(merged, rcRun("v2026.11.1-rc.1", nil)), "", "tag a candidate"},
		{"final with a rc-like tag of the version", "2026.11.0", withCandidate(merged, rcRun("v2026.11.0-rc.1x", nil)), "", "tag a candidate"},
		{"final with a manual rc run", "2026.11.0", withCandidate(merged, rcRun("v2026.11.0-rc.1", map[string]any{"event": "workflow_dispatch"})), "", "tag a candidate"},
		{"final with unreadable rc lookup", "2026.11.0", merged, "", "cannot look up"},
		{"invalid version", "2026.11", merged, "", "invalid release version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run([]string{"regression", test.version, testCommit}, &stdout, &stderr, test.api.call)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				if stdout.Len() != 0 {
					t.Fatalf("a failed decision printed %q", stdout.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(stdout.String()); got != test.want {
				t.Fatalf("decision = %q, want %q (%s)", got, test.want, stderr.String())
			}
		})
	}
}

func TestRegressionRequiresFullCommit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"regression", "2026.11.0-rc.1", "main"}, &stdout, &stderr, regressionAPI{}.call); err == nil {
		t.Fatal("a branch name was accepted as the commit")
	}
}

// The job and step names the decision reads must still exist in the workflows.
func TestRegressionJobNamesMatchWorkflows(t *testing.T) {
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	ci, hosted := read("ci.yml"), read("hosted-vmtests.yml")
	for _, want := range []string{"name: VM (${{ matrix.flavour }})", "needs.plan.outputs.kubernetes-version"} {
		if !strings.Contains(ci, want) {
			t.Errorf("ci.yml lacks %q", want)
		}
	}
	for _, want := range []string{"\n  " + kubernetesCandJob + ":\n", "name: " + kubernetesCandStep, "name: VM / ${{ matrix.name }}"} {
		if !strings.Contains(hosted, want) {
			t.Errorf("hosted-vmtests.yml lacks %q", want)
		}
	}
}
