package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"path"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/sync/errgroup"
)

type ciCostWindow struct {
	// Workflow names the one workflow to report; empty reports every workflow.
	Workflow string    `json:"workflow,omitempty"`
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
}

type ciRun struct {
	ID         int64     `json:"id"`
	Path       string    `json:"path"`
	Event      string    `json:"event"`
	Conclusion string    `json:"conclusion"`
	Attempt    int       `json:"run_attempt"`
	Created    time.Time `json:"created_at"`
	// Updated is when GitHub last changed the run, which for a completed run
	// is when it completed.
	Updated time.Time `json:"updated_at"`
}

// workflow is the workflow's file name, which is unique in the repository.
func (r ciRun) workflow() string { return path.Base(r.Path) }

type ciJob struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Attempt    int    `json:"run_attempt"`
	// Runner is empty for a job that never ran, such as one cancelled before
	// its matrix was expanded.
	Runner string `json:"runner_name"`
	// Created is when the job was queued for a runner, after the jobs it
	// needs had finished.
	Created   time.Time `json:"created_at"`
	Started   time.Time `json:"started_at"`
	Completed time.Time `json:"completed_at"`
}

// timed reports whether the job ran on a runner and has a usable duration.
func (j ciJob) timed() bool {
	return j.Runner != "" && j.Conclusion != "skipped" && !j.Started.IsZero() && !j.Completed.IsZero() && !j.Completed.Before(j.Started)
}

type ciCostSource interface {
	completedRuns(context.Context, ciCostWindow) ([]ciRun, error)
	runJobs(context.Context, int64) ([]ciJob, error)
}

type ciCostRow struct {
	Workflow      string  `json:"workflow"`
	Name          string  `json:"name"`
	Runs          int     `json:"runs"`
	Failures      int     `json:"failures"`
	Cancelled     int     `json:"cancelled"`
	MedianMinutes float64 `json:"median_minutes"`
	TotalMinutes  float64 `json:"total_minutes"`
}

// ciWorkflowRow summarizes one workflow's runs and the time its jobs waited
// for a runner.
type ciWorkflowRow struct {
	Workflow       string  `json:"workflow"`
	Runs           int     `json:"runs"`
	Failures       int     `json:"failures"`
	Cancelled      int     `json:"cancelled"`
	Jobs           int     `json:"jobs"`
	JobMinutes     float64 `json:"job_minutes"`
	WaitP50Minutes float64 `json:"runner_wait_p50_minutes"`
	WaitP90Minutes float64 `json:"runner_wait_p90_minutes"`
	WaitMaxMinutes float64 `json:"runner_wait_max_minutes"`
}

// ciMergeGroupRow summarizes one workflow's merge-group runs. Wall time runs
// from creation to completion and covers successful runs only.
type ciMergeGroupRow struct {
	Workflow       string  `json:"workflow"`
	Runs           int     `json:"runs"`
	Succeeded      int     `json:"succeeded"`
	Failures       int     `json:"failures"`
	Cancelled      int     `json:"cancelled"`
	WallP50Minutes float64 `json:"wall_p50_minutes"`
	WallP90Minutes float64 `json:"wall_p90_minutes"`
}

// ciFailingJobRow counts failed and cancelled merge-group runs by the first
// job of their last attempt to finish unsuccessfully. Job is empty when no
// job did.
type ciFailingJobRow struct {
	Workflow  string `json:"workflow"`
	Job       string `json:"job"`
	Failures  int    `json:"failures"`
	Cancelled int    `json:"cancelled"`
}

type ciCostReport struct {
	ciCostWindow
	RunsScanned        int               `json:"runs_scanned"`
	Workflows          []ciWorkflowRow   `json:"workflows"`
	MergeGroups        []ciMergeGroupRow `json:"merge_groups"`
	MergeGroupFailures []ciFailingJobRow `json:"merge_group_failures"`
	Jobs               []ciCostRow       `json:"jobs"`
	Prefixes           []ciCostRow       `json:"prefixes"`
}

