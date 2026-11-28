// Package queueadmission decides whether a pull request may enter the merge
// queue. Evaluate is a pure function of a facts snapshot: it reads no clock
// and performs no I/O, so every caller that loads the same facts reaches the
// same verdict.
package queueadmission

import "github.com/katl-dev/katl/internal/reviewrecord"

// Known is a fact that was either read or not. Its value has no exported
// accessor, so only this package decides from facts, and a fact that was not
// read cannot be mistaken for one read as empty.
type Known[T any] struct {
	value T
	known bool
}

// Of returns a fact that was read with value v. The zero Known is absent.
func Of[T any](v T) Known[T] {
	return Known[T]{value: v, known: true}
}

func (k Known[T]) get() (T, bool) {
	return k.value, k.known
}

// Facts is the snapshot Evaluate decides from.
type Facts struct {
	Target Target
	// Pulls holds every open pull request in the repository.
	Pulls map[int]Pull
	// Refs holds the dependencies that are not open pull requests.
	Refs Known[map[int]Ref]
}

// Target is the branch pull requests merge into and its merge queue.
type Target struct {
	Branch string
	// Queue lists the merge-queue entries in order.
	Queue Known[[]Entry]
}

// Entry is a merge-queue entry and the issues it closes.
type Entry struct {
	Number int
	Closes []int
}

// Ref is the state of an issue or pull request: OPEN, CLOSED, MERGED or
// NOT_FOUND.
type Ref struct {
	Pull  bool
	State string
}

// Resolved reports whether the reference no longer blocks what depends on it:
// a merged pull request or any closed issue.
func (r Ref) Resolved() bool {
	return r.State == "MERGED" || r.State == "CLOSED" && !r.Pull
}

// Pull is an open pull request.
type Pull struct {
	Number  int
	Head    string
	BaseRef string
	HeadRef string

	Author Known[Author]
	Draft  Known[bool]
	Body   Known[string]
	Labels Known[[]string]
	Closes Known[[]int]
	// BlockedBy holds the blocked-by links on the pull request and on the
	// issues it closes.
	BlockedBy Known[Blockers]
	Paths     Known[ChangedPaths]
	Review    Known[Review]
	// ReviewDecision is GitHub's: "", APPROVED, CHANGES_REQUESTED or
	// REVIEW_REQUIRED.
	ReviewDecision Known[string]
	ReleaseNotes   Known[ReleaseNotes]
	Conflicts      Known[Conflicts]
	// Removal is the latest removal from the merge queue; nil when it was
	// never removed.
	Removal   Known[*Removal]
	AutoMerge Known[bool]
	// Mergeable is MERGEABLE, CONFLICTING or UNKNOWN.
	Mergeable Known[string]
	// Threads counts the unresolved review threads.
	Threads Known[int]
	// Checks holds the latest context of each check name.
	Checks Known[[]Check]
}

type Author struct {
	Login string
	Bot   bool
}

// Blockers are blocked-by links: numbers in this repository, and links to
// other repositories as owner/name#N.
type Blockers struct {
	Local    []int
	External []string
}

// ChangedPaths lists the changed files. Complete is false when the list was
// truncated or could not be read in full.
type ChangedPaths struct {
	Files    []string
	Complete bool
}

// Review is the selected review record for the current head.
type Review struct {
	Match   reviewrecord.Match
	Verdict reviewrecord.Verdict
}

// ReleaseNotes describes the Release-Note: trailers of the squash message.
type ReleaseNotes struct {
	Count     int
	MultiLine bool
}

// Conflicts are the results of simulating the pull request over the base
// branch, the queue and the other pull requests with auto-merge pending.
type Conflicts struct {
	// Main lists the paths that conflict with the base branch.
	Main []string
	// Queued is the conflict with a queue entry, if any.
	Queued  *Conflict
	Pending []Conflict
}

type Conflict struct {
	PR    int
	Paths []string
}

// Check is the latest run of a check name: failed, cancelled, running or
// passed.
type Check struct {
	Name     string
	State    string
	Required bool
}

// Removal is the latest removal of a pull request from the merge queue.
type Removal struct {
	// Reason is GitHub's removal reason.
	Reason string
	// FailedChecks names the failed checks on the merge-group commit.
	FailedChecks []string
	// HeadAtRemoval is the head katl-queue's removal marker recorded, or
	// empty when none was recorded.
	HeadAtRemoval string
	// PushedAfter is set when GitHub records a push after the removal.
	PushedAfter bool
	// LinkedAfter is set when a blocked-by link was added after the removal.
	LinkedAfter bool
}

// InForce reports whether the removal still holds head. A recorded head
// decides; without a record only a push after the removal shows that someone
// acted on it.
func (r Removal) InForce(head string) bool {
	if r.HeadAtRemoval != "" {
		return r.HeadAtRemoval == head
	}
	return !r.PushedAfter
}

// ForFailedChecks reports whether the queue removed the entry because its
// checks failed or timed out.
func (r Removal) ForFailedChecks() bool {
	return r.Reason == "failed_checks" || r.Reason == "checks_timed_out"
}
