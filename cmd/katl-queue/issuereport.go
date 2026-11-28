package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

const issueLinks = "\n\n<!-- katl-issue links -->\n"

func originalIssueBody(body string) string {
	original, _, _ := strings.Cut(body, issueLinks)
	return original
}

func (i intakeIssue) linkedIssues(key string) []int {
	_, links, managed := strings.Cut(i.Body, issueLinks)
	if !managed && key == "Audits" && i.audit() {
		// Audits recorded by hand can use the same visible membership line.
		links = i.Body
	}
	for line := range strings.Lines(links) {
		if refs, ok := strings.CutPrefix(line, key+": "); ok {
			refs, _, _ = strings.Cut(refs, " — ")
			numbers, _ := parseGitHubNumbers(strings.Split(refs, ","), "issue")
			return numbers
		}
	}
	return nil
}

func appendIntakeLinks(opt intakeOptions, audit int) string {
	var links []string
	for _, link := range []struct {
		key string
		n   int
	}{{"Parent", opt.parent}, {"Supersedes", opt.supersedes}, {"Recurrence audit", audit}} {
		if link.n != 0 {
			links = append(links, fmt.Sprintf("%s: #%d", link.key, link.n))
		}
	}
	if len(opt.distinct) > 0 {
		links = append(links, "Distinct from: "+numbersText(opt.distinct))
	}
	if len(opt.recurrence) > 0 {
		links = append(links, "Recurrence of: "+numbersText(opt.recurrence))
	}
	if len(opt.alone) > 0 {
		links = append(links, "Unrelated: "+numbersText(opt.alone)+" — "+opt.reason)
	}
	if len(links) == 0 {
		return opt.draft.Body
	}
	return opt.draft.Body + issueLinks + strings.Join(links, "\n\n")
}

func (in issueIntake) audits() map[int]map[int]bool {
	audits := map[int]map[int]bool{}
	for n, i := range in.issues {
		if i.audit() {
			members := map[int]bool{}
			for _, member := range i.linkedIssues("Audits") {
				members[member] = true
			}
			audits[n] = members
		}
	}
	for n, i := range in.issues {
		for _, audit := range i.linkedIssues("Recurrence audit") {
			if members := audits[audit]; members != nil {
				members[n] = true
				for _, earlier := range i.linkedIssues("Recurrence of") {
					members[earlier] = true
				}
			}
		}
	}
	return audits
}

func (in issueIntake) findAudit(earlier []int) (int, error) {
	if len(earlier) < 2 {
		return 0, nil
	}
	var found []int
	for n, members := range in.audits() {
		// A filer confirms all earlier bugs share this class. A link to a
		// single audited bug identifies the audit for the whole recurrence.
		if slices.ContainsFunc(earlier, func(bug int) bool { return members[bug] }) {
			found = append(found, n)
		}
	}
	switch len(found) {
	case 0:
		return 0, nil
	case 1:
		if in.issues[found[0]].Closed {
			return 0, fmt.Errorf("recurrence audit #%d is closed; reopen it, or file without --recurrence-of", found[0])
		}
		if in.issues[found[0]].Locked {
			return 0, fmt.Errorf("audit #%d is locked; unlock it before attaching a recurrence", found[0])
		}
		return found[0], nil
	default:
		slices.Sort(found)
		return 0, fmt.Errorf("multiple audits already cover the failure class (%s); reconcile them before filing", numbersText(found))
	}
}

func finishIntake(ctx context.Context, g intakeStore, opt intakeOptions, number, audit int, out io.Writer) error {
	var followups []error
	if opt.parent != 0 {
		if err := g.addSubIssue(ctx, opt.parent, number); err != nil {
			followups = append(followups, fmt.Errorf("add #%d as a sub-issue of #%d: %w", number, opt.parent, err))
		}
	}
	if opt.supersedes != 0 {
		if err := g.comment(ctx, opt.supersedes, fmt.Sprintf("Superseded by #%d.", number)); err != nil {
			followups = append(followups, fmt.Errorf("comment on superseded issue #%d: %w", opt.supersedes, err))
		}
	}
	if len(opt.recurrence) >= 2 {
		if audit != 0 {
			if err := g.comment(ctx, audit, fmt.Sprintf("#%d recurs after %s.", number, numbersText(opt.recurrence))); err != nil {
				followups = append(followups, fmt.Errorf("comment on recurrence audit #%d: %w", audit, err))
			} else {
				fmt.Fprintf(out, "Attached #%d to audit #%d.\n", number, audit)
			}
		} else {
			members := append(slices.Clone(opt.recurrence), number)
			created, err := g.createIntakeIssue(ctx, recurrenceAudit(opt, members))
			if err != nil {
				followups = append(followups, fmt.Errorf("create the recurrence audit for %s: %w", numbersText(members), err))
			} else {
				fmt.Fprintf(out, "Created audit #%d for %s.\n", created.Number, numbersText(members))
			}
		}
	}
	if len(followups) > 0 {
		return fmt.Errorf("#%d was created; check GitHub and finish any missing links or comments: %w", number, errors.Join(followups...))
	}
	return nil
}