func ciCostCommand(ctx context.Context, flags *flag.FlagSet, args []string, out io.Writer) error {
	since := flags.String("since", "", "start date inclusive (YYYY-MM-DD in UTC, default seven days ago)")
	workflow := flags.String("workflow", "", "report only this workflow filename, display name, or ID (default every workflow)")
	limit := flags.Int("limit", 0, "maximum rows per ranking (0 shows all)")
	asJSON := flags.Bool("json", false, "print the report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("ci-cost takes no positional arguments")
	}
	if flagWasSet(flags, "workflow") && strings.TrimSpace(*workflow) == "" {
		return fmt.Errorf("--workflow must name a workflow")
	}
	if *limit < 0 {
		return fmt.Errorf("--limit cannot be negative")
	}

	now := time.Now().UTC().Truncate(time.Second)
	window := ciCostWindow{Workflow: *workflow, Since: now.AddDate(0, 0, -7), Until: now}
	if flagWasSet(flags, "since") {
		date, err := time.Parse(time.DateOnly, *since)
		if err != nil {
			return fmt.Errorf("--since must be a date in YYYY-MM-DD format: %w", err)
		}
		if date.After(now) {
			return fmt.Errorf("--since cannot be after today in UTC")
		}
		window.Since = date
	}

	return ciCost(ctx, newGH(), window, *limit, *asJSON, out)
}

// ciJobReaders bounds concurrent job listings, one gh process each, well below
// GitHub's secondary rate limit on concurrent requests.
const ciJobReaders = 8

type ciJobKey struct{ workflow, name string }

type ciWorkflowSamples struct {
	runs, failures, cancelled int
	jobMinutes                time.Duration
	jobs                      int
	waits                     []time.Duration
}

type ciMergeGroupSamples struct {
	runs, succeeded, failures, cancelled int
	walls                                []time.Duration
}

func ciCost(ctx context.Context, src ciCostSource, window ciCostWindow, limit int, asJSON bool, out io.Writer) error {
	runs, err := src.completedRuns(ctx, window)
	if err != nil {
		return err
	}
	var unique []ciRun
	seenRuns := map[int64]bool{}
	for _, run := range runs {
		if !seenRuns[run.ID] {
			seenRuns[run.ID] = true
			unique = append(unique, run)
		}
	}
	runJobs := make([][]ciJob, len(unique))
	fetch, fetchCtx := errgroup.WithContext(ctx)
	fetch.SetLimit(ciJobReaders)
	for i, run := range unique {
		fetch.Go(func() error {
			found, err := src.runJobs(fetchCtx, run.ID)
			if err != nil {
				return fmt.Errorf("list jobs for run %d: %w", run.ID, err)
			}
			runJobs[i] = found
			return nil
		})
	}
	if err := fetch.Wait(); err != nil {
		return err
	}

	report := ciCostReport{ciCostWindow: window, RunsScanned: len(unique)}
	workflows := map[string]*ciWorkflowSamples{}
	groups := map[string]*ciMergeGroupSamples{}
	failing := map[ciJobKey]*ciFailingJobRow{}
	jobs := map[ciJobKey]*ciCostSamples{}
	prefixes := map[ciJobKey]*ciCostSamples{}
	seenJobs := map[int64]bool{}
	for i, run := range unique {
		found := runJobs[i]
		name := run.workflow()
		summary := workflows[name]
		if summary == nil {
			summary = &ciWorkflowSamples{}
			workflows[name] = summary
		}
		summary.runs++
		switch run.Conclusion {
		case "failure":
			summary.failures++
		case "cancelled":
			summary.cancelled++
		}
		for _, job := range found {
			if seenJobs[job.ID] || !job.timed() {
				continue
			}
			seenJobs[job.ID] = true
			summary.jobs++
			summary.jobMinutes += job.Completed.Sub(job.Started)
			if !job.Created.IsZero() && !job.Started.Before(job.Created) {
				summary.waits = append(summary.waits, job.Started.Sub(job.Created))
			}
			addCISample(jobs, ciJobKey{name, job.Name}, job)
			addCISample(prefixes, ciJobKey{name, ciJobPrefix(job.Name)}, job)
		}

		if run.Event == "merge_group" {
			addMergeGroup(groups, failing, name, run, found)
		}
	}

	report.Workflows = summarizeWorkflows(workflows)
	report.MergeGroups = summarizeMergeGroups(groups)
	report.MergeGroupFailures = rankFailingJobs(failing, limit)
	report.Jobs = rankCISamples(jobs, limit)
	report.Prefixes = rankCISamples(prefixes, limit)
	if asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	return report.writeText(out)
}

