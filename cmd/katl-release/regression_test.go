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

	fastMissing     = "CI has not passed Fast checks at " + testCommit + " on release/2026.11.0; wait for CI, or dispatch CI on release/2026.11.0, then dispatch Release again"
	fastFailed      = "CI failed Fast checks at " + testCommit + " (" + branchURL + "); fix it or re-run CI, then dispatch Release again"
	queueFastFailed = "CI failed Fast checks at " + testCommit + " (" + queueURL + ")"
	unreadable      = "could not read CI evidence at " + testCommit + "; dispatch Release again"
	unrecoverable   = "CI evidence at " + testCommit + " cannot be read, and reading it again will fail the same way"
	dispatchCI      = "dispatch CI on release/2026.11.0, then dispatch Release again, or report a katl-release defect"
)

// paged is a listing that GitHub returns as several pages.
type paged []any

// regressionGitHub serves the CI run and job listings as paginated reads;
// errors and paged values stay as given.
func regressionGitHub(t *testing.T, responses map[string]any) *fakeGitHub {
	f := &fakeGitHub{t: t, responses: map[string]any{}, pages: map[string][]any{}, failures: map[string][]error{}}
	for endpoint, response := range responses {
		_, isErr := response.(error)
		listing := strings.Contains(endpoint, "/runs?") || strings.Contains(endpoint, "/jobs?")
		switch pages, isPaged := response.(paged); {
		case isPaged:
			f.pages[endpoint] = pages
		case listing && !isErr:
			f.pages[endpoint] = []any{response}
		default:
			f.responses[endpoint] = response
		}
	}
	return f
}

func assertRegression(t *testing.T, f *fakeGitHub, version, want, wantErr string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := regression(context.Background(), []string{version, testCommit}, &stdout, &stderr, newTestReader(f))
	if wantErr != "" {
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("error = %v, want %q", err, wantErr)
		}
		if stdout.Len() != 0 {
			t.Fatalf("a failed decision printed %q", stdout.String())
		}
		return err.Error()
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
	return stderr.String()
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
		{"run listing not found", map[string]any{queueRuns: &apiError{status: 404, text: "not found"}}, "run"},
		{"job listing not found", map[string]any{queueJobs: &apiError{status: 404, text: "not found"}}, "run"},
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
		{"job count contradicts listing", map[string]any{queueJobs: map[string]any{"total_count": 150, "jobs": vmJobs(nil)["jobs"]}}, "run"},
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
			assertRegression(t, regressionGitHub(t, responses), regressionVersion, test.want, "")
		})
	}
}

// A run decision names the run that supplied Fast checks and why no VM
// evidence was taken.
func TestRegressionReason(t *testing.T) {
	for _, test := range []struct {
		name         string
		overrides    map[string]any
		fast, reason string
	}{
		{"queue Fast checks with incomplete VM evidence and no branch run",
			map[string]any{queueJobs: vmJobs(without("VM (lts)")), branchRuns: runList()},
			queueURL, "merge-queue CI run " + queueURL + " supplied no complete VM regression: CI did not run the lts VM images and scenarios"},
		{"docs-only merge-queue CI",
			map[string]any{queueJobs: jobList(fastJob("success"), map[string]any{"name": "VM (standard)", "conclusion": "skipped"})},
			queueURL, "merge-queue CI run " + queueURL + " supplied no complete VM regression"},
		{"ambiguous queue runs",
			map[string]any{queueRuns: runList(ciRun(nil), ciRun(map[string]any{"id": 9, "run_attempt": 1}))},
			queueURL, "2 merge-queue CI runs tested this commit"},
		{"unreadable queue runs",
			map[string]any{queueRuns: &apiError{status: 403, text: "forbidden"}},
			branchURL, "could not read the merge-queue CI runs: forbidden"},
		{"unreadable queue jobs",
			map[string]any{queueJobs: &apiError{status: 404, text: "not found"}},
			branchURL, "could not read the jobs of merge-queue CI run " + queueURL},
		{"failed queue run",
			map[string]any{queueRuns: runList(ciRun(map[string]any{"conclusion": "failure"}))},
			queueURL, "merge-queue CI run " + queueURL + ` concluded "failure"`},
		{"no queue run",
			map[string]any{queueRuns: runList()},
			branchURL, "no merge-queue CI run tested this commit as it landed on main"},
	} {
		t.Run(test.name, func(t *testing.T) {
			responses := map[string]any{
				queueRuns: runList(ciRun(nil)), queueJobs: vmJobs(nil), mainCompare: map[string]string{"status": "behind"},
				branchRuns: runList(branchRun("push", releaseBranch, "completed", "success")), branchJobs: jobList(fastJob("success")),
			}
			for endpoint, response := range test.overrides {
				responses[endpoint] = response
			}
			stderr := assertRegression(t, regressionGitHub(t, responses), regressionVersion, "run", "")
			if want := "Fast checks passed in " + test.fast + "\n"; !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
			if want := "Running VM regression: " + test.reason; !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
		})
	}
}

