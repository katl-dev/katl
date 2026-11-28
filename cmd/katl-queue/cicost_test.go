package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeCICost struct {
	runs []ciRun
	jobs map[int64][]ciJob
}

func (f fakeCICost) completedRuns(context.Context, ciCostWindow) ([]ciRun, error) {
	return f.runs, nil
}

func (f fakeCICost) runJobs(_ context.Context, run int64) ([]ciJob, error) {
	return f.jobs[run], nil
}

func runCICost(t *testing.T, src ciCostSource, window ciCostWindow, limit int) ciCostReport {
	t.Helper()
	var out bytes.Buffer
	if err := ciCost(context.Background(), src, window, limit, true, &out); err != nil {
		t.Fatal(err)
	}
	var report ciCostReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestCICostRankings(t *testing.T) {
	start := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	window := ciCostWindow{Since: start, Until: start.AddDate(0, 0, 3)}
	job := func(id int64, name, conclusion string, minutes int) ciJob {
		return ciJob{ID: id, Name: name, Conclusion: conclusion, Runner: "runner",
			Started: start, Completed: start.Add(time.Duration(minutes) * time.Minute)}
	}
	ci := func(id int64) ciRun { return ciRun{ID: id, Path: ".github/workflows/ci.yml", Event: "pull_request"} }
	const standard = "VM (standard) / VM / lifecycle"
	const lts = "VM (lts) / VM / lifecycle"
	retry := job(12, standard, "success", 6)
	src := fakeCICost{
		runs: []ciRun{ci(1), ci(2), ci(1), {ID: 3, Path: ".github/workflows/release.yml", Event: "push"}},
		jobs: map[int64][]ciJob{
			1: {
				job(11, standard, "failure", 14), retry, retry,
				job(13, lts, "cancelled", 19),
				job(14, "VM (standard) / VM / boot", "success", 4),
				job(15, "VM (standard) / VM / skipped", "skipped", 100),
				{ID: 16, Name: lts, Conclusion: "cancelled", Completed: start},
				{ID: 17, Name: standard, Conclusion: "failure", Started: start},
			},
			2: {
				job(21, standard, "success", 10),
				job(22, lts, "success", 1),
				job(23, "Fast checks", "success", 31),
			},
			3: {job(31, standard, "failure", 200)},
		},
	}
	// The same job name in another workflow is a separate job.
	wantJobs := []ciCostRow{
		{Workflow: "release.yml", Name: standard, Runs: 1, Failures: 1, MedianMinutes: 200, TotalMinutes: 200},
		{Workflow: "ci.yml", Name: "Fast checks", Runs: 1, MedianMinutes: 31, TotalMinutes: 31},
		{Workflow: "ci.yml", Name: standard, Runs: 3, Failures: 1, MedianMinutes: 10, TotalMinutes: 30},
		{Workflow: "ci.yml", Name: lts, Runs: 2, Cancelled: 1, MedianMinutes: 10, TotalMinutes: 20},
		{Workflow: "ci.yml", Name: "VM (standard) / VM / boot", Runs: 1, MedianMinutes: 4, TotalMinutes: 4},
	}
	wantPrefixes := []ciCostRow{
		{Workflow: "release.yml", Name: "VM", Runs: 1, Failures: 1, MedianMinutes: 200, TotalMinutes: 200},
		{Workflow: "ci.yml", Name: "VM", Runs: 6, Failures: 1, Cancelled: 1, MedianMinutes: 8, TotalMinutes: 54},
		{Workflow: "ci.yml", Name: "Fast checks", Runs: 1, MedianMinutes: 31, TotalMinutes: 31},
	}
	for _, limit := range []int{0, 1} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			report := runCICost(t, src, window, limit)

			if report.ciCostWindow != window || report.RunsScanned != 3 {
				t.Errorf("window = %+v, scanned %d runs, want %+v and 3 runs", report.ciCostWindow, report.RunsScanned, window)
			}
			jobs, prefixes := wantJobs, wantPrefixes
			if limit != 0 {
				jobs, prefixes = jobs[:limit], prefixes[:limit]
			}
			if !slices.Equal(report.Jobs, jobs) {
				t.Errorf("jobs = %+v, want %+v", report.Jobs, jobs)
			}
			if !slices.Equal(report.Prefixes, prefixes) {
				t.Errorf("prefixes = %+v, want %+v", report.Prefixes, prefixes)
			}
		})
	}
}