func addMergeGroup(groups map[string]*ciMergeGroupSamples, failing map[ciJobKey]*ciFailingJobRow, workflow string, run ciRun, jobs []ciJob) {
	group := groups[workflow]
	if group == nil {
		group = &ciMergeGroupSamples{}
		groups[workflow] = group
	}
	group.runs++
	switch run.Conclusion {
	case "success":
		group.succeeded++
		if !run.Updated.Before(run.Created) {
			group.walls = append(group.walls, run.Updated.Sub(run.Created))
		}
		return
	case "failure":
		group.failures++
	case "cancelled":
		group.cancelled++
	default:
		return
	}

	key := ciJobKey{workflow, firstFailingJob(run, jobs)}
	row := failing[key]
	if row == nil {
		row = &ciFailingJobRow{Workflow: key.workflow, Job: key.name}
		failing[key] = row
	}
	if run.Conclusion == "failure" {
		row.Failures++
	} else {
		row.Cancelled++
	}
}

// firstFailingJob names the job of the run's last attempt that ran and
// finished unsuccessfully first. A job that summarizes others, such as a
// required-check gate, finishes after the jobs it depends on, so it is
// named only when nothing else failed.
func firstFailingJob(run ciRun, jobs []ciJob) string {
	var first *ciJob
	for i, job := range jobs {
		if run.Attempt != 0 && job.Attempt != run.Attempt {
			continue
		}
		switch job.Conclusion {
		case "success", "skipped", "neutral", "":
			continue
		}
		if job.Runner == "" || job.Completed.IsZero() {
			continue
		}
		if first == nil || job.Completed.Before(first.Completed) ||
			job.Completed.Equal(first.Completed) && job.Name < first.Name {
			first = &jobs[i]
		}
	}
	if first == nil {
		return ""
	}
	return first.Name
}

