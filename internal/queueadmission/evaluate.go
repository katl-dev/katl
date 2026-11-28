package queueadmission

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/reviewrecord"
)

// Evaluate decides the verdict for the open pull request pr. Every rule that
// applies is reported. A fact that was not read yields incomplete, and the
// rules that need it report nothing.
func Evaluate(facts Facts, pr int) Verdict {
	v := Verdict{PR: pr}
	pull, ok := facts.Pulls[pr]
	if !ok {
		v.Reasons = []Reason{incomplete([]string{"pull"})}
		return v
	}
	v.Head = pull.Head
	if pull.BaseRef != facts.Target.Branch {
		v.Reasons = []Reason{incomplete([]string{"target"})}
		return v
	}

	e := &evaluation{facts: facts, pull: pull, missing: map[string]bool{}}
	e.requireFacts()

	automation := e.automation()
	e.draft()
	e.hold()
	e.dependencies()
	e.review(automation)
	e.conflicts()
	e.unmergeable()
	if !automation {
		e.mergeAlone()
		e.metadata()
	}
	e.releaseNote()
	e.ejected()
	e.ci()
	e.threads()
	if len(e.missing) > 0 {
		e.reasons = append(e.reasons, incomplete(slices.Sorted(maps.Keys(e.missing))))
	}
	e.autoMergeOff()

	slices.SortStableFunc(e.reasons, func(a, b Reason) int {
		i, _ := ruleFor(a.Code)
		j, _ := ruleFor(b.Code)
		return i - j
	})
	// kernel-automerge.yml admits automation pull requests, and no agent
	// owns them.
	if automation {
		for i := range e.reasons {
			e.reasons[i].Actor = ActorNone
		}
	}
	v.Reasons = e.reasons
	return v
}

func newReason(code Code, detail string) Reason {
	_, rule := ruleFor(code)
	return Reason{Code: code, Gates: rule.gates, Actor: rule.actor, Detail: detail, ClearsBy: rule.clearsBy}
}

func incomplete(facts []string) Reason {
	r := newReason(codeIncomplete, "could not read: "+strings.Join(facts, ", "))
	r.Facts = facts
	return r
}

type evaluation struct {
	facts   Facts
	pull    Pull
	missing map[string]bool
	reasons []Reason
}

func (e *evaluation) add(r Reason) {
	e.reasons = append(e.reasons, r)
}

// requireFacts records every absent fact of the evaluation. Every fact is
// always needed, so no rule is skipped silently because another rule made
// its fact irrelevant.
func (e *evaluation) requireFacts() {
	p := e.pull
	for name, known := range map[string]bool{
		"queue":          e.facts.Target.Queue.known,
		"refs":           e.facts.Refs.known,
		"author":         p.Author.known,
		"draft":          p.Draft.known,
		"body":           p.Body.known,
		"labels":         p.Labels.known,
		"closes":         p.Closes.known,
		"blockedBy":      p.BlockedBy.known,
		"paths":          p.Paths.known,
		"review":         p.Review.known,
		"reviewDecision": p.ReviewDecision.known,
		"releaseNotes":   p.ReleaseNotes.known,
		"conflicts":      p.Conflicts.known,
		"removal":        p.Removal.known,
		"autoMerge":      p.AutoMerge.known,
		"mergeable":      p.Mergeable.known,
		"threads":        p.Threads.known,
		"checks":         p.Checks.known,
	} {
		if !known {
			e.missing[name] = true
		}
	}
}

// inQueue reports whether the pull request is a merge-queue entry, and
// whether the queue was read.
func (e *evaluation) inQueue() (queued, known bool) {
	queue, ok := e.facts.Target.Queue.get()
	return slices.ContainsFunc(queue, func(en Entry) bool { return en.Number == e.pull.Number }), ok
}

func (e *evaluation) automation() bool {
	author, okA := e.pull.Author.get()
	paths, okP := e.pull.Paths.get()
	return okA && okP && author.Bot && author.Login == automationLogin && paths.Complete &&
		automationPaths(e.pull.HeadRef, paths.Files)
}

