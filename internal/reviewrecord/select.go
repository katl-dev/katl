package reviewrecord

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/queuemarker"
)

// ParseComment reads the record from the final nonblank line. Review prose can
// include example markers without changing the record.
func ParseComment(body string) (Record, bool) {
	body = strings.TrimSpace(body)
	lastLine := body[strings.LastIndexByte(body, '\n')+1:]
	markers := queuemarker.Parse(lastLine)
	if len(markers) != 1 || markers[0].Kind != reviewMarker {
		return Record{}, false
	}
	parts := strings.Split(markers[0].Value, ":")
	if len(parts) != 3 {
		return Record{}, false
	}
	r := Record{Verdict: Verdict(parts[0]), Head: parts[1], Base: parts[2]}
	if r.validate() != nil {
		return Record{}, false
	}
	r.normalizeSHAs()
	return r, true
}

// Comment holds the facts needed to read a review record. ID orders comments
// by creation, and Member includes repository owners.
type Comment struct {
	ID     int64
	Author string
	Member bool
	Body   string
}

type Match int

const (
	Missing Match = iota
	Stale
	Direct
	EquivalentPatch
)

// PatchIdentity returns the identity of the PR diff for base and head. An
// empty identity cannot establish patch equivalence.
type PatchIdentity func(base, head string) (string, error)

type postedReview struct {
	record  Record
	comment Comment
}

func newestFirst(comments []Comment, eligible func(Comment) bool) []postedReview {
	var reviews []postedReview
	for _, c := range comments {
		if !eligible(c) {
			continue
		}
		if record, ok := ParseComment(c.Body); ok {
			reviews = append(reviews, postedReview{record, c})
		}
	}
	slices.SortStableFunc(reviews, func(a, b postedReview) int {
		return cmp.Compare(b.comment.ID, a.comment.ID)
	})
	return reviews
}

// AlreadyPosted reports whether body is the newest recorded review for head.
// A caller's own comments count for repeated posts even if the caller is
// outside the repository. Only member comments count for selection.
func AlreadyPosted(comments []Comment, viewer, head, body string) bool {
	for _, r := range newestFirst(comments, func(c Comment) bool {
		return c.Member || (viewer != "" && c.Author == viewer)
	}) {
		if r.record.Head == head {
			return strings.TrimSpace(r.comment.Body) == body
		}
	}
	return false
}

// Select returns the newest applicable member review. If reviews exist but
// none apply, it returns the newest review with Stale. A nil patch function
// matches the head only. Failure to compare a possibly newer review returns
// an error, since that review could supersede an older one.
func Select(comments []Comment, head, base string, patchID PatchIdentity) (Record, Match, error) {
	reviews := newestFirst(comments, func(c Comment) bool { return c.Member })
	if len(reviews) == 0 {
		return Record{}, Missing, nil
	}

	var currentPatchID string
	for i, r := range reviews {
		if r.record.Head == head {
			return r.record, Direct, nil
		}
		if patchID == nil {
			continue
		}
		if i == 0 {
			var err error
			currentPatchID, err = patchID(base, head)
			if err != nil {
				return Record{}, Missing, fmt.Errorf("compare patch for current head %s: %w", head, err)
			}
		}
		if currentPatchID == "" {
			continue
		}
		reviewedPatchID, err := patchID(r.record.Base, r.record.Head)
		if err != nil {
			return Record{}, Missing, fmt.Errorf("compare patch for reviewed head %s: %w", r.record.Head, err)
		}
		if reviewedPatchID == currentPatchID {
			return r.record, EquivalentPatch, nil
		}
	}
	return reviews[0].record, Stale, nil
}