// TestCICostFailingJob covers attribution that the recorded runs do not:
// reruns, ties, jobs that never ran, and a group with no unsuccessful job.
func TestCICostFailingJob(t *testing.T) {
	start := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return start.Add(time.Duration(minutes) * time.Minute) }
	job := func(id int64, name, conclusion string, attempt, completed int) ciJob {
		return ciJob{ID: id, Name: name, Conclusion: conclusion, Attempt: attempt, Runner: "runner", Started: start, Completed: at(completed)}
	}
	group := func(id int64, conclusion string, attempt int) ciRun {
		return ciRun{ID: id, Path: ".github/workflows/ci.yml", Event: "merge_group", Conclusion: conclusion, Attempt: attempt}
	}
	src := fakeCICost{
		runs: []ciRun{
			group(1, "failure", 2),
			group(2, "failure", 1),
			group(3, "cancelled", 1),
			{ID: 4, Path: ".github/workflows/ci.yml", Event: "pull_request", Conclusion: "failure", Attempt: 1},
		},
		jobs: map[int64][]ciJob{
			// The first attempt's earlier failure did not decide the run.
			1: {job(11, "Fast checks", "failure", 1, 2), job(12, "VM lane", "failure", 2, 9), job(13, "CI", "failure", 2, 10)},
			// Equal completion times fall back to the name.
			2: {job(21, "VM lane", "failure", 1, 5), job(22, "Drivers", "failure", 1, 5), job(23, "Fast checks", "success", 1, 1)},
			// A job cancelled before it reached a runner did not stop the group.
			3: {job(31, "Fast checks", "success", 1, 3), job(32, "Plan", "skipped", 1, 0),
				{ID: 33, Name: "VM / ${{ matrix.name }}", Conclusion: "cancelled", Attempt: 1, Started: start, Completed: start}},
			4: {job(41, "Fast checks", "failure", 1, 1)},
		},
	}

	report := runCICost(t, src, ciCostWindow{Since: start, Until: at(60)}, 0)

	want := []ciFailingJobRow{
		{Workflow: "ci.yml", Job: "Drivers", Failures: 1},
		{Workflow: "ci.yml", Job: "VM lane", Failures: 1},
		{Workflow: "ci.yml", Job: "", Cancelled: 1},
	}
	if !slices.Equal(report.MergeGroupFailures, want) {
		t.Errorf("merge-group failures = %+v, want %+v", report.MergeGroupFailures, want)
	}
}

// fakeGH puts a gh on PATH that answers each REST read with its route's file.
// Routes ending in "*" match endpoint prefixes. The fake logs every call and
// fails the first failTimes reads of failEndpoint with failStderr.
func fakeGH(t *testing.T, routes map[string]string, failEndpoint string, failTimes int, failStderr string) (log string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")

	var script strings.Builder
	fmt.Fprintf(&script, "#!%s\nprintf '%%s\\n' \"$*\" >> '%s'\n", sh, log)
	if failEndpoint != "" {
		fmt.Fprintf(&script, "if [ \"$4\" = '%s' ] && [ \"$(grep -cxF -- \"$*\" '%s')\" -le %d ]; then\n  printf '%%s\\n' '%s' >&2; exit 1\nfi\n",
			failEndpoint, log, failTimes, failStderr)
	}
	script.WriteString("case \"$4\" in\n")
	for route, file := range routes {
		pattern := "'" + route + "'"
		if prefix, ok := strings.CutSuffix(route, "*"); ok {
			pattern = "'" + prefix + "'*"
		}
		fmt.Fprintf(&script, "  %s) cat '%s' ;;\n", pattern, file)
	}
	script.WriteString("  *) printf 'unexpected gh invocation: %s\\n' \"$*\" >&2; exit 1 ;;\nesac\n")
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script.String()), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func countCalls(t *testing.T, log, endpoint string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for line := range strings.Lines(string(raw)) {
		if strings.TrimSuffix(line, "\n") == "api --paginate --slurp "+endpoint+" --hostname github.com" {
			n++
		}
	}
	return n
}

