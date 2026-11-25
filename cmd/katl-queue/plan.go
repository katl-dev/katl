package main

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// candidate is a ready pull request that could join the merge queue.
type candidate struct {
	Number  int
	Head    string
	Created time.Time
	Labels  []string
	// Closes lists the issues the pull request closes.
	Closes []int
	// Deps lists the issues and pull requests it declares itself blocked
	// by, including those declared on the issues it closes.
	Deps []int
	// External lists the blockers in other repositories, as owner/name#N.
	// Their numbers mean nothing here, so they cannot be checked.
	External     []string
	AutoMerge    bool
	ChecksFailed bool
	// OptionalFailed names failing checks that branch rules do not require.
	OptionalFailed []string
	// Ejected is set when the merge queue removed it for failed checks and
	// its head has not changed since.
	Ejected bool
	// Dequeued is set when someone dequeued it by hand and its head has not
	// changed since.
	Dequeued bool
}

// externalRef is the state of an issue or pull request that a candidate
// depends on but that is not itself a candidate.
type externalRef struct {
	Pull  bool
	State string // OPEN, CLOSED or MERGED
}

type planInput struct {
	Base       string
	Queue      []queuedPR
	Candidates []candidate
	Refs       map[int]externalRef
}

type heldPR struct {
	Number int
	// Blocker is the pull request this one waits behind, or 0.
	Blocker int
	Reason  string
	Files   []string
	// Rebase is set when only a rebase onto the base branch can unblock it.
	Rebase bool
	// Advisory is set when GitHub would still queue the pull request, so
	// auto-merge stays enabled.
	Advisory bool
}

type plan struct {
	// Enqueue lists the pull requests to enqueue, in the order to enqueue
	// them.
	Enqueue []int
	Held    []heldPR
}

// priorityRank orders release blockers first, then dev-experience work, then
// everything else.
func priorityRank(labels []string) int {
	switch {
	case slices.Contains(labels, "release-blocker"):
		return 0
	case slices.Contains(labels, "dev-experience"):
		return 1
	}
	return 2
}

func (c candidate) compare(o candidate) int {
	return cmp.Or(
		cmp.Compare(priorityRank(c.Labels), priorityRank(o.Labels)),
		c.Created.Compare(o.Created),
		cmp.Compare(c.Number, o.Number),
	)
}

var dependsOnLine = regexp.MustCompile(`(?im)^\s*depends on\b(.*)$`)
var issueRef = regexp.MustCompile(`#(\d+)`)

// declaredDeps lists the numbers on "Depends on #N" lines of a pull request
// body.
func declaredDeps(body string) []int {
	var deps []int
	for _, line := range dependsOnLine.FindAllStringSubmatch(body, -1) {
		for _, ref := range issueRef.FindAllStringSubmatch(line[1], -1) {
			n, _ := strconv.Atoi(ref[1])
			deps = append(deps, n)
		}
	}
	return deps
}

// waits is a candidate's resolved ordering constraints. A dependency counts as
// satisfied once it has merged, so a candidate never joins the queue in the
// same pass as something it depends on.
type waits struct {
	// on lists candidates that must merge first.
	on []int
	// problem is set when a dependency is not a candidate and has not
	// merged.
	problem string
	blocker int
}

func resolveDeps(in planInput) map[int]waits {
	byNumber := map[int]candidate{}
	closers := map[int][]int{}
	for _, c := range in.Candidates {
		byNumber[c.Number] = c
		for _, issue := range c.Closes {
			closers[issue] = append(closers[issue], c.Number)
		}
	}
	// queued maps the pull requests in the queue, and the issues they close,
	// to the queued pull request.
	queued := map[int]int{}
	for _, q := range in.Queue {
		queued[q.Number] = q.Number
		for _, issue := range q.Closes {
			queued[issue] = q.Number
		}
	}

	resolved := map[int]waits{}
	for _, c := range in.Candidates {
		var w waits
		unmet := func(blocker int, format string, args ...any) {
			if w.problem == "" {
				w.problem, w.blocker = fmt.Sprintf(format, args...), blocker
			}
		}
		for _, dep := range c.Deps {
			if dep == c.Number || slices.Contains(c.Closes, dep) {
				continue
			}
			_, isCandidate := byNumber[dep]
			ref, known := in.Refs[dep]
			switch {
			case isCandidate:
				w.on = append(w.on, dep)
			case len(closers[dep]) > 0:
				for _, n := range closers[dep] {
					if n != c.Number {
						w.on = append(w.on, n)
					}
				}
			case queued[dep] != 0:
				unmet(queued[dep], "waits for #%d, which is queued and has not merged", queued[dep])
			case !known:
				unmet(dep, "depends on #%d, which was not found", dep)
			case ref.State == "MERGED", ref.State == "CLOSED" && !ref.Pull:
			case ref.State == "CLOSED":
				unmet(dep, "depends on #%d, which was closed without merging", dep)
			default:
				unmet(dep, "waits for #%d, which has not merged", dep)
			}
		}
		slices.Sort(w.on)
		w.on = slices.Compact(w.on)
		resolved[c.Number] = w
	}
	return resolved
}

// dependencyOrder sorts candidates so that each follows what it waits on,
// choosing by priority among those ready. Candidates that can never be
// placed because of a dependency cycle are returned separately.
func dependencyOrder(cands []candidate, resolved map[int]waits) (ordered []candidate, cyclic []candidate) {
	remaining := slices.Clone(cands)
	slices.SortFunc(remaining, candidate.compare)
	placed := map[int]bool{}
	for len(remaining) > 0 {
		i := slices.IndexFunc(remaining, func(c candidate) bool {
			return !slices.ContainsFunc(resolved[c.Number].on, func(n int) bool { return !placed[n] })
		})
		if i < 0 {
			return ordered, remaining
		}
		placed[remaining[i].Number] = true
		ordered = append(ordered, remaining[i])
		remaining = slices.Delete(remaining, i, i+1)
	}
	return ordered, nil
}