func recurrenceAudit(opt intakeOptions, members []int) issueDraft {
	class := strings.TrimSpace(opt.failureClass)
	if class == "" {
		_, after, hasArea := strings.Cut(opt.draft.Title, ": ")
		class = opt.draft.Title
		if hasArea {
			class = after
		}
	}
	return issueDraft{
		issueText: issueText{
			Title: "audit: " + class,
			Body: "Investigate the failure class shared by " + numbersText(members) + ".\n\n" +
				"## Acceptance criteria\n\n" +
				"- Name the invariant the failures violate.\n" +
				"- Search the repository for other code implementing the same responsibility.\n" +
				"- Consolidate that responsibility behind one owner, or record why the implementations differ." +
				issueLinks + "Audits: " + numbersText(members),
		},
		Kind: opt.draft.Kind, Area: opt.draft.Area, Priority: opt.draft.Priority, Milestone: opt.draft.Milestone,
	}
}

func reportIssues(in issueIntake, out io.Writer) {
	edges := map[int][]int{}
	pairs := 0
	for _, n := range slices.Sorted(maps.Keys(in.scorer.documents)) {
		a := in.issues[n]
		for _, match := range in.scorer.rank(a.issueText, n) {
			if match.Number <= n {
				continue
			}
			b := in.issues[match.Number]
			if issueFamily(a, b) {
				continue
			}
			aSupersedes := slices.Contains(a.linkedIssues("Supersedes"), b.Number)
			bSupersedes := slices.Contains(b.linkedIssues("Supersedes"), a.Number)
			if !a.Closed && !b.Closed {
				switch {
				case aSupersedes:
					fmt.Fprintf(out, "Supersession #%d supersedes #%d\n", a.Number, b.Number)
					pairs++
				case bSupersedes:
					fmt.Fprintf(out, "Supersession #%d supersedes #%d\n", b.Number, a.Number)
					pairs++
				case match.strong() && !slices.Contains(a.linkedIssues("Distinct from"), b.Number) &&
					!slices.Contains(b.linkedIssues("Distinct from"), a.Number):
					fmt.Fprintf(out, "Likely duplicate %.2f #%d %s / #%d %s\n", match.Score, a.Number, a.Title, b.Number, b.Title)
					pairs++
				}
			}
			if match.strong() && a.bug() && b.bug() && !aSupersedes && !bSupersedes &&
				!slices.Contains(a.linkedIssues("Unrelated"), b.Number) && !slices.Contains(b.linkedIssues("Unrelated"), a.Number) {
				edges[n] = append(edges[n], b.Number)
				edges[b.Number] = append(edges[b.Number], n)
			}
		}
	}
	if pairs == 0 {
		fmt.Fprintln(out, "No likely duplicate or superseded open pairs.")
	}
	printUnauditedClusters(in.audits(), edges, out)
}

func printUnauditedClusters(audits map[int]map[int]bool, edges map[int][]int, out io.Writer) {
	visited := map[int]bool{}
	for _, n := range slices.Sorted(maps.Keys(edges)) {
		if visited[n] {
			continue
		}
		cluster := []int{n}
		visited[n] = true
		for at := 0; at < len(cluster); at++ {
			for _, neighbor := range edges[cluster[at]] {
				if !visited[neighbor] {
					cluster = append(cluster, neighbor)
					visited[neighbor] = true
				}
			}
		}
		if len(cluster) < 3 {
			continue
		}
		covered := false
		for _, members := range audits {
			if !slices.ContainsFunc(cluster, func(n int) bool { return !members[n] }) {
				covered = true
				break
			}
		}
		if !covered {
			slices.Sort(cluster)
			fmt.Fprintln(out, "Unaudited recurrence cluster", numbersText(cluster))
		}
	}
}
