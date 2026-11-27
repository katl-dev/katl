package reviewrecord_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/katl-dev/katl/internal/reviewrecord"
)

func memberReview(id int64, verdict reviewrecord.Verdict, head, base string) reviewrecord.Comment {
	return reviewrecord.Comment{
		ID: id, Member: true,
		Body: fmt.Sprintf("<!-- katl-queue:review %s:%s:%s -->", verdict, head, base),
	}
}

func TestSelectNewestMemberReview(t *testing.T) {
	approved := memberReview(10, reviewrecord.Approved, currentHead, reviewedBase)
	requested := memberReview(11, reviewrecord.ChangesRequested, currentHead, reviewedBase)
	approvedAgain := memberReview(12, reviewrecord.Approved, currentHead, currentBase)
	otherHead := memberReview(13, reviewrecord.ChangesRequested, reviewedHead, reviewedBase)
	outsider := memberReview(14, reviewrecord.ChangesRequested, currentHead, currentBase)
	outsider.Member = false

	for _, test := range []struct {
		name     string
		comments []reviewrecord.Comment
		want     reviewrecord.Record
		match    reviewrecord.Match
	}{
		{"newer changes requested", []reviewrecord.Comment{requested, approved, otherHead},
			reviewrecord.Record{Verdict: reviewrecord.ChangesRequested, Head: currentHead, Base: reviewedBase}, reviewrecord.Direct},
		{"newer approval", []reviewrecord.Comment{approvedAgain, requested, outsider, otherHead},
			reviewrecord.Record{Verdict: reviewrecord.Approved, Head: currentHead, Base: currentBase}, reviewrecord.Direct},
		{"other head only", []reviewrecord.Comment{outsider, otherHead},
			reviewrecord.Record{Verdict: reviewrecord.ChangesRequested, Head: reviewedHead, Base: reviewedBase}, reviewrecord.Stale},
		{"nonmember only", []reviewrecord.Comment{outsider}, reviewrecord.Record{}, reviewrecord.Missing},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, match, err := reviewrecord.Select(test.comments, currentHead, currentBase, nil)
			if got != test.want || match != test.match || err != nil {
				t.Errorf("Select = %+v, %v, %v, want %+v, %v, nil", got, match, err, test.want, test.match)
			}
		})
	}
}

func TestSelectNewestApplicablePatch(t *testing.T) {
	approved := memberReview(1, reviewrecord.Approved, currentHead, currentBase)
	requested := memberReview(2, reviewrecord.ChangesRequested, reviewedHead, reviewedBase)
	patchID := func(base, head string) (string, error) {
		if base == currentBase && head == currentHead || base == reviewedBase && head == reviewedHead {
			return "matching-patch", nil
		}
		return "", fmt.Errorf("patch unavailable for %s to %s", base, head)
	}

	got, match, err := reviewrecord.Select([]reviewrecord.Comment{approved, requested}, currentHead, currentBase, patchID)
	if err != nil || match != reviewrecord.EquivalentPatch || got.Verdict != reviewrecord.ChangesRequested || got.Head != reviewedHead {
		t.Errorf("Select = %+v, %v, %v, want newer changes-requested on equivalent patch", got, match, err)
	}

	approved.ID = 3
	got, match, err = reviewrecord.Select([]reviewrecord.Comment{approved, requested}, currentHead, currentBase, func(_, _ string) (string, error) {
		t.Fatal("direct match compared a patch")
		return "", nil
	})
	if err != nil || match != reviewrecord.Direct || got.Verdict != reviewrecord.Approved {
		t.Errorf("Select = %+v, %v, %v, want newer direct approval", got, match, err)
	}
}

func TestSelectUnverifiablePatch(t *testing.T) {
	comments := []reviewrecord.Comment{memberReview(1, reviewrecord.Approved, reviewedHead, reviewedBase)}
	t.Run("empty diff", func(t *testing.T) {
		_, match, err := reviewrecord.Select(comments, currentHead, currentBase, func(_, _ string) (string, error) {
			return "", nil
		})
		if match != reviewrecord.Stale || err != nil {
			t.Errorf("Select match = %v, %v, want stale without a patch ID", match, err)
		}
	})
	t.Run("failed comparison", func(t *testing.T) {
		unavailable := errors.New("commit unavailable")
		_, _, err := reviewrecord.Select(comments, currentHead, currentBase, func(_, head string) (string, error) {
			if head == currentHead {
				return "current-patch", nil
			}
			return "", unavailable
		})
		if !errors.Is(err, unavailable) {
			t.Errorf("Select error = %v, want %v", err, unavailable)
		}
	})
}