// percentile returns the nearest-rank percentile, a duration that occurred.
func percentile(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Ceil(p * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}

func summarizeWorkflows(groups map[string]*ciWorkflowSamples) []ciWorkflowRow {
	rows := make([]ciWorkflowRow, 0, len(groups))
	for name, samples := range groups {
		row := ciWorkflowRow{
			Workflow:   name,
			Runs:       samples.runs,
			Failures:   samples.failures,
			Cancelled:  samples.cancelled,
			Jobs:       samples.jobs,
			JobMinutes: samples.jobMinutes.Minutes(),
		}
		if waits := samples.waits; len(waits) > 0 {
			slices.Sort(waits)
			row.WaitP50Minutes = percentile(waits, 0.5).Minutes()
			row.WaitP90Minutes = percentile(waits, 0.9).Minutes()
			row.WaitMaxMinutes = waits[len(waits)-1].Minutes()
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b ciWorkflowRow) int {
		return cmp.Or(cmp.Compare(b.JobMinutes, a.JobMinutes), cmp.Compare(a.Workflow, b.Workflow))
	})
	return rows
}

func summarizeMergeGroups(groups map[string]*ciMergeGroupSamples) []ciMergeGroupRow {
	rows := make([]ciMergeGroupRow, 0, len(groups))
	for name, samples := range groups {
		row := ciMergeGroupRow{
			Workflow:  name,
			Runs:      samples.runs,
			Succeeded: samples.succeeded,
			Failures:  samples.failures,
			Cancelled: samples.cancelled,
		}
		if walls := samples.walls; len(walls) > 0 {
			slices.Sort(walls)
			row.WallP50Minutes = percentile(walls, 0.5).Minutes()
			row.WallP90Minutes = percentile(walls, 0.9).Minutes()
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b ciMergeGroupRow) int {
		return cmp.Or(cmp.Compare(b.Runs, a.Runs), cmp.Compare(a.Workflow, b.Workflow))
	})
	return rows
}

func rankFailingJobs(groups map[ciJobKey]*ciFailingJobRow, limit int) []ciFailingJobRow {
	rows := make([]ciFailingJobRow, 0, len(groups))
	for _, row := range groups {
		rows = append(rows, *row)
	}
	slices.SortFunc(rows, func(a, b ciFailingJobRow) int {
		return cmp.Or(
			cmp.Compare(b.Failures+b.Cancelled, a.Failures+a.Cancelled),
			cmp.Compare(b.Failures, a.Failures),
			cmp.Compare(a.Workflow, b.Workflow),
			cmp.Compare(a.Job, b.Job))
	})
	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows
}

func ciJobPrefix(name string) string {
	caller, _, _ := strings.Cut(name, " / ")
	// Reusable workflow jobs include the calling job before the slash.
	// A matrix suffix belongs to that caller, even when the name continues.
	if prefix, _, ok := strings.Cut(caller, " ("); ok && strings.HasSuffix(caller, ")") {
		return prefix
	}
	return caller
}

type ciCostSamples struct {
	durations []time.Duration
	total     time.Duration
	failures  int
	cancelled int
}

func addCISample(groups map[ciJobKey]*ciCostSamples, key ciJobKey, job ciJob) {
	samples := groups[key]
	if samples == nil {
		samples = &ciCostSamples{}
		groups[key] = samples
	}
	duration := job.Completed.Sub(job.Started)
	samples.durations = append(samples.durations, duration)
	samples.total += duration
	switch job.Conclusion {
	case "failure":
		samples.failures++
	case "cancelled":
		samples.cancelled++
	}
}

func rankCISamples(groups map[ciJobKey]*ciCostSamples, limit int) []ciCostRow {
	rows := make([]ciCostRow, 0, len(groups))
	for key, samples := range groups {
		durations := samples.durations
		slices.Sort(durations)
		middle := len(durations) / 2
		median := durations[middle].Minutes()
		if len(durations)%2 == 0 {
			median = (durations[middle-1].Minutes() + median) / 2
		}
		rows = append(rows, ciCostRow{
			Workflow:      key.workflow,
			Name:          key.name,
			Runs:          len(durations),
			Failures:      samples.failures,
			Cancelled:     samples.cancelled,
			MedianMinutes: median,
			TotalMinutes:  samples.total.Minutes(),
		})
	}
	slices.SortFunc(rows, func(a, b ciCostRow) int {
		return cmp.Or(cmp.Compare(b.TotalMinutes, a.TotalMinutes),
			cmp.Compare(a.Workflow, b.Workflow), cmp.Compare(a.Name, b.Name))
	})
	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows
}

func (r ciCostReport) writeText(out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	scope := r.Workflow
	if scope == "" {
		scope = "all workflows"
	}
	fmt.Fprintf(w, "%s | %s to %s | %d completed workflow runs scanned\n",
		scope, r.Since.Format(time.RFC3339), r.Until.Format(time.RFC3339), r.RunsScanned)

	fmt.Fprintf(w, "\nWorkflow\tRuns\tFailures\tCancelled\tJob minutes\tRunner wait p50\tp90\tmax (min)\n")
	for _, row := range r.Workflows {
		wait := "-\t-\t-"
		if row.Jobs > 0 {
			wait = fmt.Sprintf("%.1f\t%.1f\t%.1f", row.WaitP50Minutes, row.WaitP90Minutes, row.WaitMaxMinutes)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%.2f\t%s\n",
			row.Workflow, row.Runs, row.Failures, row.Cancelled, row.JobMinutes, wait)
	}

	fmt.Fprintf(w, "\nMerge groups\tRuns\tSucceeded\tFailures\tCancelled\tWall p50\tp90 (min)\n")
	for _, row := range r.MergeGroups {
		wall := "-\t-"
		if row.Succeeded > 0 {
			wall = fmt.Sprintf("%.1f\t%.1f", row.WallP50Minutes, row.WallP90Minutes)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\n",
			row.Workflow, row.Runs, row.Succeeded, row.Failures, row.Cancelled, wall)
	}

	fmt.Fprintf(w, "\nFirst failing job in merge groups\tWorkflow\tFailures\tCancelled\n")
	for _, row := range r.MergeGroupFailures {
		job := row.Job
		if job == "" {
			job = "(no unsuccessful job)"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\n", job, row.Workflow, row.Failures, row.Cancelled)
	}

	for _, table := range []struct {
		label string
		rows  []ciCostRow
	}{{"Job", r.Jobs}, {"Job prefix", r.Prefixes}} {
		fmt.Fprintf(w, "\n%s\tWorkflow\tRuns\tFailures\tCancelled\tMedian (min)\tTotal (min)\n", table.label)
		for _, row := range table.rows {
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%.2f\t%.2f\n",
				row.Name, row.Workflow, row.Runs, row.Failures, row.Cancelled, row.MedianMinutes, row.TotalMinutes)
		}
	}
	return w.Flush()
}
