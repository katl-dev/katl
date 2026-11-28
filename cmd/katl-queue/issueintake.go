package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
)

const (
	issueUsage      = "usage: scripts/katl-issue <new|report> [arguments]\n"
	recurrenceLabel = "recurrence"
)

type intakeStore interface {
	intakeIssues(context.Context) ([]intakeIssue, error)
	issueLabels(context.Context) ([]string, error)
	addRecurrenceLabel(context.Context) error
	createIntakeIssue(context.Context, issueDraft) (intakeIssue, error)
	comment(context.Context, int, string) error
	addSubIssue(context.Context, int, int) error
}

func issueCommand(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(errOut, issueUsage)
		return errors.New("missing command")
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(out, issueUsage)
		return nil
	case "new":
		options, err := parseIntake(args[1:], errOut)
		if err != nil {
			return err
		}
		return fileIssue(ctx, newGH(), options, out)
	case "report":
		flags := flag.NewFlagSet("katl-issue report", flag.ContinueOnError)
		flags.SetOutput(errOut)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() > 0 {
			return errors.New("report takes no arguments")
		}
		issues, err := newGH().intakeIssues(ctx)
		if err != nil {
			return err
		}
		reportIssues(newIssueIntake(issues), out)
		return nil
	default:
		return fmt.Errorf("unknown command %q; use new or report", args[0])
	}
}

type intakeOptions struct {
	draft                         issueDraft
	parent, duplicate, supersedes int
	distinct, recurrence, alone   []int
	reason, failureClass          string
}

func parseIntake(args []string, errOut io.Writer) (intakeOptions, error) {
	var opt intakeOptions
	var bodyFile string
	flags := flag.NewFlagSet("katl-issue new", flag.ContinueOnError)
	flags.SetOutput(errOut)
	d := &opt.draft
	flags.StringVar(&d.Title, "title", "", "issue title")
	flags.StringVar(&bodyFile, "body-file", "", "Markdown body with an Acceptance criteria section")
	flags.StringVar(&d.Kind, "kind", "", "kind label, such as bug, flaky-test, or enhancement")
	flags.StringVar(&d.Area, "area", "", "area label, such as dev-tooling")
	flags.StringVar(&d.Priority, "priority", "", "priority label, such as p1")
	flags.StringVar(&d.Milestone, "milestone", "", "optional milestone title")
	flags.IntVar(&opt.parent, "parent", 0, "parent issue number")
	flags.IntVar(&opt.duplicate, "duplicate-of", 0, "comment on this issue instead of creating one")
	flags.IntVar(&opt.supersedes, "supersedes", 0, "link and comment on the superseded issue")
	flags.Func("distinct-from", "strong open matches for a distinct issue (N[,M], repeatable)", issueNumberList(&opt.distinct))
	flags.Func("recurrence-of", "earlier bugs of the same failure class (N[,M], repeatable)", issueNumberList(&opt.recurrence))
	flags.Func("unrelated", "bug matches from a different failure class (N[,M], repeatable)", issueNumberList(&opt.alone))
	flags.StringVar(&opt.reason, "reason", "", "reason the --unrelated issues are a different failure class")
	flags.StringVar(&opt.failureClass, "failure-class", "", "audit title after 'audit:' (defaults to the proposed title)")
	if err := flags.Parse(args); err != nil {
		return opt, err
	}
	if flags.NArg() > 0 {
		return opt, fmt.Errorf("unexpected argument %q; quote values containing spaces", flags.Arg(0))
	}

	for _, input := range []struct {
		flag  string
		value *string
	}{
		{"title", &d.Title}, {"body-file", &bodyFile}, {"kind", &d.Kind}, {"area", &d.Area}, {"priority", &d.Priority},
	} {
		*input.value = strings.TrimSpace(*input.value)
		if *input.value == "" {
			return opt, fmt.Errorf("--%s is required", input.flag)
		}
	}
	if strings.ContainsAny(d.Title, "\r\n") || strings.ContainsAny(opt.failureClass, "\r\n") {
		return opt, errors.New("--title and --failure-class must each be one line")
	}
	if opt.parent < 0 || opt.duplicate < 0 || opt.supersedes < 0 {
		return opt, errors.New("--parent, --duplicate-of and --supersedes take positive issue numbers")
	}
	if opt.duplicate != 0 && (opt.parent != 0 || opt.supersedes != 0 || len(opt.distinct)+len(opt.recurrence)+len(opt.alone) > 0) {
		return opt, errors.New("--duplicate-of only posts a comment; remove the issue creation options")
	}
	opt.reason = strings.Join(strings.Fields(opt.reason), " ")
	if (len(opt.alone) > 0) != (opt.reason != "") {
		return opt, errors.New("--unrelated and --reason must be supplied together")
	}
	if opt.failureClass != "" && len(opt.recurrence) < 2 {
		return opt, errors.New("--failure-class requires at least two --recurrence-of issues")
	}

	d.Area = prefixedLabel("area: ", d.Area)
	d.Priority = prefixedLabel("priority: ", d.Priority)
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		return opt, err
	}
	d.Body = strings.TrimSpace(string(body))
	if strings.Contains(d.Body, strings.TrimSpace(issueLinks)) {
		return opt, errors.New("--body-file contains a katl-issue link marker; remove the generated links before filing")
	}
	if !hasAcceptanceCriteria(d.Body) {
		return opt, errors.New("--body-file needs an Acceptance criteria section with at least one criterion")
	}
	return opt, nil
}