// Runs recorded from katl-dev/katl: three successful, one failed and one
// cancelled ci.yml merge group, failed ci.yml and system-extensions.yml pull
// request runs, and a prune-merge-runs.yml run. The expected figures were
// derived from the recordings with jq.
var recordedWindow = ciCostWindow{
	Since: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC),
	Until: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
}

const (
	recordedRuns      = "repos/katl-dev/katl/actions/runs?per_page=100&status=completed&created=2026-11-20T00%3A00%3A00Z..2026-11-28T00%3A00%3A00Z"
	recordedFailedRun = 45922785640
)

func recordedJobs(id int64) string {
	return fmt.Sprintf("repos/katl-dev/katl/actions/runs/%d/jobs?per_page=100&filter=all", id)
}

func recordedRoutes(t *testing.T) map[string]string {
	t.Helper()
	data, err := filepath.Abs(filepath.Join("testdata", "cicost"))
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]string{recordedRuns: filepath.Join(data, "runs.json")}
	for _, id := range []int64{45980906207, 45975254635, 45974191884, 45922785640, 45957450152, 45934693880, 45930757191, 45985168411} {
		routes[recordedJobs(id)] = filepath.Join(data, fmt.Sprintf("jobs-%d.json", id))
	}
	return routes
}

func minutes(seconds int) float64 { return (time.Duration(seconds) * time.Second).Minutes() }

func TestCICostRecorded(t *testing.T) {
	fakeGH(t, recordedRoutes(t), "", 0, "")

	report := runCICost(t, newGH(), recordedWindow, 0)

	if report.RunsScanned != 8 {
		t.Errorf("scanned %d runs, want 8", report.RunsScanned)
	}
	// Runner wait is a job's start less its creation. In these runs no job
	// waited more than seven seconds, while every job ran for longer.
	wantWorkflows := []ciWorkflowRow{
		{Workflow: "ci.yml", Runs: 6, Failures: 2, Cancelled: 1, Jobs: 172, JobMinutes: minutes(86115),
			WaitP50Minutes: minutes(2), WaitP90Minutes: minutes(2), WaitMaxMinutes: minutes(7)},
		{Workflow: "prune-merge-runs.yml", Runs: 1, Jobs: 1, JobMinutes: minutes(30),
			WaitP50Minutes: minutes(2), WaitP90Minutes: minutes(2), WaitMaxMinutes: minutes(2)},
		{Workflow: "system-extensions.yml", Runs: 1, Failures: 1, Jobs: 1, JobMinutes: minutes(18),
			WaitP50Minutes: minutes(1), WaitP90Minutes: minutes(1), WaitMaxMinutes: minutes(1)},
	}
	if !slices.Equal(report.Workflows, wantWorkflows) {
		t.Errorf("workflows = %+v, want %+v", report.Workflows, wantWorkflows)
	}
	// Successful groups took 1716, 1802 and 2054 seconds. Nearest-rank
	// percentiles are durations that occurred; interpolation would not be.
	wantGroups := []ciMergeGroupRow{{Workflow: "ci.yml", Runs: 5, Succeeded: 3, Failures: 1, Cancelled: 1,
		WallP50Minutes: minutes(1802), WallP90Minutes: minutes(2054)}}
	if !slices.Equal(report.MergeGroups, wantGroups) {
		t.Errorf("merge groups = %+v, want %+v", report.MergeGroups, wantGroups)
	}
	// The failed group's summary job CI also failed, but last. The cancelled
	// group's lane ran into its time limit.
	wantFailing := []ciFailingJobRow{
		{Workflow: "ci.yml", Job: "Production (standard) / Build Extension (nvidia)", Failures: 1},
		{Workflow: "ci.yml", Job: "VM (standard) / VM / cluster", Cancelled: 1},
	}
	if !slices.Equal(report.MergeGroupFailures, wantFailing) {
		t.Errorf("merge-group failures = %+v, want %+v", report.MergeGroupFailures, wantFailing)
	}
}