func TestRegressionPagesListings(t *testing.T) {
	var others []any
	for id := range 100 {
		others = append(others, ciRun(map[string]any{"id": 100 + id, "event": "push"}))
	}
	jobs := vmJobs(nil)["jobs"].([]map[string]any)
	assertRegression(t, regressionGitHub(t, map[string]any{
		queueRuns: paged{
			map[string]any{"total_count": 101, "workflow_runs": others},
			map[string]any{"total_count": 101, "workflow_runs": []any{ciRun(nil)}},
		},
		queueJobs: paged{
			map[string]any{"total_count": len(jobs), "jobs": jobs[:4]},
			map[string]any{"total_count": len(jobs), "jobs": jobs[4:]},
		},
		mainCompare: map[string]string{"status": "behind"},
	}), regressionVersion, "skip", "")
}

func TestRegressionFastChecks(t *testing.T) {
	forbidden := &apiError{status: 403, text: "gh: Resource not accessible by integration (HTTP 403)"}
	for _, test := range []struct {
		name, version string
		runs, jobs    any
		wantErr       []string
	}{
		{"release branch push", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "success")), vmJobs(nil), nil},
		{"release branch dispatch", regressionVersion, runList(branchRun("workflow_dispatch", releaseBranch, "completed", "success")), vmJobs(nil), nil},
		{"final at a release-only commit", "2026.11.0", runList(branchRun("push", releaseBranch, "completed", "success")), jobList(fastJob("success")), nil},
		{"Fast checks passed in a failed run", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "failure")), jobList(fastJob("success")), nil},
		{"no run", regressionVersion, runList(), nil, []string{fastMissing}},
		{"pending run", regressionVersion, runList(branchRun("push", releaseBranch, "in_progress", "")), nil, []string{fastMissing}},
		{"Fast checks failed", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "failure")), jobList(fastJob("failure")), []string{fastFailed}},
		{"Fast checks skipped", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "success")), jobList(fastJob("skipped")), []string{fastMissing}},
		{"successful run without Fast checks", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "success")), jobList(), []string{fastMissing}},
		{"push on another branch", regressionVersion, runList(branchRun("push", "main", "completed", "success")), nil, []string{fastMissing}},
		{"pull request from the release branch", regressionVersion, runList(branchRun("pull_request", releaseBranch, "completed", "success")), nil, []string{fastMissing}},
		{"run listing refused", regressionVersion, forbidden, nil, []string{unrecoverable, "Resource not accessible", dispatchCI}},
		{"run listing not found", regressionVersion, &apiError{status: 404, text: "not found"}, nil, []string{unrecoverable, dispatchCI}},
		{"job listing not found", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "success")), &apiError{status: 404, text: "not found"}, []string{unrecoverable, dispatchCI}},
		{"run without its attempt", regressionVersion, runList(ciRun(map[string]any{"id": 8, "run_attempt": 0, "head_branch": releaseBranch, "event": "push"})), nil, []string{unrecoverable, "did not identify its attempt", dispatchCI}},
		{"duplicate Fast checks jobs", regressionVersion, runList(branchRun("push", releaseBranch, "completed", "success")), jobList(fastJob("success"), fastJob("success")), []string{unrecoverable, "multiple Fast checks jobs", dispatchCI}},
	} {
		t.Run(test.name, func(t *testing.T) {
			responses := map[string]any{queueRuns: runList(), branchRuns: test.runs}
			if test.jobs != nil {
				responses[branchJobs] = test.jobs
			}
			wantErr := ""
			if len(test.wantErr) > 0 {
				wantErr = test.wantErr[0]
			}
			message := assertRegression(t, regressionGitHub(t, responses), test.version, "run", wantErr)
			for _, want := range test.wantErr[min(1, len(test.wantErr)):] {
				if !strings.Contains(message, want) {
					t.Errorf("error = %q, want %q", message, want)
				}
			}
			if wantErr == "" && !strings.Contains(message, "Fast checks passed in "+branchURL+"\n") {
				t.Errorf("stderr = %q, want the branch run as Fast checks evidence", message)
			}
		})
	}
}