func prefixedLabel(prefix, value string) string {
	return prefix + strings.TrimSpace(strings.TrimPrefix(value, prefix))
}

func issueNumberList(dst *[]int) func(string) error {
	return func(arg string) error {
		numbers, err := parseGitHubNumbers(strings.Split(arg, ","), "issue")
		if err != nil {
			return err
		}
		*dst = append(*dst, numbers...)
		slices.Sort(*dst)
		*dst = slices.Compact(*dst)
		return nil
	}
}

var issueHeading = regexp.MustCompile(`^#{1,6}[ \t]+(.+)`)

func hasAcceptanceCriteria(body string) bool {
	var fence string
	inSection := false
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(line, fence) && strings.Trim(line, fence[:1]) == "" {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fence = line[:len(line)-len(strings.TrimLeft(line, line[:1]))]
			continue
		}
		if heading := issueHeading.FindStringSubmatch(line); heading != nil {
			inSection = strings.EqualFold(strings.Trim(heading[1], " #"), "Acceptance criteria")
		} else if inSection && len(strings.Trim(line, "-*+0123456789.) ")) > 0 {
			return true
		}
	}
	return false
}

type issueIntake struct {
	issues map[int]intakeIssue
	scorer issueScorer
}

func newIssueIntake(issues []intakeIssue) issueIntake {
	index := make(map[int]intakeIssue, len(issues))
	corpus := make(map[int]issueText, len(issues))
	for _, issue := range issues {
		index[issue.Number] = issue
		if !issue.Closed || issue.bug() {
			corpus[issue.Number] = issueText{issue.Title, originalIssueBody(issue.Body)}
		}
	}
	return issueIntake{issues: index, scorer: newIssueScorer(corpus)}
}

func issueFamily(a, b intakeIssue) bool {
	return (a.Parent != 0 && (a.Parent == b.Number || a.Parent == b.Parent)) ||
		(b.Parent != 0 && b.Parent == a.Number)
}

func (i intakeIssue) audit() bool { return strings.HasPrefix(i.Title, "audit: ") }

func (i issueIntake) printMatches(draft intakeIssue, matches []issueMatch, out io.Writer) {
	shown := 0
	for _, m := range matches {
		target := i.issues[m.Number]
		if issueFamily(draft, target) || !m.Identical && m.Score < issueMatchFloor ||
			target.Closed && (!draft.bug() || !target.bug()) {
			continue
		}
		label := "Match"
		if draft.bug() && target.bug() {
			label = "Recurrence"
		}
		if shown < 5 {
			fmt.Fprintf(out, "%s %.2f ", label, m.Score)
			printIntakeIssue(out, target)
			shown++
		} else if label == "Recurrence" && m.strong() {
			fmt.Fprintf(out, "Recurrence %.2f #%d %s\n", m.Score, target.Number, target.Title)
		}
	}
	if shown == 0 {
		fmt.Fprintln(out, "No matches above the reporting floor.")
	}
	if draft.Parent != 0 {
		for _, number := range slices.Sorted(maps.Keys(i.issues)) {
			target := i.issues[number]
			if issueFamily(draft, target) {
				fmt.Fprint(out, "Family ")
				printIntakeIssue(out, target)
			}
		}
	}
}

func printIntakeIssue(out io.Writer, issue intakeIssue) {
	summary := ""
	for line := range strings.Lines(originalIssueBody(issue.Body)) {
		if summary = strings.TrimSpace(line); summary != "" {
			break
		}
	}
	runes := []rune(summary)
	if len(runes) > 120 {
		summary = string(runes[:119]) + "…"
	}
	state := "open"
	if issue.Closed {
		state = "closed"
	}
	fmt.Fprintf(out, "#%d %s [%s, milestone %s]\n", issue.Number, issue.Title, state, cmp.Or(issue.Milestone, "none"))
	if summary != "" {
		fmt.Fprintf(out, "  %s\n", summary)
	}
}