func TestCICostReadFailures(t *testing.T) {
	failed := recordedJobs(recordedFailedRun)
	for _, tc := range []struct {
		name      string
		stderr    string
		failTimes int
		wantErr   string
		wantCalls int
	}{
		{name: "transient recovers", stderr: "gh: Server Error (HTTP 502)", failTimes: 2, wantCalls: 3},
		{name: "transient persists", stderr: "gh: Server Error (HTTP 502)", failTimes: 100,
			wantErr: "HTTP 502", wantCalls: readAttempts},
		{name: "permanent", stderr: "gh: Not Found (HTTP 404)", failTimes: 100,
			wantErr: "HTTP 404", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := fakeGH(t, recordedRoutes(t), failed, tc.failTimes, tc.stderr)
			g := newGH()
			g.readDelay = time.Millisecond

			var out bytes.Buffer
			err := ciCost(context.Background(), g, recordedWindow, 0, true, &out)

			if calls := countCalls(t, log, failed); calls != tc.wantCalls {
				t.Errorf("read the failing endpoint %d times, want %d", calls, tc.wantCalls)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ci-cost: %v", err)
				}
				var report ciCostReport
				if err := json.Unmarshal(out.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				if report.RunsScanned != 8 {
					t.Errorf("scanned %d runs, want 8", report.RunsScanned)
				}
				return
			}
			want := fmt.Sprintf("list jobs for run %d: ", recordedFailedRun)
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one naming run %d and %s", err, recordedFailedRun, tc.wantErr)
			}
			if out.Len() != 0 {
				t.Errorf("printed a report despite the failure: %s", &out)
			}
		})
	}
}