func TestRegressionQueueFastChecksFailed(t *testing.T) {
	assertRegression(t, regressionGitHub(t, map[string]any{
		queueRuns: runList(ciRun(nil)), mainCompare: map[string]string{"status": "behind"},
		queueJobs: vmJobs(replaced("Fast checks", fastJob("failure"))), branchRuns: runList(),
	}), regressionVersion, "", queueFastFailed)
}

func TestRegressionUnexpectedComparison(t *testing.T) {
	message := assertRegression(t, regressionGitHub(t, map[string]any{
		queueRuns: runList(ciRun(nil)), mainCompare: map[string]string{"status": "unknown"}, branchRuns: runList(),
	}), regressionVersion, "", unrecoverable)
	if !strings.Contains(message, `unexpected comparison status "unknown"`) || !strings.Contains(message, dispatchCI) {
		t.Fatalf("error = %q, want the comparison status and the next step", message)
	}
}

func TestRegressionRequiresQueueFastChecksOrBranchEvidence(t *testing.T) {
	assertRegression(t, regressionGitHub(t, map[string]any{
		queueRuns: runList(ciRun(nil)), mainCompare: map[string]string{"status": "behind"},
		queueJobs: vmJobs(without("Fast checks")), branchRuns: runList(),
	}), regressionVersion, "", fastMissing)
}

func TestRegressionRequiresLandedQueueCommitForFastChecks(t *testing.T) {
	assertRegression(t, regressionGitHub(t, map[string]any{
		queueRuns: runList(ciRun(nil)), mainCompare: map[string]string{"status": "ahead"},
		queueJobs: vmJobs(nil), branchRuns: runList(),
	}), regressionVersion, "", fastMissing)
}

func TestRegressionRetries(t *testing.T) {
	unavailable := &apiError{status: 503, text: "gh: Service Unavailable (HTTP 503)"}
	passingBranch := runList(branchRun("push", releaseBranch, "completed", "success"))
	for _, test := range []struct {
		name, endpoint string
		failures       int
		overrides      map[string]any
		want, wantErr  string
	}{
		{"transient jobs read then success", queueJobs, 1, map[string]any{branchRuns: runList()}, "skip", ""},
		{"persistent VM read failure with branch Fast checks", queueJobs, readAttempts, nil, "run", ""},
		{"persistent Fast checks read failure", branchJobs, readAttempts, map[string]any{queueRuns: runList()}, "", unreadable},
		{"persistent run listing failure without other evidence", queueRuns, readAttempts, map[string]any{branchRuns: runList()}, "", unreadable},
		// The unread branch run could have passed Fast checks.
		{"read failure outranks a failed Fast checks", branchJobs, readAttempts, map[string]any{queueJobs: vmJobs(replaced("Fast checks", fastJob("failure")))}, "", unreadable},
	} {
		t.Run(test.name, func(t *testing.T) {
			responses := map[string]any{
				queueRuns: runList(ciRun(nil)), mainCompare: map[string]string{"status": "behind"},
				queueJobs: vmJobs(nil), branchRuns: passingBranch, branchJobs: jobList(fastJob("success")),
			}
			for endpoint, response := range test.overrides {
				responses[endpoint] = response
			}
			f := regressionGitHub(t, responses)
			f.failures[test.endpoint] = slices.Repeat([]error{unavailable}, test.failures)
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
	assertRegression(t, regressionGitHub(t, nil), "2026.11", "", "invalid release version")
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