func fileIssue(ctx context.Context, g intakeStore, opt intakeOptions, out io.Writer) error {
	issues, err := g.intakeIssues(ctx)
	if err != nil {
		return err
	}
	in := newIssueIntake(issues)
	draft := intakeIssue{issueText: opt.draft.issueText, Parent: opt.parent, Labels: opt.draft.labels()}
	matches := in.scorer.rank(draft.issueText, 0)
	in.printMatches(draft, matches, out)

	if err := in.validateDecisions(draft, opt, matches); err != nil {
		return err
	}
	labels, err := g.issueLabels(ctx)
	if err != nil {
		return err
	}
	if err := checkIntakeLabels(opt.draft, labels); err != nil {
		return err
	}
	if opt.duplicate != 0 {
		if err := g.comment(ctx, opt.duplicate, "### "+draft.Title+"\n\n"+draft.Body); err != nil {
			return err
		}
		fmt.Fprintf(out, "Commented on #%d.\n", opt.duplicate)
		return nil
	}

	audit, err := in.findAudit(opt.recurrence)
	if err != nil {
		return err
	}
	if len(opt.recurrence) > 0 {
		if !slices.Contains(labels, recurrenceLabel) {
			if err := g.addRecurrenceLabel(ctx); err != nil {
				return fmt.Errorf("ensure the %q label: %w", recurrenceLabel, err)
			}
		}
		opt.draft.Recurrence = true
	}
	opt.draft.Body = appendIntakeLinks(opt, audit)
	created, err := g.createIntakeIssue(ctx, opt.draft)
	if err != nil {
		return fmt.Errorf("create issue (check the report or GitHub before retrying a failed request): %w", err)
	}
	fmt.Fprintf(out, "Created #%d.\n", created.Number)
	return finishIntake(ctx, g, opt, created.Number, audit, out)
}

func checkIntakeLabels(d issueDraft, labels []string) error {
	for _, p := range []struct{ flag, label string }{
		{"kind", d.Kind}, {"area", d.Area}, {"priority", d.Priority},
	} {
		if !slices.Contains(labels, p.label) {
			return fmt.Errorf("--%s label %q does not exist in the repository", p.flag, p.label)
		}
	}
	switch d.Kind {
	case "bug", "flaky-test", "enhancement", "documentation", "dev-experience", "question", "test-design":
		return nil
	default:
		return fmt.Errorf("--kind %q is not an issue kind", d.Kind)
	}
}

func (in issueIntake) validateDecisions(draft intakeIssue, opt intakeOptions, matches []issueMatch) error {
	for _, n := range []int{opt.parent, opt.duplicate, opt.supersedes} {
		if n == 0 {
			continue
		}
		if _, ok := in.issues[n]; !ok {
			return fmt.Errorf("issue #%d was not found in the repository", n)
		}
	}
	for _, n := range []int{opt.duplicate, opt.supersedes} {
		if n != 0 && in.issues[n].Locked {
			return fmt.Errorf("issue #%d is locked; unlock it before posting a comment", n)
		}
	}
	if opt.duplicate != 0 {
		return nil
	}
	for _, n := range opt.distinct {
		if target, ok := in.issues[n]; !ok || target.Closed || n == opt.supersedes {
			return fmt.Errorf("--distinct-from #%d must name an open issue you are not superseding", n)
		}
	}
	for _, n := range append(slices.Clone(opt.recurrence), opt.alone...) {
		target := in.issues[n]
		if !draft.bug() || !target.bug() || issueFamily(draft, target) {
			return fmt.Errorf("#%d must be a bug or flaky-test outside the proposed bug's family", n)
		}
		if slices.Contains(opt.recurrence, n) && slices.Contains(opt.alone, n) {
			return fmt.Errorf("#%d cannot be both --recurrence-of and --unrelated", n)
		}
	}

	var unresolved, related []int
	for _, match := range matches {
		n := match.Number
		target := in.issues[n]
		if match.Identical && !target.Closed {
			return fmt.Errorf("identical to open issue #%d; use --duplicate-of %d to comment there", n, n)
		}
		if !match.strong() || issueFamily(draft, target) {
			continue
		}
		if !target.Closed && n != opt.supersedes && !slices.Contains(opt.distinct, n) {
			unresolved = append(unresolved, n)
		}
		if draft.bug() && target.bug() && n != opt.supersedes &&
			!slices.Contains(opt.recurrence, n) && !slices.Contains(opt.alone, n) {
			related = append(related, n)
		}
	}
	var decisions []error
	if len(unresolved) > 0 {
		decisions = append(decisions, fmt.Errorf("strong open matches %s require --distinct-from, --supersedes or --duplicate-of", numbersText(unresolved)))
	}
	if len(related) > 0 {
		decisions = append(decisions, fmt.Errorf("recurrences %s require --recurrence-of or --unrelated with --reason", numbersText(related)))
	}
	return errors.Join(decisions...)
}
