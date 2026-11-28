package queueadmission

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CheckName is the required check that reports the verdict.
const CheckName = "queue-admission"

// Code identifies a reason. Codes are stable once shipped.
type Code string

// Actor is who must act to clear a reason.
type Actor string

const (
	ActorNone       Actor = "none"
	ActorOwner      Actor = "owner"
	ActorReviewer   Actor = "reviewer"
	ActorMaintainer Actor = "maintainer"
)

// Gate is what a reason gates: admission, which the check enforces; github,
// which GitHub enforces itself; or none.
type Gate string

const (
	GateAdmission Gate = "admission"
	GateGitHub    Gate = "github"
	GateNone      Gate = "none"
)

const (
	codeIncomplete        Code = "incomplete"
	codeDraft             Code = "draft"
	codeHold              Code = "hold"
	codeDependencyOpen    Code = "dependency-open"
	codeDependencyBroken  Code = "dependency-broken"
	codeReviewMissing     Code = "review-missing"
	codeReviewStale       Code = "review-stale"
	codeChangesRequested  Code = "changes-requested"
	codeConflictMain      Code = "conflict-main"
	codeConflictQueued    Code = "conflict-queued"
	codeConflictPending   Code = "conflict-pending"
	codeMergeAlone        Code = "merge-alone"
	codeMetadata          Code = "metadata"
	codeReleaseNote       Code = "release-note"
	codeEjected           Code = "ejected"
	codeCIFailed          Code = "ci-failed"
	codeThreadsUnresolved Code = "threads-unresolved"
	codeUnmergeable       Code = "unmergeable"
	codeAutoMergeOff      Code = "auto-merge-off"
	codeCIOptionalFailed  Code = "ci-optional-failed"
	codeCIRunning         Code = "ci-running"
	codeQueued            Code = "queued"
)

// tag is how the renderer labels a reason that gates nothing.
type tag string

const (
	tagNone     tag = ""
	tagAdvisory tag = "advisory"
	tagProgress tag = "progress"
)

type codeRule struct {
	code     Code
	gates    Gate
	actor    Actor
	clearsBy string
	tag      tag
}

// codeRules fixes each code's gate, actor and next action. Its order is the
// order reasons are reported in.
var codeRules = []codeRule{
	{codeIncomplete, GateAdmission, ActorNone, "the next evaluation reads the missing fact", tagNone},
	{codeDraft, GateAdmission, ActorOwner, "mark the PR ready", tagNone},
	{codeHold, GateAdmission, ActorMaintainer, "meet the named precondition and remove the `Hold:` line", tagNone},
	{codeDependencyOpen, GateAdmission, ActorNone, "the blocking PR merges or the blocking issue closes", tagNone},
	{codeDependencyBroken, GateAdmission, ActorOwner, "fix the link: the dependency closed unmerged, is missing, cyclic or in another repository", tagNone},
	{codeReviewMissing, GateAdmission, ActorReviewer, "post an `approved` review record for the current head", tagNone},
	{codeReviewStale, GateAdmission, ActorReviewer, "post an `approved` review record for the current head", tagNone},
	{codeChangesRequested, GateAdmission, ActorOwner, "push a fix", tagNone},
	{codeConflictMain, GateAdmission, ActorOwner, "rebase onto the base branch", tagNone},
	{codeConflictQueued, GateAdmission, ActorNone, "the queued PR merges or leaves the queue", tagNone},
	{codeConflictPending, GateNone, ActorNone, "advisory: the other PR is not queued yet", tagAdvisory},
	{codeMergeAlone, GateAdmission, ActorNone, "the queue drains", tagNone},
	{codeMetadata, GateAdmission, ActorOwner, "add exactly one kind label and at least one area label", tagNone},
	{codeReleaseNote, GateAdmission, ActorOwner, "keep at most one single-line `Release-Note:`", tagNone},
	{codeEjected, GateAdmission, ActorOwner, "push a new head, or link the cause as blocked-by", tagNone},
	{codeCIFailed, GateGitHub, ActorOwner, "fix the failing checks and push, or re-run a cancelled check", tagNone},
	{codeThreadsUnresolved, GateGitHub, ActorOwner, "resolve the review threads", tagNone},
	{codeUnmergeable, GateGitHub, ActorOwner, "rebase onto the base branch", tagNone},
	{codeAutoMergeOff, GateGitHub, ActorOwner, "run `katl-queue enqueue PR`", tagNone},
	{codeCIOptionalFailed, GateNone, ActorNone, "informational: not required to merge", tagAdvisory},
	{codeCIRunning, GateNone, ActorNone, "checks finish", tagProgress},
	{codeQueued, GateNone, ActorNone, "the merge group passes", tagProgress},
}