func TestRunsCreatedSplitsAtSearchLimit(t *testing.T) {
	dir := t.TempDir()
	page := func(name string, first, last int64) string {
		var runs []ciRun
		for id := first; id <= last; id++ {
			runs = append(runs, ciRun{ID: id, Path: ".github/workflows/ci.yml", Conclusion: "success"})
		}
		raw, err := json.Marshal([]map[string][]ciRun{{"workflow_runs": runs}})
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, raw, 0644); err != nil {
			t.Fatal(err)
		}
		return file
	}
	const endpoint = "repos/katl-dev/katl/actions/runs?per_page=100&status=completed&created="
	// The whole range reaches the limit, so it is listed as two halves that
	// share no second. Its first half reaches the limit again.
	routes := map[string]string{
		endpoint + "2026-11-01T00%3A00%3A00Z..2026-11-01T00%3A00%3A11Z": page("all", 1, 1000),
		endpoint + "2026-11-01T00%3A00%3A00Z..2026-11-01T00%3A00%3A05Z": page("first", 1, 1000),
		endpoint + "2026-11-01T00%3A00%3A00Z..2026-11-01T00%3A00%3A02Z": page("first-first", 1, 700),
		endpoint + "2026-11-01T00%3A00%3A03Z..2026-11-01T00%3A00%3A05Z": page("first-second", 701, 1100),
		endpoint + "2026-11-01T00%3A00%3A06Z..2026-11-01T00%3A00%3A11Z": page("second", 1101, 1200),
	}
	fakeGH(t, routes, "", 0, "")
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	runs, err := newGH().completedRuns(context.Background(),
		ciCostWindow{Since: since, Until: since.Add(11 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}

	if len(runs) != 1200 || runs[0].ID != 1 || runs[len(runs)-1].ID != 1200 {
		t.Fatalf("listed %d runs, want runs 1 to 1200", len(runs))
	}
}

func TestCICostCommand(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return file
	}
	// Run 11 failed on its second attempt in Publish; the first attempt's
	// Build failure was retried. Run 12 was cancelled and run 13 succeeded.
	runs := write("runs.json", `[
  {"workflow_runs":[
    {"id":11,"path":".github/workflows/release.yml","event":"merge_group","status":"completed","conclusion":"failure","run_attempt":2,
     "created_at":"2026-11-22T00:00:00Z","updated_at":"2026-11-22T00:20:00Z"},
    {"id":12,"path":".github/workflows/release.yml","event":"merge_group","status":"completed","conclusion":"cancelled","run_attempt":1,
     "created_at":"2026-11-22T01:00:00Z","updated_at":"2026-11-22T01:30:00Z"}]},
  {"workflow_runs":[
    {"id":13,"path":".github/workflows/release.yml","event":"merge_group","status":"completed","conclusion":"success","run_attempt":1,
     "created_at":"2026-11-22T02:00:00Z","updated_at":"2026-11-22T02:40:00Z"}]}]`)
	routes := map[string]string{
		"repos/katl-dev/katl/actions/workflows?per_page=100": write("workflows.json",
			`[{"workflows":[{"id":2,"name":"CI"}]},{"workflows":[{"id":3,"name":"Release Build"}]}]`),
		"repos/katl-dev/katl/actions/workflows/3/runs?per_page=100&status=completed&created=2026-11-22T00%3A00%3A00Z..*": runs,
		"repos/katl-dev/katl/actions/runs?per_page=100&status=completed&created=2026-11-22T00%3A00%3A00Z..*":             runs,
		"repos/katl-dev/katl/actions/runs/11/jobs?per_page=100&filter=all": write("jobs-11.json", `[
  {"jobs":[{"id":21,"name":"Build (lts)","conclusion":"failure","run_attempt":1,"runner_name":"runner",
    "created_at":"2026-11-22T00:00:00Z","started_at":"2026-11-22T00:00:00Z","completed_at":"2026-11-22T00:12:00Z"}]},
  {"jobs":[{"id":22,"name":"Build (lts)","conclusion":"success","run_attempt":2,"runner_name":"runner",
    "created_at":"2026-11-22T00:10:00Z","started_at":"2026-11-22T00:13:00Z","completed_at":"2026-11-22T00:19:00Z"},
   {"id":24,"name":"Publish","conclusion":"failure","run_attempt":2,"runner_name":"runner",
    "created_at":"2026-11-22T00:19:00Z","started_at":"2026-11-22T00:19:30Z","completed_at":"2026-11-22T00:20:00Z"}]}]`),
		"repos/katl-dev/katl/actions/runs/12/jobs?per_page=100&filter=all": write("jobs-12.json", `[
  {"jobs":[{"id":23,"name":"Build (standard)","conclusion":"cancelled","run_attempt":1,"runner_name":"runner",
    "created_at":"2026-11-22T01:00:00Z","started_at":"2026-11-22T01:00:00Z","completed_at":"2026-11-22T01:30:00Z"}]}]`),
		"repos/katl-dev/katl/actions/runs/13/jobs?per_page=100&filter=all": write("jobs-13.json", `[{"jobs":[]}]`),
	}
	fakeGH(t, routes, "", 0, "")
	// Waits are 0, 3, 0.5 and 0 minutes; job minutes are 12, 6, 0.5 and 30.
	tables := []string{
		"Workflow Runs Failures Cancelled Job minutes Runner wait p50 p90 max (min)",
		"release.yml 3 1 1 48.50 0.0 3.0 3.0",
		"Merge groups Runs Succeeded Failures Cancelled Wall p50 p90 (min)",
		"release.yml 3 1 1 1 40.0 40.0",
		"First failing job in merge groups Workflow Failures Cancelled",
		"Publish release.yml 1 0",
		"Job Workflow Runs Failures Cancelled Median (min) Total (min)",
		"Build (standard) release.yml 1 0 1 30.00 30.00",
		"Job prefix Workflow Runs Failures Cancelled Median (min) Total (min)",
		"Build release.yml 3 1 1 12.00 48.00",
	}

	for _, tc := range []struct {
		name  string
		args  []string
		scope string
	}{
		{name: "every workflow", scope: "all workflows"},
		{name: "one workflow", args: []string{"--workflow", "Release Build"}, scope: "Release Build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := append([]string{"ci-cost", "--since", "2026-11-22", "--limit", "1"}, tc.args...)
			if err := run(context.Background(), args, &out, &stderr); err != nil {
				t.Fatalf("ci-cost: %v (%s)", err, &stderr)
			}

			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if !strings.HasPrefix(lines[0], tc.scope+" | 2026-11-22T00:00:00Z to ") ||
				!strings.HasSuffix(lines[0], " | 3 completed workflow runs scanned") {
				t.Errorf("unexpected report header %q", lines[0])
			}
			var rows []string
			for _, line := range lines[1:] {
				if line != "" {
					rows = append(rows, strings.Join(strings.Fields(line), " "))
				}
			}
			if !slices.Equal(rows, tables) {
				t.Errorf("report rows = %q, want %q", rows, tables)
			}
		})
	}
}