func (e *evaluation) draft() {
	if draft, _ := e.pull.Draft.get(); draft {
		e.add(newReason(codeDraft, "the PR is a draft"))
	}
}

func (e *evaluation) hold() {
	body, _ := e.pull.Body.get()
	holds := HoldLines(body)
	switch {
	case len(holds) == 0:
	case holds[0] == "":
		e.add(newReason(codeHold, "Hold: line names nothing that clears it; say what must happen"))
	default:
		e.add(newReason(codeHold, "Hold: "+holds[0]))
	}
}

// pullDeps lists the declared and blocked-by dependencies of an open pull
// request, and the names of the facts that are missing to know them.
func pullDeps(p Pull) (Node, []string) {
	n := Node{Number: p.Number}
	var missing []string
	body, okBody := p.Body.get()
	blockers, okBlockers := p.BlockedBy.get()
	closes, okCloses := p.Closes.get()
	if !okBody {
		missing = append(missing, "body")
	}
	if !okCloses {
		missing = append(missing, "closes")
	}
	if !okBlockers {
		missing = append(missing, "blockedBy")
	}
	n.Closes = closes
	n.Deps = append(DeclaredDeps(body), blockers.Local...)
	slices.Sort(n.Deps)
	n.Deps = slices.Compact(n.Deps)
	return n, missing
}

func (e *evaluation) dependencies() {
	queue, okQueue := e.facts.Target.Queue.get()
	refs, okRefs := e.facts.Refs.get()
	self, selfMissing := pullDeps(e.pull)
	blockers, _ := e.pull.BlockedBy.get()
	if !okQueue || !okRefs || len(selfMissing) > 0 {
		return
	}

	nodes := map[int]Node{}
	incompleteNodes := map[int][]string{}
	for _, number := range slices.Sorted(maps.Keys(e.facts.Pulls)) {
		n, missing := pullDeps(e.facts.Pulls[number])
		n.Number = number
		nodes[number] = n
		if len(missing) > 0 {
			incompleteNodes[number] = missing
		}
	}
	g := newGraph(slices.Collect(maps.Values(nodes)), queue, refs)

	// A dependency outside the graph is looked up among the issues every
	// open pull request closes, so each pull's closing issues are needed.
	requireClosers := func(deps []Dep) {
		if !slices.ContainsFunc(deps, func(d Dep) bool { _, ok := nodes[d.Number]; return !ok }) {
			return
		}
		for number, missing := range incompleteNodes {
			if number != e.pull.Number && slices.Contains(missing, "closes") {
				e.missing[fmt.Sprintf("pulls[%d].closes", number)] = true
			}
		}
	}
	edges := func(number int) []int {
		if missing, ok := incompleteNodes[number]; ok {
			for _, field := range missing {
				e.missing[fmt.Sprintf("pulls[%d].%s", number, field)] = true
			}
			return nil
		}
		deps := g.resolve(nodes[number])
		requireClosers(deps)
		var on []int
		for _, d := range deps {
			if d.Kind == DepPulls {
				on = append(on, d.On...)
			}
		}
		return on
	}

	deps := g.resolve(self)
	requireClosers(deps)
	var open, brokenPRs []int
	var broken []string
	for _, d := range deps {
		switch d.Kind {
		case DepPulls:
			for _, p := range d.On {
				if reaches(edges, p, e.pull.Number) {
					brokenPRs = append(brokenPRs, p)
					broken = append(broken, fmt.Sprintf("#%d depends back on this PR", p))
				} else {
					open = append(open, p)
				}
			}
		case DepQueued:
			open = append(open, d.On...)
		case DepOpen:
			open = append(open, d.Number)
		case DepUnknown:
			e.missing["refs"] = true
		case DepNotFound:
			brokenPRs = append(brokenPRs, d.Number)
			broken = append(broken, fmt.Sprintf("#%d was not found", d.Number))
		case DepClosed:
			brokenPRs = append(brokenPRs, d.Number)
			broken = append(broken, fmt.Sprintf("#%d was closed without merging", d.Number))
		}
	}
	for _, ext := range blockers.External {
		broken = append(broken, ext+" is in another repository")
	}

	if len(open) > 0 {
		slices.Sort(open)
		open = slices.Compact(open)
		r := newReason(codeDependencyOpen, "waits for "+numbersText(open))
		r.PRs = open
		e.add(r)
	}
	if len(broken) > 0 {
		r := newReason(codeDependencyBroken, strings.Join(broken, "; "))
		r.PRs = brokenPRs
		e.add(r)
	}
}

