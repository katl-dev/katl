package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func runList(runs ...map[string]any) map[string]any {
	return map[string]any{"total_count": len(runs), "workflow_runs": append([]map[string]any{}, runs...)}
}

func ciRun(overrides map[string]any) map[string]any {
	run := map[string]any{
		"id": 7, "run_attempt": 2, "head_sha": testCommit, "head_branch": "gh-readonly-queue/main/pr-9-" + testCommit,
		"event": "merge_group", "path": ciWorkflowPath, "status": "completed", "conclusion": "success",
		"html_url": queueURL,
	}
	for key, value := range overrides {
		run[key] = value
	}
	return run
}

func branchRun(event, branch, status, conclusion string) map[string]any {
	return ciRun(map[string]any{
		"id": 8, "run_attempt": 1, "head_branch": branch, "event": event,
		"status": status, "conclusion": conclusion, "html_url": branchURL,
	})
}

func fastJob(conclusion string) map[string]any {
	return map[string]any{"name": "Fast checks", "status": "completed", "conclusion": conclusion}
}

func jobList(jobs ...map[string]any) map[string]any {
	return map[string]any{"total_count": len(jobs), "jobs": append([]map[string]any{}, jobs...)}
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

// vmJobs lists passing Fast checks and a plan, images and scenario job per
// flavour, then applies the override to the job list.
func vmJobs(change func([]map[string]any) []map[string]any) map[string]any {
	jobs := []map[string]any{fastJob("success")}
	for _, flavour := range []string{"standard", "lts"} {
		jobs = append(jobs,
			vmJob(flavour, "plan", "success", false),
			vmJob(flavour, "images", "success", false),
			vmJob(flavour, "VM / install", "success", false))
	}
	if change != nil {
		jobs = change(jobs)
	}
	return jobList(jobs...)
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
	regressionVersion = "2026.11.0-rc.1"
	releaseBranch     = "release/2026.11.0"
	queueRuns         = repoPrefix + "actions/workflows/ci.yml/runs?head_sha=" + testCommit + "&event=merge_group&per_page=100"
	branchRuns        = repoPrefix + "actions/workflows/ci.yml/runs?head_sha=" + testCommit + "&branch=release%2F2026.11.0&per_page=100"
	mainCompare       = repoPrefix + "compare/main..." + testCommit + "?per_page=1"
	queueJobs         = repoPrefix + "actions/runs/7/attempts/2/jobs?per_page=100"
	branchJobs        = repoPrefix + "actions/runs/8/attempts/1/jobs?per_page=100"
	queueURL          = "https://example.test/ci/7"
	branchURL         = "https://example.test/ci/8"

	fastMissing = "CI has not passed Fast checks at " + testCommit + " on release/2026.11.0; wait for CI, or dispatch CI on release/2026.11.0, then dispatch Release again"
	fastFailed  = "CI failed Fast checks at " + testCommit + " (" + branchURL + "); fix it or re-run CI, then dispatch Release again"
	unreadable  = "could not read CI evidence at " + testCommit + "; dispatch Release again"
)

func assertRegression(t *testing.T, f *fakeGitHub, version, want, wantErr string) {
	t.Helper()
	var stdout bytes.Buffer
	err := regression(context.Background(), []string{version, testCommit}, &stdout, newTestReader(f))
	if wantErr != "" {
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("error = %v, want %q", err, wantErr)
		}
		if stdout.Len() != 0 {
			t.Fatalf("a failed decision printed %q", stdout.String())
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&stdout)
	var got struct {
		Decision string   `json:"decision"`
		Flavours []string `json:"vm_flavours"`
		Evidence *string  `json:"evidence"`
	}
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("decode decision: %v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("expected one JSON object, next decode returned %v", err)
	}
	if got.Decision != want {
		t.Fatalf("decision = %q, want %q", got.Decision, want)
	}
	if got.Evidence == nil {
		t.Fatal("decision did not supply an evidence string")
	}
	if want == "skip" {
		if got.Flavours == nil || len(got.Flavours) != 0 || *got.Evidence != queueURL {
			t.Fatalf("skip needs an empty JSON array and the queue run URL, got %+v", got)
		}
	} else if !slices.Equal(got.Flavours, []string{"standard", "lts"}) || *got.Evidence != "" {
		t.Fatalf("run needs both flavours and empty VM evidence, got %+v", got)
	}
}

func TestRegressionVMEvidence(t *testing.T) {
	for _, test := range []struct {
		name      string
		overrides map[string]any
		want      string
	}{
		{"complete merge-queue CI", nil, "skip"},
		{"unreadable runs", map[string]any{queueRuns: errors.New("invalid JSON")}, "run"},
		{"failed run lookup", map[string]any{queueRuns: &apiError{status: 403, text: "forbidden"}}, "run"},
		{"push cannot supply VM evidence", map[string]any{queueRuns: runList(ciRun(map[string]any{"event": "push"}))}, "run"},
		{"another commit's run", map[string]any{queueRuns: runList(ciRun(map[string]any{"head_sha": strings.Repeat("a", 40)}))}, "run"},
		{"another workflow's run", map[string]any{queueRuns: runList(ciRun(map[string]any{"path": ".github/workflows/other.yml"}))}, "run"},
		{"release-branch group", map[string]any{queueRuns: runList(ciRun(map[string]any{"head_branch": "gh-readonly-queue/release/2026.11/pr-9"}))}, "run"},
		{"unfinished run", map[string]any{queueRuns: runList(ciRun(map[string]any{"status": "in_progress", "conclusion": nil}))}, "run"},
		{"failed run", map[string]any{queueRuns: runList(ciRun(map[string]any{"conclusion": "failure"}))}, "run"},
		{"group never merged", map[string]any{mainCompare: map[string]string{"status": "ahead"}}, "run"},
		{"docs-only merge-queue CI", map[string]any{queueJobs: jobList(fastJob("success"), map[string]any{"name": "VM (standard)", "conclusion": "skipped"})}, "run"},
		{"skipped flavour", map[string]any{queueJobs: vmJobs(without("VM (lts)"))}, "run"},
		{"missing scenario", map[string]any{queueJobs: vmJobs(without("VM (lts) / VM / "))}, "run"},
		{"missing images", map[string]any{queueJobs: vmJobs(without("VM (lts) / images"))}, "run"},
		{"failed scenario", map[string]any{queueJobs: vmJobs(replaced("VM (lts) / VM / install", vmJob("lts", "VM / install", "failure", false)))}, "run"},
		{"queue Fast checks skipped with passing branch Fast checks", map[string]any{queueJobs: vmJobs(replaced("Fast checks", fastJob("skipped")))}, "skip"},
		{"queue conclusion contradicts Fast checks", map[string]any{queueJobs: vmJobs(replaced("Fast checks", fastJob("failure")))}, "run"},
		{"skipped scenario", map[string]any{queueJobs: vmJobs(replaced("VM (standard) / VM / install", vmJob("standard", "VM / install", "skipped", false)))}, "run"},
		{"presubmit Kubernetes candidate", map[string]any{queueJobs: vmJobs(replaced("VM (standard) / images", vmJob("standard", "images", "success", true)))}, "run"},
		{"candidate step missing", map[string]any{queueJobs: vmJobs(replaced("VM (standard) / images", map[string]any{"name": "VM (standard) / images", "conclusion": "success"}))}, "run"},
		{"truncated jobs", map[string]any{queueJobs: map[string]any{"total_count": 150, "jobs": vmJobs(nil)["jobs"]}}, "run"},
		{"truncated runs", map[string]any{queueRuns: map[string]any{"total_count": 150, "workflow_runs": []any{ciRun(nil)}}}, "run"},
		{"ambiguous queue runs", map[string]any{queueRuns: runList(ciRun(nil), ciRun(map[string]any{"id": 9, "run_attempt": 1}))}, "run"},
		{"duplicate images jobs", map[string]any{queueJobs: vmJobs(func(jobs []map[string]any) []map[string]any {
			return append(jobs, vmJob("lts", "images", "success", false))
		})}, "run"},
		{"main itself", map[string]any{mainCompare: map[string]string{"status": "identical"}}, "skip"},
		{"diverged from main", map[string]any{mainCompare: map[string]string{"status": "diverged"}}, "run"},
		{"commit no longer found", map[string]any{mainCompare: &apiError{status: 404, text: "not found"}}, "run"},
	} {
		t.Run(test.name, func(t *testing.T) {
			responses := map[string]any{
				queueRuns: runList(ciRun(nil)), queueJobs: vmJobs(nil), mainCompare: map[string]string{"status": "behind"},
				branchRuns: runList(branchRun("push", releaseBranch, "completed", "success")), branchJobs: jobList(fastJob("success")),
			}
			for endpoint, response := range test.overrides {
				responses[endpoint] = response
			}
			assertRegression(t, &fakeGitHub{t: t, responses: responses}, regressionVersion, test.want, "")
		})
	}
}

func TestRegressionFastChecks(t *testing.T) {
	for _, test := range []struct {
		name, version string
		runs, jobs    map[string]any
		wantErr       string
	}{
		{"release branch push", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "success")), vmJobs(nil), ""},
		{"release branch dispatch", regressionVersion, runList(branchRun("workflow_dispatch", releaseBranch, "completed", "success")), vmJobs(nil), ""},
		{"final at a release-only commit", "2026.11.0", runList(branchRun("push", releaseBranch, "completed", "success")), jobList(fastJob("success")), ""},
		{"no run", regressionVersion, runList(), nil, fastMissing},
		{"pending run", regressionVersion, runList(branchRun("push", releaseBranch, "in_progress", "")), nil, fastMissing},
		{"Fast checks failed", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "failure")), jobList(fastJob("failure")), fastFailed},
		{"Fast checks skipped", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "success")), jobList(fastJob("skipped")), fastMissing},
		{"successful run without Fast checks", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "success")), jobList(), fastMissing},
		{"Fast checks passed in a failed run", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "failure")), jobList(fastJob("success")), fastMissing},
		{"push on another branch", regressionVersion, runList(branchRun("push", "main", "completed", "success")), nil, fastMissing},
		{"pull request from the release branch", regressionVersion, runList(branchRun("pull_request", releaseBranch, "completed", "success")), nil, fastMissing},
	} {
		t.Run(test.name, func(t *testing.T) {
			responses := map[string]any{queueRuns: runList(), branchRuns: test.runs}
			if test.jobs != nil {
				responses[branchJobs] = test.jobs
			}
			assertRegression(t, &fakeGitHub{t: t, responses: responses}, test.version, "run", test.wantErr)
		})
	}
}

