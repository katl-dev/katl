package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const parentStallAge = 7 * 24 * time.Hour

func parentsCommand(ctx context.Context, flags *flag.FlagSet, args []string, out io.Writer) error {
	days := flags.Int("days", 14, "number of UTC dates to include, counting today")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("parents takes no positional arguments")
	}
	if *days < 1 {
		return fmt.Errorf("--days must be positive")
	}
	now := time.Now().UTC()
	start := now.Truncate(24*time.Hour).AddDate(0, 0, 1-*days)
	if start.Year() < 1 || start.After(now) {
		return fmt.Errorf("--days extends beyond the supported date range")
	}

	facts, err := newGH().parentFacts(ctx, start)
	if err != nil {
		return err
	}
	return evaluateParents(facts, start, now).writeText(out)
}

type issueLink struct {
	repo   string // empty for a reference in the current repository
	number int
}

var closesWhenLine = regexp.MustCompile(`(?i)^\s*Closes when:\s*(.*)$`)
var designLine = regexp.MustCompile(`(?i)^\s*(?:Design(?: review)?:|#{1,6}\s+(?:Design|Proposal)\s*$)`)
var trackerLink = regexp.MustCompile(`(?i)https?://` + regexp.QuoteMeta(katlHost) +
	`/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)\b|(?:\b([\w.-]+/[\w.-]+))?#(\d+)\b`)

type parentCandidate struct {
	inScope bool
	hasLine bool
	links   []issueLink
}

func candidateParent(issue trackerIssue) parentCandidate {
	p := parentCandidate{inScope: issue.SubIssuesSummary.Total > 0}
	for line := range unfencedLines(issue.Body) {
		p.inScope = p.inScope || designLine.MatchString(line)
		if condition := closesWhenLine.FindStringSubmatch(line); condition != nil {
			p.hasLine = true
			for _, match := range trackerLink.FindAllStringSubmatch(condition[1], -1) {
				number, err := strconv.Atoi(cmp.Or(match[2], match[4]))
				if err == nil && number > 0 {
					p.links = append(p.links, issueLink{repo: strings.ToLower(cmp.Or(match[1], match[3])), number: number})
				}
			}
		}
	}
	return p
}

type trackerFacts struct {
	issues     []trackerIssue
	children   map[int][]subIssue
	references map[issueLink]externalRef
}

type parentFinding struct {
	number int
	title  string
	url    string
}

type issueCounts struct {
	opened, closed, open int
}

type parentReport struct {
	finished, conditionsMet, stalled, missingCondition []parentFinding
	start, now                                         time.Time
	days                                               map[time.Time]issueCounts
	milestones                                         map[string]issueCounts
}

func evaluateParents(facts trackerFacts, start, now time.Time) parentReport {
	r := parentReport{
		start: start, now: now,
		days: map[time.Time]issueCounts{}, milestones: map[string]issueCounts{},
	}
	var openBefore int
	for _, issue := range facts.issues {
		before, changes := issueWindow(issue, start, now, r.days)
		openBefore += before
		openNow := before + changes.open
		if openNow != 0 || changes.opened != 0 || changes.closed != 0 {
			milestone := ""
			if issue.Milestone != nil {
				milestone = issue.Milestone.Title
			}
			row := r.milestones[milestone]
			row.opened += changes.opened
			row.closed += changes.closed
			row.open += openNow
			r.milestones[milestone] = row
		}

		if issue.State != "OPEN" {
			continue
		}
		p := candidateParent(issue)
		f := parentFinding{number: issue.Number, title: issue.Title, url: issue.URL}
		summary := issue.SubIssuesSummary
		if summary.Total > 0 && summary.Completed == summary.Total {
			r.finished = append(r.finished, f)
		}
		if p.inScope && len(p.links) > 0 && !slices.ContainsFunc(p.links, func(link issueLink) bool {
			return !facts.references[link].resolved()
		}) {
			r.conditionsMet = append(r.conditionsMet, f)
		}
		children := facts.children[issue.Number]
		if summary.Total > 0 && len(children) == summary.Total && !slices.ContainsFunc(children, func(child subIssue) bool {
			lastChange := child.CreatedAt
			if events := child.Timeline.Nodes; len(events) > 0 {
				lastChange = events[0].CreatedAt
			}
			return lastChange.After(now.Add(-parentStallAge))
		}) {
			r.stalled = append(r.stalled, f)
		}
		if p.inScope && !p.hasLine {
			r.missingCondition = append(r.missingCondition, f)
		}
	}

	open := openBefore
	for day := start; !day.After(now); day = day.AddDate(0, 0, 1) {
		row := r.days[day]
		open += row.open
		row.open = open
		r.days[day] = row
	}
	for _, findings := range [][]parentFinding{r.finished, r.conditionsMet, r.stalled, r.missingCondition} {
		slices.SortFunc(findings, func(a, b parentFinding) int { return cmp.Compare(a.number, b.number) })
	}
	return r
}

