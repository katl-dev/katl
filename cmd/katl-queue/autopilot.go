package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// httpsCredential makes git authenticate to GitHub over HTTPS through the
// GitHub CLI, which keeps working when the SSH agent is locked.
var httpsCredential = []string{"-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential"}

func (g *gh) remoteURL() string {
	return "https://github.com/" + g.owner + "/" + g.name + ".git"
}

type actionKind string

const (
	actionDequeue actionKind = "dequeue"
	actionExplain actionKind = "explain"
	actionUnlabel actionKind = "unlabel"
	actionRebase  actionKind = "rebase"
	actionEnqueue actionKind = "enqueue"
)

var actionOrder = []actionKind{actionDequeue, actionExplain, actionUnlabel, actionRebase, actionEnqueue}

type action struct {
	Kind   actionKind
	Number int
	// Head and Branch identify what a rebase starts from.
	Head   string
	Branch string
	Detail string
	// Files lists the conflicted paths a rebase is expected to hit.
	Files []string
	// RemovedAt is the removal an explain action reports.
	RemovedAt time.Time
}

// decision is what one autopilot pass would do.
type decision struct {
	Actions []action
	Plan    plan
}

// candidates lists the pull requests that could join the queue now.
func candidates(w world) []candidate {
	var out []candidate
	for _, pr := range w.PRs {
		if w.eligible(pr) && !pr.InQueue {
			out = append(out, pr.candidate)
		}
	}
	return out
}

// planFor plans the candidates over the world's queue. Open pull requests
// outside the candidate set count as open dependencies.
func planFor(ctx context.Context, objects *scratchObjects, w world, cands []candidate) (plan, error) {
	refs := map[int]externalRef{}
	for _, pr := range w.PRs {
		refs[pr.Number] = externalRef{Pull: true, State: "OPEN"}
	}
	for n, ref := range w.Refs {
		refs[n] = ref
	}
	return planQueue(ctx, objects, planInput{Base: w.Base, Queue: w.Queue, Candidates: cands, Refs: refs})
}

// decide derives one autopilot pass from the world. It performs no side
// effects, so a dry run is the same decision without applying it.
func decide(ctx context.Context, objects *scratchObjects, w world) (decision, error) {
	var d decision

	unmergeable := map[int]bool{}
	for _, e := range w.Entries {
		if e.State == "UNMERGEABLE" {
			unmergeable[e.Number] = true
			d.Actions = append(d.Actions, action{Kind: actionDequeue, Number: e.Number, Detail: "the queue marked the entry unmergeable"})
		}
	}
	w.Queue = slices.DeleteFunc(slices.Clone(w.Queue), func(q queuedPR) bool { return unmergeable[q.Number] })

	var cands []candidate
	for _, pr := range w.PRs {
		if !w.eligible(pr) || pr.InQueue {
			continue
		}
		if pr.Removal != nil && pr.Removal.Reason != "merged" && !slices.Contains(pr.Reported, pr.Removal.At.UTC().Format(time.RFC3339)) {
			d.Actions = append(d.Actions, action{Kind: actionExplain, Number: pr.Number, RemovedAt: pr.Removal.At, Detail: "removed from the queue: " + pr.Removal.Reason})
		}

		sim, err := simulate(ctx, objects, w.Base, nil, queuedPR{Number: pr.Number, Head: pr.Head})
		if err != nil {
			return d, err
		}
		switch {
		case sim.clean() && pr.NeedsLabel:
			d.Actions = append(d.Actions, action{Kind: actionUnlabel, Number: pr.Number, Detail: "now merges cleanly over " + w.Branch})
		case !sim.clean() && pr.NeedsLabel:
			// Resolving the conflict needs a person; the label stays
			// until the pull request merges cleanly again.
			continue
		case !sim.clean():
			d.Actions = append(d.Actions, action{Kind: actionRebase, Number: pr.Number, Head: pr.Head, Branch: pr.HeadRef, Files: sim.Conflicts,
				Detail: "conflicts with " + w.Branch})
			continue
		}
		cands = append(cands, pr.candidate)
	}

	var err error
	if d.Plan, err = planFor(ctx, objects, w, cands); err != nil {
		return d, err
	}
	for _, n := range d.Plan.Enqueue {
		if !slices.ContainsFunc(cands, func(c candidate) bool { return c.Number == n && c.AutoMerge }) {
			d.Actions = append(d.Actions, action{Kind: actionEnqueue, Number: n})
		}
	}

	slices.SortStableFunc(d.Actions, func(a, b action) int {
		return slices.Index(actionOrder, a.Kind) - slices.Index(actionOrder, b.Kind)
	})
	return d, nil
}