func (e *evaluation) review(automation bool) {
	rec, okReview := e.pull.Review.get()
	decision, _ := e.pull.ReviewDecision.get()
	if okReview && (rec.Match == reviewrecord.Direct || rec.Match == reviewrecord.EquivalentPatch) && rec.Verdict == reviewrecord.ChangesRequested {
		e.add(newReason(codeChangesRequested, "the review record for this head requests changes"))
	} else if decision == "CHANGES_REQUESTED" {
		e.add(newReason(codeChangesRequested, "a GitHub review requests changes"))
	}

	paths, okPaths := e.pull.Paths.get()
	if automation || !okReview || !okPaths {
		return
	}
	var classified, why []string
	for _, f := range paths.Files {
		reasons := classify(f, classReviewRequired)
		if len(reasons) == 0 {
			continue
		}
		classified = append(classified, f)
		for _, r := range reasons {
			if !slices.Contains(why, r) {
				why = append(why, r)
			}
		}
	}
	if !paths.Complete {
		why = append(why, "the changed-file list is incomplete")
	}
	if len(why) == 0 {
		return
	}

	var r Reason
	switch rec.Match {
	case reviewrecord.Missing:
		r = newReason(codeReviewMissing, "review required ("+strings.Join(why, ", ")+"); no review record")
	case reviewrecord.Stale:
		r = newReason(codeReviewStale, "review required ("+strings.Join(why, ", ")+"); the review record is for an older head")
	default:
		return
	}
	r.Paths = classified[:min(len(classified), shownPaths)]
	e.add(r)
}

func (e *evaluation) conflicts() {
	c, ok := e.pull.Conflicts.get()
	if !ok {
		return
	}
	if len(c.Main) > 0 {
		r := newReason(codeConflictMain, "conflicts with "+e.facts.Target.Branch)
		r.Paths = c.Main
		e.add(r)
	}
	if c.Queued != nil {
		r := newReason(codeConflictQueued, fmt.Sprintf("conflicts with #%d, which is in the merge queue", c.Queued.PR))
		r.PRs, r.Paths = []int{c.Queued.PR}, c.Queued.Paths
		e.add(r)
	}
	for _, p := range c.Pending {
		r := newReason(codeConflictPending, fmt.Sprintf("conflicts with #%d, which has auto-merge enabled and has not joined the queue", p.PR))
		r.PRs, r.Paths = []int{p.PR}, p.Paths
		e.add(r)
	}
}

func (e *evaluation) unmergeable() {
	if m, _ := e.pull.Mergeable.get(); m == "CONFLICTING" {
		e.add(newReason(codeUnmergeable, "GitHub reports a conflict with "+e.facts.Target.Branch))
	}
}

func (e *evaluation) mergeAlone() {
	paths, okPaths := e.pull.Paths.get()
	queue, okQueue := e.facts.Target.Queue.get()
	if queued, _ := e.inQueue(); !okPaths || !okQueue || queued {
		return
	}
	var alone []string
	for _, f := range paths.Files {
		if len(classify(f, classMergeAlone)) > 0 {
			alone = append(alone, f)
		}
	}
	var entries []int
	for _, en := range queue {
		entries = append(entries, en.Number)
	}
	if len(alone) == 0 || len(entries) == 0 {
		return
	}
	r := newReason(codeMergeAlone, "changes what every merge group runs; the queue holds "+numbersText(entries))
	r.PRs, r.Paths = entries, alone
	e.add(r)
}

var kindLabels = []string{"bug", "enhancement", "documentation"}

