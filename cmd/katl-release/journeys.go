package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/releaseversion"
)

const (
	userFacingLabel     = "user-facing"
	journeyPassedLabel  = "journey: passed"
	journeyGapLabel     = "journey: gap"
	releaseBlockerLabel = "release-blocker"
)

func journeys(ctx context.Context, args []string, stdout io.Writer, r *reader) error {
	if len(args) != 1 {
		return errors.New("usage: scripts/katl-release journeys VERSION")
	}
	version, err := validateVersion(args[0])
	if err != nil || !version.IsFinal() {
		return fmt.Errorf("VERSION must be a final KatlOS version such as 2026.12.1, got %q", args[0])
	}

	// Bind the branch head first and read the pin there, so the range and
	// every comparison use the same commits.
	branch := releasestate.Branch(version)
	head, err := r.refCommit(ctx, releasestate.Ref(version))
	if err != nil {
		return err
	}
	if head == "" {
		return fmt.Errorf("GitHub has no %s branch; run the journey gate after the branch cut", branch)
	}
	previous, err := r.upgradeBaseline(ctx, branch, head)
	if err != nil {
		return err
	}
	if compareFinal(previous, version) >= 0 {
		return fmt.Errorf("%s on %s names %s, which is not a release before %s. %s", upgradeBaselineFile, branch, previous.Tag(), version, pinRecovery(branch))
	}
	base, err := r.refCommit(ctx, "refs/tags/"+previous.Tag())
	if err != nil {
		return err
	}
	if base == "" {
		return fmt.Errorf("GitHub has no %s tag, which %s on %s names. %s", previous.Tag(), upgradeBaselineFile, branch, pinRecovery(branch))
	}

	prs, err := r.outstandingJourneyPRs(ctx, head, base)
	if err != nil {
		return err
	}
	blockers, err := r.journeyIssues(ctx, "open", releaseBlockerLabel, journeyGapLabel)
	if err != nil {
		return err
	}

	if len(prs) == 0 && len(blockers) == 0 {
		fmt.Fprintf(stdout, "Journey gate clear for %s since %s.\n", branch, previous.Tag())
		return nil
	}
	fmt.Fprintf(stdout, "Outstanding journeys for %s since %s\n", branch, previous.Tag())
	for _, pr := range prs {
		fmt.Fprintf(stdout, "  PR #%d %s (%s)\n", pr.Number, pr.Title, pr.URL)
	}
	for _, issue := range blockers {
		fmt.Fprintf(stdout, "  Gap fix #%d %s (%s)\n", issue.Number, issue.Title, issue.URL)
	}
	return errors.New("resolve the listed journeys and gap fixes before cutting a release candidate")
}

// upgradeBaseline reads the pinned previous final release at commit of branch.
func (r *reader) upgradeBaseline(ctx context.Context, branch, commit string) (releaseversion.Version, error) {
	body, err := r.get(ctx, r.repo("contents/"+upgradeBaselineFile+"?ref="+commit), false, "application/vnd.github.raw")
	if notFound(err) {
		return releaseversion.Version{}, fmt.Errorf("%s is missing on %s. %s", upgradeBaselineFile, branch, pinRecovery(branch))
	}
	if err != nil {
		return releaseversion.Version{}, err
	}
	previous, err := parseUpgradeBaseline(body)
	if err != nil {
		return releaseversion.Version{}, fmt.Errorf("%s: %w. %s", branch, err, pinRecovery(branch))
	}
	return previous, nil
}

// pinRecovery tells the release manager how to correct the pin the gate read.
func pinRecovery(branch string) string {
	return "Update the pin on " + branch + " through a pull request"
}

// GitHub's issues listing also includes PRs. Its merge timestamp identifies
// merged PRs, while the PR detail supplies the commit actually merged.
type journeyIssue struct {
	Number      int
	Title       string
	URL         string `json:"html_url"`
	Labels      []struct{ Name string }
	PullRequest *struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

func (r *reader) journeyIssues(ctx context.Context, state string, labels ...string) ([]journeyIssue, error) {
	query := url.Values{"state": {state}, "labels": {strings.Join(labels, ",")}, "per_page": {"100"}}
	var issues []journeyIssue
	err := pages(ctx, r, r.repo("issues?"+query.Encode()), func(page []journeyIssue) {
		issues = append(issues, page...)
	})
	return issues, err
}

func (r *reader) outstandingJourneyPRs(ctx context.Context, head, base string) ([]journeyIssue, error) {
	issues, err := r.journeyIssues(ctx, "closed", userFacingLabel)
	if err != nil {
		return nil, err
	}
	var outstanding []journeyIssue
	for _, pr := range issues {
		if pr.PullRequest == nil || pr.PullRequest.MergedAt == nil ||
			slices.ContainsFunc(pr.Labels, func(label struct{ Name string }) bool { return label.Name == journeyPassedLabel }) {
			continue
		}
		var details struct {
			MergeCommit string `json:"merge_commit_sha"`
		}
		if err := r.decode(ctx, r.repo(fmt.Sprintf("pulls/%d", pr.Number)), &details); err != nil {
			return nil, err
		}
		if !fullSHA.MatchString(details.MergeCommit) {
			return nil, fmt.Errorf("merged PR #%d has no full merge commit SHA", pr.Number)
		}
		onBranch, err := r.isAncestor(ctx, details.MergeCommit, head)
		if err != nil {
			return nil, err
		}
		if !onBranch {
			continue
		}
		alreadyShipped, err := r.isAncestor(ctx, details.MergeCommit, base)
		if err != nil {
			return nil, err
		}
		if !alreadyShipped {
			outstanding = append(outstanding, pr)
		}
	}
	return outstanding, nil
}