func ruleFor(code Code) (int, codeRule) {
	for i, r := range codeRules {
		if r.code == code {
			return i, r
		}
	}
	panic("queueadmission: unknown code " + string(code))
}

// Reason is one thing that stands between a pull request and a merge, or
// reports its progress.
type Reason struct {
	Code  Code  `json:"code"`
	Gates Gate  `json:"gates"`
	Actor Actor `json:"actor"`
	// Detail is one line naming the pull requests, issues and paths
	// involved.
	Detail string `json:"detail"`
	// ClearsBy is one line naming the next action.
	ClearsBy string   `json:"clearsBy"`
	PRs      []int    `json:"prs,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	// Facts names the facts that could not be read; incomplete only.
	Facts []string `json:"facts,omitempty"`
}

// Verdict is every reason that applies to a pull request's head, in table
// order.
type Verdict struct {
	PR      int
	Head    string
	Reasons []Reason
}

// Admitted reports whether nothing gates admission.
func (v Verdict) Admitted() bool {
	for _, r := range v.Reasons {
		if r.Gates == GateAdmission {
			return false
		}
	}
	return true
}

// Enqueueable reports whether enabling auto-merge is all that stands between
// the pull request and the queue.
func (v Verdict) Enqueueable() bool {
	for _, r := range v.Reasons {
		if r.Gates != GateNone && r.Code != codeAutoMergeOff {
			return false
		}
	}
	return true
}

// Actor is the first of owner, reviewer and maintainer that any reason
// names, or none.
func (v Verdict) Actor() Actor {
	for _, actor := range []Actor{ActorOwner, ActorReviewer, ActorMaintainer} {
		for _, r := range v.Reasons {
			if r.Actor == actor {
				return actor
			}
		}
	}
	return ActorNone
}

func (v Verdict) MarshalJSON() ([]byte, error) {
	reasons := v.Reasons
	if reasons == nil {
		reasons = []Reason{}
	}
	return json.Marshal(struct {
		PR          int      `json:"pr"`
		Head        string   `json:"head"`
		Admitted    bool     `json:"admitted"`
		Enqueueable bool     `json:"enqueueable"`
		Actor       Actor    `json:"actor"`
		Reasons     []Reason `json:"reasons"`
	}{v.PR, v.Head, v.Admitted(), v.Enqueueable(), v.Actor(), reasons})
}

const shownPaths = 10

// Text renders the verdict for a pull request into branch.
func (v Verdict) Text(branch string) string {
	var b strings.Builder
	head := v.Head
	if len(head) > 7 {
		head = head[:7]
	}
	state := "admitted"
	actor := v.Actor()
	switch {
	case !v.Admitted() && actor == ActorNone:
		state = "gated, waiting"
	case !v.Admitted():
		state = fmt.Sprintf("gated, %s must act", actor)
	case actor != ActorNone:
		state = fmt.Sprintf("admitted, %s must act", actor)
	}
	fmt.Fprintf(&b, "#%d (%s) into %s: %s\n", v.PR, head, branch, state)

	for _, r := range v.Reasons {
		_, rule := ruleFor(r.Code)
		label := fmt.Sprintf("[gates, %s]", r.Actor)
		switch {
		case rule.tag != tagNone:
			label = "[" + string(rule.tag) + "]"
		case r.Gates == GateGitHub:
			label = fmt.Sprintf("[github, %s]", r.Actor)
		}
		fmt.Fprintf(&b, "  %s  %s  %s\n", r.Code, label, r.Detail)
		if rule.tag != tagProgress {
			fmt.Fprintf(&b, "      clears by: %s\n", r.ClearsBy)
		}
		for i, p := range r.Paths {
			if i == shownPaths {
				fmt.Fprintf(&b, "      ... and %d more\n", len(r.Paths)-shownPaths)
				break
			}
			fmt.Fprintf(&b, "      %s\n", p)
		}
	}
	return b.String()
}
