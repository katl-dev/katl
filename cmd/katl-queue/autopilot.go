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

	"github.com/katl-dev/katl/internal/queuemarker"
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
	actionDisable actionKind = "disable-auto-merge"
	actionExplain actionKind = "explain"
	actionRecord  actionKind = "record"
	actionReport  actionKind = "report"
	actionUnlabel actionKind = "unlabel"
	actionRebase  actionKind = "rebase"
	actionEnqueue actionKind = "enqueue"
)

var actionOrder = []actionKind{actionDequeue, actionDisable, actionExplain, actionRecord, actionReport, actionUnlabel, actionRebase, actionEnqueue}

type action struct {
	Kind   actionKind
	Number int
	// ID is the GraphQL ID of the pull request a dequeue acts on.
	ID string
	// Head and Branch identify what a rebase starts from, and the head a
	// record or report is about.
	Head   string
	Branch string
	Detail string
	// Checks names the failing optional checks a report lists.
	Checks []string
	// Files lists the conflicted paths a rebase is expected to hit.
	Files []string
	// RemovalID is the removal an explain or record action is about.
	RemovalID string
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
//
// A pull request is rebased only when the plan would otherwise enqueue it:
// one that is held for failed checks, an ejection, unmet dependencies or a
// cycle is left for its author.
func decide(ctx context.Context, objects *scratchObjects, w world) (decision, error) {
	var d decision

	byNumber := map[int]openPR{}
	for _, pr := range w.PRs {
		byNumber[pr.Number] = pr
	}

	unmergeable := map[int]bool{}
	for _, e := range w.Entries {
		if pr, ok := byNumber[e.Number]; e.State == "UNMERGEABLE" && ok && w.eligible(pr) {
			unmergeable[e.Number] = true
			d.Actions = append(d.Actions, action{Kind: actionDequeue, Number: e.Number, ID: e.ID, Head: pr.Head, Detail: "the queue marked the entry unmergeable"})
		}
	}
	w.Queue = slices.DeleteFunc(slices.Clone(w.Queue), func(q queuedPR) bool { return unmergeable[q.Number] })

	var cands []candidate
	for _, pr := range w.PRs {
		if !w.eligible(pr) || pr.InQueue {
			continue
		}
		if pr.unrecorded() {
			switch pr.removalKind() {
			case removalEjected:
				d.Actions = append(d.Actions, action{Kind: actionExplain, Number: pr.Number, RemovalID: pr.Removal.ID, Head: pr.Head, Detail: "removed from the queue: " + pr.Removal.Reason})
			case removalDequeued:
				d.Actions = append(d.Actions, action{Kind: actionRecord, Number: pr.Number, RemovalID: pr.Removal.ID, Head: pr.Head, Detail: "dequeued by hand; leaving it out until the head changes"})
			}
		}
		if len(pr.OptionalFailed) > 0 && !pr.OptionalReported {
			d.Actions = append(d.Actions, action{Kind: actionReport, Number: pr.Number, Head: pr.Head, Checks: pr.OptionalFailed, Detail: "optional checks failed: " + strings.Join(pr.OptionalFailed, ", ")})
		}
		if pr.NeedsLabel {
			sim, err := simulate(ctx, objects, w.Base, nil, queuedPR{Number: pr.Number, Head: pr.Head})
			if err != nil {
				return d, err
			}
			if sim.clean() {
				d.Actions = append(d.Actions, action{Kind: actionUnlabel, Number: pr.Number, Detail: "now merges cleanly over " + w.Branch})
			}
		}
		cands = append(cands, pr.candidate)
	}

	var err error
	if d.Plan, err = planFor(ctx, objects, w, cands); err != nil {
		return d, err
	}
	for _, h := range d.Plan.Held {
		pr := byNumber[h.Number]
		if pr.AutoMerge && !h.Advisory && !h.Rebase {
			d.Actions = append(d.Actions, action{Kind: actionDisable, Number: pr.Number, Detail: "held, so auto-merge must not queue it: " + h.Reason})
		}
		// A label means an earlier rebase conflicted and a person must
		// resolve it.
		if h.Rebase && !pr.NeedsLabel {
			d.Actions = append(d.Actions, action{Kind: actionRebase, Number: pr.Number, Head: pr.Head, Branch: pr.HeadRef, Files: h.Files,
				Detail: "conflicts with " + w.Branch})
		}
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

// rebaseResult is the outcome of a rebase: the paths that conflicted, or the
// head that was pushed.
type rebaseResult struct {
	Conflicts []string
	Head      string
}

// effects is every change autopilot makes outside the process. It exists so
// that tests can substitute the GitHub and git side effects.
type effects interface {
	Enqueue(ctx context.Context, number int) error
	// Dequeue removes the pull request with the given GraphQL ID from the
	// merge queue.
	Dequeue(ctx context.Context, id string) error
	DisableAutoMerge(ctx context.Context, number int) error
	AddLabel(ctx context.Context, number int, label string) error
	RemoveLabel(ctx context.Context, number int, label string) error
	Comment(ctx context.Context, number int, body string) error
	// Markers lists the markers in the comments katl-queue wrote.
	Markers(ctx context.Context, number int) ([]queuemarker.Marker, error)
	// Why returns the katl-queue why report for a pull request.
	Why(ctx context.Context, number int) (string, error)
	// Rebase rebases head onto the base branch and pushes the result with
	// a lease on head. It reports the conflicted paths instead of pushing
	// when the rebase does not apply cleanly.
	Rebase(ctx context.Context, branch, head string) (rebaseResult, error)
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
		// GitHub records the dequeue as a manual removal by this login,
		// the same as a hand dequeue; the marker is what tells them apart,
		// so it goes first.
		if err := fx.Comment(ctx, a.Number, dequeueMarker(a.Head).String()+"\nDequeued by katl-queue because the queue marked the entry unmergeable. It stays out of the queue until a new head is pushed."); err != nil {
			return err
		}
		return fx.Dequeue(ctx, a.ID)
	case actionDisable:
		return fx.DisableAutoMerge(ctx, a.Number)
	case actionUnlabel:
		return fx.RemoveLabel(ctx, a.Number, needsRebaseLabel)
	case actionExplain:
		report, err := fx.Why(ctx, a.Number)
		if err != nil {
			return err
		}
		return fx.Comment(ctx, a.Number, removalMarker(a.RemovalID, a.Head).String()+"\n```\n"+strings.TrimSpace(report)+"\n```")
	case actionRecord:
		return fx.Comment(ctx, a.Number, removalMarker(a.RemovalID, a.Head).String()+"\nDequeued by hand. katl-queue leaves it out of the queue until a new head is pushed.")
	case actionReport:
		body := optionalMarker(a.Head).String() + "\nOptional checks failed on `" + short(a.Head) + "`. They do not block merging, so katl-queue leaves the pull request alone:\n\n"
		for _, name := range a.Checks {
			body += "- " + name + "\n"
		}
		return fx.Comment(ctx, a.Number, body)
	case actionRebase:
		return applyRebase(ctx, fx, a, out)
	}
	return fmt.Errorf("unknown action %q", a.Kind)
}

func applyRebase(ctx context.Context, fx effects, a action, out io.Writer) error {
	res, err := fx.Rebase(ctx, a.Branch, a.Head)
	if err != nil {
		return err
	}
	if len(res.Conflicts) == 0 {
		fmt.Fprintln(out, "  rebased and pushed")
		return fx.Comment(ctx, a.Number, "Rebased onto the base branch by katl-queue; the head is now `"+short(res.Head)+"`.")
	}

	fmt.Fprintf(out, "  rebase conflicts in %s; labelling %s and stopping\n", strings.Join(res.Conflicts, ", "), needsRebaseLabel)
	if err := fx.AddLabel(ctx, a.Number, needsRebaseLabel); err != nil {
		return err
	}
	marks, err := fx.Markers(ctx, a.Number)
	if err != nil {
		return err
	}
	conflict := queuemarker.Marker{Kind: "needs-rebase", Value: a.Head}
	if slices.Contains(marks, conflict) {
		return nil
	}
	body := conflict.String() + "\nRebasing onto the base branch conflicts in:\n\n"
	for _, path := range res.Conflicts {
		body += "- `" + path + "`\n"
	}
	body += "\nResolve the conflict and push; katl-queue removes `" + needsRebaseLabel + "` once the pull request merges cleanly."
	return fx.Comment(ctx, a.Number, body)
}

// ghEffects applies actions to GitHub through the CLI and pushes over HTTPS.
type ghEffects struct {
	g     *gh
	base  string
	local string
	out   io.Writer
}

func (e ghEffects) repoArgs() []string { return []string{"--repo", e.g.owner + "/" + e.g.name} }

// Enqueue enables auto-merge and waits for the entry to appear: GitHub queues
// asynchronously, so enabling the next before then would reorder the queue.
func (e ghEffects) Enqueue(ctx context.Context, number int) error {
	if err := e.g.enableAutoMerge(ctx, number); err != nil {
		return err
	}
	waitQueued(ctx, e.g, number, e.out)
	return nil
}

func (e ghEffects) Dequeue(ctx context.Context, id string) error {
	_, err := runGH(ctx, "api", "graphql", "-f", "id="+id, "-f",
		"query=mutation($id: ID!) { dequeuePullRequest(input: {id: $id}) { clientMutationId } }")
	return err
}

func (e ghEffects) DisableAutoMerge(ctx context.Context, number int) error {
	return e.g.disableAutoMerge(ctx, number)
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
	return e.g.comment(ctx, number, body)
}

func (e ghEffects) Markers(ctx context.Context, number int) ([]queuemarker.Marker, error) {
	return e.g.markers(ctx, number)
}

func (e ghEffects) Why(ctx context.Context, number int) (string, error) {
	var out bytes.Buffer
	err := why(ctx, e.g, number, &out)
	return out.String(), err
}

func (e ghEffects) Rebase(ctx context.Context, branch, head string) (rebaseResult, error) {
	return rebaser{local: e.local, remote: e.g.remoteURL(), auth: httpsCredential, env: os.Environ()}.rebase(ctx, branch, head, e.base)
}

// rebaser rebases a pull request branch in a throwaway repository, so the
// operator's checkout, branches and index are never touched.
type rebaser struct {
	local  string // repository whose objects are borrowed to avoid refetching
	remote string
	auth   []string
	env    []string // the environment to derive git's from
}

// gitEnvKeep names the variables git and the GitHub CLI credential helper
// need from the operator's environment. Everything else, notably every GIT_*
// variable, stays out of the throwaway repository's commands.
var (
	gitEnvKeep     = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR", "DBUS_SESSION_BUS_ADDRESS", "NIX_SSL_CERT_FILE", "CURL_CA_BUNDLE", "ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"}
	gitEnvKeepPref = []string{"LC_", "XDG_", "GH_", "GITHUB_", "SSL_"}
)

func gitEnvFrom(environ []string) []string {
	var env []string
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if slices.Contains(gitEnvKeep, name) || slices.ContainsFunc(gitEnvKeepPref, func(p string) bool { return strings.HasPrefix(name, p) }) {
			env = append(env, entry)
		}
	}
	return env
}

// rebaseEnv is the environment of the rebase commands: only what they need
// from environ, no configuration beyond the repository's own, and the
// committer identity.
func rebaseEnv(environ []string, name, email string) []string {
	return append(gitEnvFrom(environ),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=gc.auto", "GIT_CONFIG_VALUE_0=0", "GIT_CONFIG_KEY_1=maintenance.auto", "GIT_CONFIG_VALUE_1=false",
		"GIT_COMMITTER_NAME="+name,
		"GIT_COMMITTER_EMAIL="+email,
	)
}

func (r rebaser) rebase(ctx context.Context, branch, head, base string) (rebaseResult, error) {
	dir, err := os.MkdirTemp("", "katl-queue-rebase-")
	if err != nil {
		return rebaseResult{}, err
	}
	defer os.RemoveAll(dir)

	// The identity comes from the operator's own git configuration, which
	// the rebase commands themselves must not read.
	hostEnv := gitEnvFrom(r.env)
	identity := func(key, fallback string) string {
		if value := strings.TrimSpace(gitOutput(ctx, r.local, hostEnv, "config", "user."+key)); value != "" {
			return value
		}
		return fallback
	}
	env := rebaseEnv(r.env, identity("name", "katl-queue"), identity("email", "katl-queue@invalid"))
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
		return rebaseResult{}, err
	}
	if objects := strings.TrimSpace(gitOutput(ctx, r.local, hostEnv, "rev-parse", "--path-format=absolute", "--git-path", "objects")); objects != "" {
		if err := os.WriteFile(filepath.Join(dir, ".git", "objects", "info", "alternates"), []byte(objects+"\n"), 0o644); err != nil {
			return rebaseResult{}, err
		}
	}
	if _, err := git(append(slices.Clone(r.auth), "fetch", "--quiet", "--no-tags", r.remote, head, base)...); err != nil {
		return rebaseResult{}, err
	}
	if _, err := git("checkout", "--quiet", "--detach", head); err != nil {
		return rebaseResult{}, err
	}
	if _, err := git("rebase", base); err != nil {
		unmerged, _ := git("diff", "--name-only", "--diff-filter=U")
		git("rebase", "--abort")
		if conflicts := strings.Fields(unmerged); len(conflicts) > 0 {
			return rebaseResult{Conflicts: conflicts}, nil
		}
		return rebaseResult{}, err
	}
	rebased, err := git("rev-parse", "HEAD")
	if err != nil {
		return rebaseResult{}, err
	}

	// The lease on the head the decision was made from keeps a push the
	// author made meanwhile from being overwritten.
	ref := "refs/heads/" + branch
	_, err = git(append(slices.Clone(r.auth), "push", "--quiet", "--force-with-lease="+ref+":"+head, r.remote, "HEAD:"+ref)...)
	return rebaseResult{Head: strings.TrimSpace(rebased)}, err
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
	return apply(ctx, ghEffects{g: g, base: w.Base, local: ".", out: out}, d.Actions, out)
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