func (e *evaluation) metadata() {
	labels, ok := e.pull.Labels.get()
	if !ok {
		return
	}
	var kinds []string
	area := false
	for _, l := range labels {
		if slices.Contains(kindLabels, l) {
			kinds = append(kinds, l)
		}
		area = area || strings.HasPrefix(l, "area: ")
	}
	var problems []string
	switch {
	case len(kinds) == 0:
		problems = append(problems, "no kind label ("+strings.Join(kindLabels, ", ")+")")
	case len(kinds) > 1:
		problems = append(problems, "kind labels "+strings.Join(kinds, ", ")+"; keep one")
	}
	if !area {
		problems = append(problems, "no `area: ` label")
	}
	if len(problems) > 0 {
		e.add(newReason(codeMetadata, strings.Join(problems, "; ")))
	}
}

func (e *evaluation) releaseNote() {
	notes, ok := e.pull.ReleaseNotes.get()
	if !ok {
		return
	}
	var problems []string
	if notes.Count > 1 {
		problems = append(problems, fmt.Sprintf("%d `Release-Note:` trailers", notes.Count))
	}
	if notes.MultiLine {
		problems = append(problems, "a `Release-Note:` continues onto a second line")
	}
	if len(problems) > 0 {
		e.add(newReason(codeReleaseNote, strings.Join(problems, "; ")))
	}
}

func (e *evaluation) ejected() {
	removal, _ := e.pull.Removal.get()
	if removal == nil || !removal.ForFailedChecks() || !removal.InForce(e.pull.Head) || removal.LinkedAfter ||
		slices.Equal(removal.FailedChecks, []string{CheckName}) {
		return
	}
	detail := "removed from the merge queue (" + removal.Reason + ")"
	if len(removal.FailedChecks) > 0 {
		detail += ": " + strings.Join(removal.FailedChecks, ", ") + " failed"
	}
	e.add(newReason(codeEjected, detail))
}

func (e *evaluation) ci() {
	if queued, known := e.inQueue(); known && queued {
		e.add(newReason(codeQueued, "in the merge queue"))
	}
	checks, ok := e.pull.Checks.get()
	if !ok {
		return
	}
	var failed, cancelled, optional []string
	running := 0
	for _, c := range checks {
		// The verdict is reported as this check, so reading it would make
		// the verdict depend on itself.
		if c.Name == CheckName {
			continue
		}
		switch {
		case c.State == "failed" && c.Required:
			failed = append(failed, c.Name)
		case c.State == "failed":
			optional = append(optional, c.Name)
		case c.State == "cancelled" && c.Required:
			cancelled = append(cancelled, c.Name)
		case c.State == "running":
			running++
		}
	}

	var problems []string
	if len(failed) > 0 {
		problems = append(problems, "failed: "+strings.Join(failed, ", "))
	}
	for _, name := range cancelled {
		problems = append(problems, name+" cancelled; re-run it")
	}
	if len(problems) > 0 {
		e.add(newReason(codeCIFailed, strings.Join(problems, "; ")))
	}
	if len(optional) > 0 {
		e.add(newReason(codeCIOptionalFailed, "optional checks failed: "+strings.Join(optional, ", ")))
	}
	if len(problems) == 0 && running > 0 {
		e.add(newReason(codeCIRunning, plural(running, "check", "checks")+" running"))
	}
}

func (e *evaluation) threads() {
	if n, _ := e.pull.Threads.get(); n > 0 {
		e.add(newReason(codeThreadsUnresolved, plural(n, "unresolved review thread", "unresolved review threads")))
	}
}

// autoMergeOff reports auto-merge only when it is the last thing in the way,
// so it never changes who must act on a pull request something else blocks.
func (e *evaluation) autoMergeOff() {
	enabled, ok := e.pull.AutoMerge.get()
	queued, _ := e.inQueue()
	if !ok || enabled || queued || slices.ContainsFunc(e.reasons, func(r Reason) bool { return r.Gates != GateNone }) {
		return
	}
	e.add(newReason(codeAutoMergeOff, "auto-merge is not enabled"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func numbersText(numbers []int) string {
	parts := make([]string, len(numbers))
	for i, n := range numbers {
		parts[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}