// cycleMembers returns the numbers of stuck candidates that can reach
// themselves through their dependencies.
func cycleMembers(stuck []candidate, resolved map[int]waits) []int {
	var members []int
	for _, c := range stuck {
		seen := map[int]bool{}
		var reaches func(n int) bool
		reaches = func(n int) bool {
			for _, dep := range resolved[n].on {
				if dep == c.Number {
					return true
				}
				if !seen[dep] {
					seen[dep] = true
					if reaches(dep) {
						return true
					}
				}
			}
			return false
		}
		if reaches(c.Number) {
			members = append(members, c.Number)
		}
	}
	slices.Sort(members)
	return members
}

func numbersText(numbers []int) string {
	parts := make([]string, len(numbers))
	for i, n := range numbers {
		parts[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// planQueue decides which candidates to enqueue and in what order, and which
// to hold with the reason. Declared dependencies come first, then priority.
// Each candidate is simulated over the base plus the queue plus the
// candidates already chosen, so a candidate that conflicts with an earlier
// choice is held behind it.
func planQueue(ctx context.Context, objects *scratchObjects, in planInput) (plan, error) {
	resolved := resolveDeps(in)
	ordered, stuck := dependencyOrder(in.Candidates, resolved)

	var p plan
	cycle := cycleMembers(stuck, resolved)
	for _, c := range stuck {
		reason := "waits on a candidate in a dependency cycle"
		if slices.Contains(cycle, c.Number) {
			reason = "in a dependency cycle: " + numbersText(cycle)
		}
		p.Held = append(p.Held, heldPR{Number: c.Number, Reason: reason})
	}

	// A pull request others declare a dependency on cannot be left with a
	// failing check, optional or not.
	dependedOn := map[int]bool{}
	for _, w := range resolved {
		for _, n := range w.on {
			dependedOn[n] = true
		}
	}

	chosen := slices.Clone(in.Queue)
	for _, c := range ordered {
		w := resolved[c.Number]
		hold := func(h heldPR) {
			h.Number = c.Number
			p.Held = append(p.Held, h)
		}

		if c.Ejected {
			hold(heldPR{Reason: "was removed from the merge queue for failed checks and no push since is recorded; fix it and push (katl-queue never retries)"})
			continue
		}
		if c.Dequeued {
			hold(heldPR{Reason: "was dequeued by hand; katl-queue leaves it out until a new head is pushed"})
			continue
		}
		if c.ChecksFailed {
			hold(heldPR{Reason: "checks failed; fix them and push (katl-queue never retries checks)"})
			continue
		}
		if len(c.OptionalFailed) > 0 && dependedOn[c.Number] {
			hold(heldPR{Advisory: true, Reason: "optional checks failed (" + strings.Join(c.OptionalFailed, ", ") + ") and other pull requests depend on it; fix them and push"})
			continue
		}
		if len(c.External) > 0 {
			hold(heldPR{Reason: "blocked by " + strings.Join(c.External, ", ") + " in another repository, which katl-queue cannot check"})
			continue
		}
		if w.problem != "" {
			hold(heldPR{Blocker: w.blocker, Reason: w.problem})
			continue
		}
		if len(w.on) > 0 {
			hold(heldPR{Blocker: w.on[0], Reason: fmt.Sprintf("waits for #%d to merge", w.on[0])})
			continue
		}

		sim, err := simulate(ctx, objects, in.Base, chosen, queuedPR{Number: c.Number, Head: c.Head})
		if err != nil {
			return p, err
		}
		switch {
		case sim.clean():
			p.Enqueue = append(p.Enqueue, c.Number)
			chosen = append(chosen, queuedPR{Number: c.Number, Head: c.Head})
		case sim.ConflictWith == 0:
			hold(heldPR{Reason: "conflicts with the base branch; rebase it", Files: sim.Conflicts, Rebase: true})
		default:
			hold(heldPR{Blocker: sim.ConflictWith, Reason: fmt.Sprintf("conflicts with #%d; held behind it", sim.ConflictWith), Files: sim.Conflicts})
		}
	}
	return p, nil
}

// issueInfo is an open issue considered for starting work on.
type issueInfo struct {
	Number   int
	Title    string
	Assigned bool
	Labels   []string
	Created  time.Time
	Blockers []int
	// External lists blockers in other repositories, as owner/name#N.
	External []string
}

// startable lists open, unassigned issues whose blockers are all closed, in
// priority order. Blockers missing from refs count as open, and blockers in
// other repositories cannot be checked, so they hold an issue back.
func startable(issues []issueInfo, refs map[int]externalRef) []issueInfo {
	var out []issueInfo
	for _, issue := range issues {
		if issue.Assigned || len(issue.External) > 0 {
			continue
		}
		open := slices.ContainsFunc(issue.Blockers, func(n int) bool {
			ref, ok := refs[n]
			return !ok || ref.State == "OPEN"
		})
		if !open {
			out = append(out, issue)
		}
	}
	slices.SortFunc(out, func(a, b issueInfo) int {
		return candidate{Number: a.Number, Created: a.Created, Labels: a.Labels}.compare(candidate{Number: b.Number, Created: b.Created, Labels: b.Labels})
	})
	return out
}