func (a action) String() string {
	text := fmt.Sprintf("%s #%d", a.Kind, a.Number)
	if a.Detail != "" {
		text += ": " + a.Detail
	}
	return text
}

// effects is every change autopilot makes outside the process. It exists so
// that tests can substitute the GitHub and git side effects.
type effects interface {
	Enqueue(ctx context.Context, number int) error
	Dequeue(ctx context.Context, number int) error
	AddLabel(ctx context.Context, number int, label string) error
	RemoveLabel(ctx context.Context, number int, label string) error
	Comment(ctx context.Context, number int, body string) error
	// Why returns the katl-queue why report for a pull request.
	Why(ctx context.Context, number int) (string, error)
	// Rebase rebases head onto the base branch and pushes the result with
	// a lease on head. It reports the conflicted paths instead of pushing
	// when the rebase does not apply cleanly.
	Rebase(ctx context.Context, branch, head string) (conflicts []string, err error)
}

// apply performs the actions in order. A failure on one pull request does
// not stop the others.
func apply(ctx context.Context, fx effects, actions []action, out io.Writer) error {
	var errs []error
	for _, a := range actions {
		fmt.Fprintln(out, a)
		if err := applyOne(ctx, fx, a, out); err != nil {
			fmt.Fprintf(out, "  failed: %v\n", err)
			errs = append(errs, fmt.Errorf("#%d: %w", a.Number, err))
		}
	}
	return errors.Join(errs...)
}

func applyOne(ctx context.Context, fx effects, a action, out io.Writer) error {
	switch a.Kind {
	case actionEnqueue:
		return fx.Enqueue(ctx, a.Number)
	case actionDequeue:
		return fx.Dequeue(ctx, a.Number)
	case actionUnlabel:
		return fx.RemoveLabel(ctx, a.Number, needsRebaseLabel)
	case actionExplain:
		report, err := fx.Why(ctx, a.Number)
		if err != nil {
			return err
		}
		return fx.Comment(ctx, a.Number, ejectionMarker(a.RemovedAt)+"\n```\n"+strings.TrimSpace(report)+"\n```")
	case actionRebase:
		conflicts, err := fx.Rebase(ctx, a.Branch, a.Head)
		if err != nil {
			return err
		}
		if len(conflicts) == 0 {
			fmt.Fprintln(out, "  rebased and pushed")
			return nil
		}
		fmt.Fprintf(out, "  rebase conflicts in %s; labelled %s and stopped\n", strings.Join(conflicts, ", "), needsRebaseLabel)
		body := "Rebasing onto the base branch conflicts in:\n\n"
		for _, path := range conflicts {
			body += "- `" + path + "`\n"
		}
		body += "\nResolve the conflict and push; katl-queue removes `" + needsRebaseLabel + "` once the pull request merges cleanly."
		return errors.Join(fx.AddLabel(ctx, a.Number, needsRebaseLabel), fx.Comment(ctx, a.Number, body))
	}
	return fmt.Errorf("unknown action %q", a.Kind)
}

// ghEffects applies actions to GitHub through the CLI and pushes over HTTPS.
type ghEffects struct {
	g     *gh
	base  string
	local string
}

func (e ghEffects) repoArgs() []string { return []string{"--repo", e.g.owner + "/" + e.g.name} }

func (e ghEffects) Enqueue(ctx context.Context, number int) error {
	return e.g.enableAutoMerge(ctx, number)
}

func (e ghEffects) Dequeue(ctx context.Context, number int) error {
	id, err := runGH(ctx, append([]string{"pr", "view", fmt.Sprint(number), "--json", "id", "--jq", ".id"}, e.repoArgs()...)...)
	if err != nil {
		return err
	}
	_, err = runGH(ctx, "api", "graphql", "-f", "id="+strings.TrimSpace(string(id)), "-f",
		"query=mutation($id: ID!) { dequeuePullRequest(input: {id: $id}) { clientMutationId } }")
	return err
}

func (e ghEffects) AddLabel(ctx context.Context, number int, label string) error {
	_, err := runGH(ctx, append([]string{"pr", "edit", fmt.Sprint(number), "--add-label", label}, e.repoArgs()...)...)
	return err
}

func (e ghEffects) RemoveLabel(ctx context.Context, number int, label string) error {
	_, err := runGH(ctx, append([]string{"pr", "edit", fmt.Sprint(number), "--remove-label", label}, e.repoArgs()...)...)
	return err
}

func (e ghEffects) Comment(ctx context.Context, number int, body string) error {
	_, err := runGH(ctx, append([]string{"pr", "comment", fmt.Sprint(number), "--body", body}, e.repoArgs()...)...)
	return err
}

