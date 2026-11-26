package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

type ciCostWindow struct {
	Workflow string    `json:"workflow"`
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
}

type ciJob struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Conclusion string    `json:"conclusion"`
	Started    time.Time `json:"started_at"`
	Completed  time.Time `json:"completed_at"`
}

type ciCostSource interface {
	completedRuns(context.Context, ciCostWindow) ([]int64, error)
	runJobs(context.Context, int64) ([]ciJob, error)
}

type ciCostRow struct {
	Name          string  `json:"name"`
	Runs          int     `json:"runs"`
	Failures      int     `json:"failures"`
	Cancelled     int     `json:"cancelled"`
	MedianMinutes float64 `json:"median_minutes"`
	TotalMinutes  float64 `json:"total_minutes"`
}

type ciCostReport struct {
	ciCostWindow
	RunsScanned int         `json:"runs_scanned"`
	Jobs        []ciCostRow `json:"jobs"`
	Prefixes    []ciCostRow `json:"prefixes"`
}

func ciCostCommand(ctx context.Context, flags *flag.FlagSet, args []string, out io.Writer) error {
	since := flags.String("since", "", "start date inclusive (YYYY-MM-DD in UTC, default seven days ago)")
	workflow := flags.String("workflow", "ci.yml", "workflow filename, display name, or ID")
	limit := flags.Int("limit", 0, "maximum rows per ranking (0 shows all)")
	asJSON := flags.Bool("json", false, "print the report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("ci-cost takes no positional arguments")
	}
	if strings.TrimSpace(*workflow) == "" {
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

	g, err := newGH(ctx)
	if err != nil {
		return err
	}
	return ciCost(ctx, g, window, *limit, *asJSON, out)
}

func ciCost(ctx context.Context, src ciCostSource, window ciCostWindow, limit int, asJSON bool, out io.Writer) error {
	runs, err := src.completedRuns(ctx, window)
	if err != nil {
		return err
	}
	report := ciCostReport{ciCostWindow: window}
	jobs := map[string]*ciCostSamples{}
	prefixes := map[string]*ciCostSamples{}
	seenRuns := map[int64]bool{}
	seenJobs := map[int64]bool{}
	for _, run := range runs {
		if seenRuns[run] {
			continue
		}
		seenRuns[run] = true
		found, err := src.runJobs(ctx, run)
		if err != nil {
			return fmt.Errorf("list jobs for run %d: %w", run, err)
		}
		report.RunsScanned++
		for _, job := range found {
			if seenJobs[job.ID] || job.Conclusion == "skipped" || job.Started.IsZero() ||
				job.Completed.IsZero() || job.Completed.Before(job.Started) {
				continue
			}
			seenJobs[job.ID] = true
			addCISample(jobs, job.Name, job)
			addCISample(prefixes, ciJobPrefix(job.Name), job)
		}
	}

	report.Jobs = rankCISamples(jobs, limit)
	report.Prefixes = rankCISamples(prefixes, limit)
	if asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	return report.writeText(out)
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

func addCISample(groups map[string]*ciCostSamples, name string, job ciJob) {
	samples := groups[name]
	if samples == nil {
		samples = &ciCostSamples{}
		groups[name] = samples
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

func rankCISamples(groups map[string]*ciCostSamples, limit int) []ciCostRow {
	rows := make([]ciCostRow, 0, len(groups))
	for name, samples := range groups {
		durations := samples.durations
		slices.Sort(durations)
		middle := len(durations) / 2
		median := durations[middle].Minutes()
		if len(durations)%2 == 0 {
			median = (durations[middle-1].Minutes() + median) / 2
		}
		rows = append(rows, ciCostRow{
			Name:          name,
			Runs:          len(durations),
			Failures:      samples.failures,
			Cancelled:     samples.cancelled,
			MedianMinutes: median,
			TotalMinutes:  samples.total.Minutes(),
		})
	}
	slices.SortFunc(rows, func(a, b ciCostRow) int {
		return cmp.Or(cmp.Compare(b.TotalMinutes, a.TotalMinutes), cmp.Compare(a.Name, b.Name))
	})
	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows
}

func (r ciCostReport) writeText(out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "%s | %s to %s | %d completed workflow runs scanned\n",
		r.Workflow, r.Since.Format(time.RFC3339), r.Until.Format(time.RFC3339), r.RunsScanned)
	for _, table := range []struct {
		label string
		rows  []ciCostRow
	}{{"Job", r.Jobs}, {"Job prefix", r.Prefixes}} {
		fmt.Fprintf(w, "\n%s\tRuns\tFailures\tCancelled\tMedian (min)\tTotal (min)\n", table.label)
		for _, row := range table.rows {
			fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%.2f\t%.2f\n",
				row.Name, row.Runs, row.Failures, row.Cancelled, row.MedianMinutes, row.TotalMinutes)
		}
	}
	return w.Flush()
}
