package devclean

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// PullRequest is a pull request that GitHub associates with a commit.
type PullRequest struct {
	Number int
	// Merged reports whether the pull request was merged.
	Merged bool
	// HeadSHA is the last commit of the pull request's head branch.
	HeadSHA string
}

// PullRequestLookup reports the pull requests GitHub associates with a commit.
// An error means GitHub could not be asked, never that there are none.
type PullRequestLookup interface {
	PullRequests(ctx context.Context, commit string) ([]PullRequest, error)
}

// mergedHead returns the merged pull request whose head commit is exactly
// commit. A squash merge leaves a pull request's own commits on no remote ref,
// so this is the evidence that they were landed.
func mergedHead(ctx context.Context, lookup PullRequestLookup, commit string) (PullRequest, bool, error) {
	prs, err := lookup.PullRequests(ctx, commit)
	if err != nil {
		return PullRequest{}, false, err
	}
	for _, pr := range prs {
		if pr.Merged && pr.HeadSHA == commit {
			return pr, true, nil
		}
	}
	return PullRequest{}, false, nil
}

// GitHubLookup asks GitHub through the gh CLI about the repository that the
// origin remote of Repo points at.
type GitHubLookup struct {
	// Environ is the environment git and gh run in.
	Environ []string
	// Repo is a path inside the repository whose origin is queried.
	Repo string
}

// PullRequests implements PullRequestLookup.
func (l GitHubLookup) PullRequests(ctx context.Context, commit string) ([]PullRequest, error) {
	g := gitRunner{environ: l.Environ}
	url, err := g.run(ctx, l.Repo, "remote", "get-url", "origin")
	if err != nil {
		return nil, err
	}
	slug, err := githubSlug(strings.TrimSpace(url))
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "repos/"+slug+"/commits/"+commit+"/pulls")
	cmd.Env = l.Environ
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("gh api: %w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("gh api: %w", err)
	}

	var raw []struct {
		Number   int        `json:"number"`
		MergedAt *time.Time `json:"merged_at"`
		Head     struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh api: unexpected response: %w", err)
	}
	prs := make([]PullRequest, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, PullRequest{Number: r.Number, Merged: r.MergedAt != nil, HeadSHA: r.Head.SHA})
	}
	return prs, nil
}

var githubRemote = regexp.MustCompile(`^(?:git@github\.com:|(?:https|ssh)://(?:[^@/]+@)?github\.com/)([^/]+/[^/]+?)(?:\.git)?/?$`)

// githubSlug returns owner/repo for a GitHub remote URL.
func githubSlug(url string) (string, error) {
	m := githubRemote.FindStringSubmatch(url)
	if m == nil {
		return "", fmt.Errorf("origin %q is not a GitHub remote", url)
	}
	return m[1], nil
}