// issueWindow rewinds today's state to just before the first UTC date, then
// records the transitions in the window in both the daily and issue totals.
func issueWindow(issue trackerIssue, start, now time.Time, byDay map[time.Time]issueCounts) (int, issueCounts) {
	var before int
	var total issueCounts
	if issue.State == "OPEN" {
		before = 1
	}
	record := func(at time.Time, opened, closed, delta int) {
		if at.Before(start) {
			return
		}
		before -= delta
		if at.After(now) {
			return
		}
		day := at.UTC().Truncate(24 * time.Hour)
		row := byDay[day]
		row.opened += opened
		row.closed += closed
		row.open += delta
		byDay[day] = row
		total.opened += opened
		total.closed += closed
		total.open += delta
	}
	record(issue.CreatedAt, 1, 0, 1)
	for _, event := range issue.Timeline.Nodes {
		switch event.Type {
		case "ClosedEvent":
			record(event.CreatedAt, 0, 1, -1)
		case "ReopenedEvent":
			record(event.CreatedAt, 0, 0, 1)
		}
	}
	return before, total
}

func (r parentReport) writeText(out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 1, ' ', 0)
	linkTitle := strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "\t", " ")
	fmt.Fprintln(w, "\n## Parent and design issue reconciliation")
	for _, section := range []struct {
		title string
		items []parentFinding
	}{
		{"All sub-issues closed", r.finished},
		{"Closes when references resolved", r.conditionsMet},
		{"No sub-issue state change for 7 days", r.stalled},
		{"Missing a Closes when line", r.missingCondition},
	} {
		fmt.Fprintf(w, "\n### %s\n\n", section.title)
		if len(section.items) == 0 {
			fmt.Fprintln(w, "None")
		}
		for _, issue := range section.items {
			fmt.Fprintf(w, "- [#%d %s](%s)\n", issue.number, linkTitle.Replace(issue.title), issue.url)
		}
	}

	fmt.Fprintln(w, "\n## Issue burndown")
	fmt.Fprintf(w, "\n%s through %s (UTC). Open is the count at the end of each date, or as of this report for today.\n\n",
		r.start.Format(time.DateOnly), r.now.Format(time.DateOnly))
	fmt.Fprintln(w, "| Date\t| Opened\t| Closed\t| Open\t|")
	fmt.Fprintln(w, "| ---\t| ---:\t| ---:\t| ---:\t|")
	for _, day := range slices.SortedFunc(maps.Keys(r.days), time.Time.Compare) {
		row := r.days[day]
		fmt.Fprintf(w, "| %s\t| %d\t| %d\t| %d\t|\n", day.Format(time.DateOnly), row.opened, row.closed, row.open)
	}

	fmt.Fprint(w, "\n### By current milestone\n\nOpened and closed are totals for the window. Open is the count as of the report.\n\n")
	fmt.Fprintln(w, "| Milestone\t| Opened\t| Closed\t| Open\t|")
	fmt.Fprintln(w, "| ---\t| ---:\t| ---:\t| ---:\t|")
	cell := strings.NewReplacer("\\", "\\\\", "|", "\\|", "\t", " ")
	for _, name := range slices.Sorted(maps.Keys(r.milestones)) {
		row := r.milestones[name]
		fmt.Fprintf(w, "| %s\t| %d\t| %d\t| %d\t|\n", cell.Replace(cmp.Or(name, "No milestone")), row.opened, row.closed, row.open)
	}
	fmt.Fprintln(w)
	return w.Flush()
}