func (e ghEffects) Why(ctx context.Context, number int) (string, error) {
	var out bytes.Buffer
	err := why(ctx, e.g, number, &out)
	return out.String(), err
}

func (e ghEffects) Rebase(ctx context.Context, branch, head string) ([]string, error) {
	return rebaser{local: e.local, remote: e.g.remoteURL(), auth: httpsCredential, env: os.Environ()}.rebase(ctx, branch, head, e.base)
}

// rebaser rebases a pull request branch in a throwaway repository, so the
// operator's checkout, branches and index are never touched.
type rebaser struct {
	local  string // repository whose objects are borrowed to avoid refetching
	remote string
	auth   []string
	env    []string
}

func (r rebaser) rebase(ctx context.Context, branch, head, base string) ([]string, error) {
	dir, err := os.MkdirTemp("", "katl-queue-rebase-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	env := slices.Clone(r.env)
	for _, key := range []string{"name", "email"} {
		value := strings.TrimSpace(gitOutput(ctx, r.local, r.env, "config", "user."+key))
		if value == "" {
			value = "katl-queue"
			if key == "email" {
				value = "katl-queue@invalid"
			}
		}
		env = append(env, "GIT_COMMITTER_"+strings.ToUpper(key)+"="+value)
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = dir, env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return string(out), nil
	}

	if _, err := git("init", "--quiet"); err != nil {
		return nil, err
	}
	if objects := strings.TrimSpace(gitOutput(ctx, r.local, r.env, "rev-parse", "--path-format=absolute", "--git-path", "objects")); objects != "" {
		if err := os.WriteFile(filepath.Join(dir, ".git", "objects", "info", "alternates"), []byte(objects+"\n"), 0o644); err != nil {
			return nil, err
		}
	}
	if _, err := git(append(slices.Clone(r.auth), "fetch", "--quiet", "--no-tags", r.remote, head, base)...); err != nil {
		return nil, err
	}
	if _, err := git("checkout", "--quiet", "--detach", head); err != nil {
		return nil, err
	}
	if _, err := git("rebase", base); err != nil {
		unmerged, _ := git("diff", "--name-only", "--diff-filter=U")
		git("rebase", "--abort")
		if conflicts := strings.Fields(unmerged); len(conflicts) > 0 {
			return conflicts, nil
		}
		return nil, err
	}

	// The lease on the head the decision was made from keeps a push the
	// author made meanwhile from being overwritten.
	ref := "refs/heads/" + branch
	_, err = git(append(slices.Clone(r.auth), "push", "--quiet", "--force-with-lease="+ref+":"+head, r.remote, "HEAD:"+ref)...)
	return nil, err
}

func gitOutput(ctx context.Context, dir string, env []string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = dir, env
	out, _ := cmd.Output()
	return string(out)
}

// autopilotPass reads the world, decides, and applies the decision unless
// dryRun is set.
func autopilotPass(ctx context.Context, g *gh, milestone string, dryRun bool, out io.Writer) error {
	w, err := g.world(ctx, milestone)
	if err != nil {
		return err
	}
	objects, err := newScratchObjects(".", os.Environ())
	if err != nil {
		return err
	}
	defer objects.Close()
	objects.config = httpsCredential
	if err := objects.fetch(ctx, g.remoteURL(), worldOids(w)...); err != nil {
		return err
	}

	d, err := decide(ctx, objects, w)
	if err != nil {
		return err
	}
	reportHeld(out, d.Plan)
	if len(d.Actions) == 0 {
		fmt.Fprintln(out, "nothing to do")
		return nil
	}
	if dryRun {
		for _, a := range d.Actions {
			fmt.Fprintf(out, "would %s\n", a)
		}
		fmt.Fprintln(out, "dry run: pass --apply to do this")
		return nil
	}
	return apply(ctx, ghEffects{g: g, base: w.Base, local: "."}, d.Actions, out)
}

func worldOids(w world) []string {
	oids := []string{w.Base}
	for _, q := range w.Queue {
		oids = append(oids, q.Head)
	}
	for _, pr := range w.PRs {
		oids = append(oids, pr.Head)
	}
	slices.Sort(oids)
	return slices.Compact(oids)
}

// autopilot repeats passes until interrupted, or runs one when once is set.
func autopilot(ctx context.Context, g *gh, milestone string, dryRun, once bool, interval time.Duration, out io.Writer) error {
	for {
		fmt.Fprintf(out, "%s autopilot pass\n", stamp())
		err := autopilotPass(ctx, g, milestone, dryRun, out)
		if once {
			return err
		}
		if err != nil {
			fmt.Fprintf(out, "%s pass failed: %v\n", stamp(), err)
		}
		if !sleep(ctx, interval) {
			return nil
		}
	}
}