func TestRegressionRequiresQueueFastChecksOrBranchEvidence(t *testing.T) {
	assertRegression(t, &fakeGitHub{t: t, responses: map[string]any{
		queueRuns: runList(ciRun(nil)), mainCompare: map[string]string{"status": "behind"},
		queueJobs: vmJobs(without("Fast checks")), branchRuns: runList(),
	}}, regressionVersion, "", fastMissing)
}

func TestRegressionRequiresLandedQueueCommitForFastChecks(t *testing.T) {
	assertRegression(t, &fakeGitHub{t: t, responses: map[string]any{
		queueRuns: runList(ciRun(nil)), mainCompare: map[string]string{"status": "ahead"},
		queueJobs: vmJobs(nil), branchRuns: runList(),
	}}, regressionVersion, "", fastMissing)
}

func TestRegressionRetries(t *testing.T) {
	unavailable := &apiError{status: 503, text: "gh: Service Unavailable (HTTP 503)"}
	passingBranch := runList(branchRun("push", releaseBranch, "completed", "success"))
	for _, test := range []struct {
		name, endpoint string
		failures       int
		branch         map[string]any
		want, wantErr  string
	}{
		{"transient jobs read then success", queueJobs, 1, runList(), "skip", ""},
		{"persistent VM read failure with branch Fast checks", queueJobs, 6, passingBranch, "run", ""},
		{"persistent Fast checks read failure", branchJobs, 6, passingBranch, "", unreadable},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &fakeGitHub{t: t, responses: map[string]any{
				queueRuns: runList(ciRun(nil)), mainCompare: map[string]string{"status": "behind"},
				queueJobs: vmJobs(nil), branchRuns: test.branch, branchJobs: jobList(fastJob("success")),
			}, failures: map[string][]error{test.endpoint: slices.Repeat([]error{unavailable}, test.failures)}}
			if test.endpoint == branchJobs {
				f.responses[queueRuns] = runList()
			}
			assertRegression(t, f, regressionVersion, test.want, test.wantErr)
			if left := len(f.failures[test.endpoint]); left != 0 {
				t.Fatalf("returned with %d unread transient failures", left)
			}
		})
	}
}

func TestRegressionRequiresFullCommit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"regression", regressionVersion, "main"}, &stdout, &stderr, nil); err == nil {
		t.Fatal("a branch name was accepted as the commit")
	}
}

func TestRegressionRequiresVersion(t *testing.T) {
	assertRegression(t, &fakeGitHub{t: t}, "2026.11", "", "invalid release version")
}

func TestRequireVMRegressionWithCallerPrefix(t *testing.T) {
	payload, err := json.Marshal(vmJobs(func(jobs []map[string]any) []map[string]any {
		for _, job := range jobs {
			name := job["name"].(string)
			job["name"] = strings.Replace(name, "VM (", "Release VM (", 1)
		}
		return jobs
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response struct{ Jobs []ciJob }
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	if err := requireVMRegression(response.Jobs, "Release VM (%s) / "); err != nil {
		t.Fatal(err)
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
	for _, want := range []string{"name: " + fastChecksJob, "name: VM (${{ matrix.flavour }})", "needs.plan.outputs.kubernetes-version"} {
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
