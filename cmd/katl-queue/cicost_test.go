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
	runs map[string][]int64
	jobs map[int64][]ciJob
}

func (f fakeCICost) completedRuns(_ context.Context, window ciCostWindow) ([]int64, error) {
	return f.runs[window.Workflow], nil
}

func (f fakeCICost) runJobs(_ context.Context, run int64) ([]ciJob, error) {
	return f.jobs[run], nil
}

func TestCICostRankings(t *testing.T) {
	start := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	window := ciCostWindow{Workflow: "ci.yml", Since: start, Until: start.AddDate(0, 0, 3)}
	job := func(id int64, name, conclusion string, minutes int) ciJob {
		return ciJob{ID: id, Name: name, Conclusion: conclusion,
			Started: start, Completed: start.Add(time.Duration(minutes) * time.Minute)}
	}
	const standard = "VM (standard) / VM / lifecycle"
	const lts = "VM (lts) / VM / lifecycle"
	retry := job(12, standard, "success", 6)
	src := fakeCICost{
		runs: map[string][]int64{"ci.yml": {1, 2, 1}, "release.yml": {3}},
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
	wantJobs := []ciCostRow{
		{Name: "Fast checks", Runs: 1, MedianMinutes: 31, TotalMinutes: 31},
		{Name: standard, Runs: 3, Failures: 1, MedianMinutes: 10, TotalMinutes: 30},
		{Name: lts, Runs: 2, Cancelled: 1, MedianMinutes: 10, TotalMinutes: 20},
		{Name: "VM (standard) / VM / boot", Runs: 1, MedianMinutes: 4, TotalMinutes: 4},
	}
	wantPrefixes := []ciCostRow{
		{Name: "VM", Runs: 6, Failures: 1, Cancelled: 1, MedianMinutes: 8, TotalMinutes: 54},
		{Name: "Fast checks", Runs: 1, MedianMinutes: 31, TotalMinutes: 31},
	}
	for _, limit := range []int{0, 1} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			var out bytes.Buffer
			if err := ciCost(context.Background(), src, window, limit, true, &out); err != nil {
				t.Fatal(err)
			}
			var report ciCostReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}

			if report.ciCostWindow != window || report.RunsScanned != 2 {
				t.Errorf("window = %+v, scanned %d runs, want %+v and 2 runs", report.ciCostWindow, report.RunsScanned, window)
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

func TestCICostCommand(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := `case "$*" in
  "repo view --json owner,name --jq "*) printf '%s\n' example/katl ;;
  "api --paginate --slurp repos/example/katl/actions/workflows?per_page=100")
    printf '%s\n' '[{"workflows":[{"id":2,"name":"CI"}]},{"workflows":[{"id":3,"name":"Release Build"}]}]' ;;
  "api --paginate --slurp repos/example/katl/actions/workflows/3/runs?per_page=100&status=completed&created=2026-11-22T00%3A00%3A00Z.."*)
    printf '%s\n' '[
      {"workflow_runs":[{"id":11,"status":"completed","created_at":"2026-11-22T00:00:00Z"}]},
      {"workflow_runs":[{"id":12,"status":"completed","created_at":"2026-11-22T01:00:00Z"}]}]' ;;
  "api --paginate --slurp repos/example/katl/actions/runs/11/jobs?per_page=100&filter=all")
    printf '%s\n' '[
      {"jobs":[{"id":21,"name":"Build (lts)","conclusion":"failure","started_at":"2026-11-22T00:00:00Z","completed_at":"2026-11-22T00:12:00Z"}]},
      {"jobs":[{"id":22,"name":"Build (lts)","conclusion":"success","started_at":"2026-11-22T00:13:00Z","completed_at":"2026-11-22T00:19:00Z"}]}]' ;;
  "api --paginate --slurp repos/example/katl/actions/runs/12/jobs?per_page=100&filter=all")
    printf '%s\n' '[{"jobs":[{"id":23,"name":"Build (standard)","conclusion":"cancelled","started_at":"2026-11-22T01:00:00Z","completed_at":"2026-11-22T01:30:00Z"}]}]' ;;
  *) printf 'unexpected gh invocation: %s\n' "$*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!"+sh+"\n"+script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	var out, stderr bytes.Buffer
	args := []string{"ci-cost", "--since", "2026-11-22", "--workflow", "Release Build", "--limit", "1"}
	if err := run(context.Background(), args, &out, &stderr); err != nil {
		t.Fatalf("ci-cost: %v (%s)", err, &stderr)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.HasPrefix(lines[0], "Release Build | 2026-11-22T00:00:00Z to ") ||
		!strings.HasSuffix(lines[0], " | 2 completed workflow runs scanned") {
		t.Errorf("unexpected report header %q", lines[0])
	}
	var rows []string
	for _, line := range lines[1:] {
		if line != "" {
			rows = append(rows, strings.Join(strings.Fields(line), " "))
		}
	}
	want := []string{
		"Job Runs Failures Cancelled Median (min) Total (min)",
		"Build (standard) 1 0 1 30.00 30.00",
		"Job prefix Runs Failures Cancelled Median (min) Total (min)",
		"Build 3 1 1 12.00 48.00",
	}
	if !slices.Equal(rows, want) {
		t.Errorf("report rows = %q, want %q", rows, want)
	}
}
