// Package automationpr publishes the branches behind automation pull
// requests, such as kernel lock and builder pin updates.
package automationpr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Proposal is one file written on top of a base commit.
type Proposal struct {
	Repository string
	Base       string
	Branch     string
	Message    string
	Path       string
	Content    []byte
	// Matches returns an error when an existing branch carries different
	// content at Path, so that branch is preserved rather than reused.
	Matches func(existing []byte) error
}

// Publish creates Branch with one commit on Base that writes Path, or, when
// Branch already exists, advances it by merging Base and checks its content.
// It never force-updates or recreates a branch, because reviewers and CI may
// be looking at it. It returns the branch head to qualify.
func Publish(ctx context.Context, p Proposal) (string, error) {
	api := "repos/" + p.Repository
	var existing struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	_, err := gh(ctx, "GET", api+"/git/ref/heads/"+p.Branch, nil, &existing)
	if err == nil {
		if _, err := gh(ctx, "POST", api+"/merges", map[string]string{"base": p.Branch, "head": p.Base}, nil); err != nil {
			return "", err
		}
		if _, err := gh(ctx, "GET", api+"/git/ref/heads/"+p.Branch, nil, &existing); err != nil {
			return "", err
		}
		var file struct {
			Content string `json:"content"`
		}
		if _, err := gh(ctx, "GET", api+"/contents/"+p.Path+"?ref="+existing.Object.SHA, nil, &file); err != nil {
			return "", err
		}
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
		if err != nil {
			return "", err
		}
		if err := p.Matches(data); err != nil {
			return "", err
		}
		return existing.Object.SHA, nil
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		return "", err
	}

	var parent struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if _, err := gh(ctx, "GET", api+"/git/commits/"+p.Base, nil, &parent); err != nil {
		return "", err
	}
	entries := []map[string]string{{"path": p.Path, "mode": "100644", "type": "blob", "content": string(p.Content)}}
	var tree struct {
		SHA string `json:"sha"`
	}
	if _, err := gh(ctx, "POST", api+"/git/trees", map[string]any{"base_tree": parent.Tree.SHA, "tree": entries}, &tree); err != nil {
		return "", err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if _, err := gh(ctx, "POST", api+"/git/commits", map[string]any{"message": p.Message, "tree": tree.SHA, "parents": []string{p.Base}}, &commit); err != nil {
		return "", err
	}
	if _, err := gh(ctx, "POST", api+"/git/refs", map[string]string{"ref": "refs/heads/" + p.Branch, "sha": commit.SHA}, nil); err != nil {
		return "", err
	}
	return commit.SHA, nil
}

func gh(ctx context.Context, method, endpoint string, input, output any) ([]byte, error) {
	args := []string{"api", "--method", method, endpoint}
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			return nil, err
		}
		args = append(args, "--input", "-")
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = bytes.NewReader(body)
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("GitHub %s %s: %w: %s", method, endpoint, err, diagnostics.String())
	}
	if output != nil && len(bytes.TrimSpace(data)) != 0 {
		if err := json.Unmarshal(data, output); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// pageSize is the largest page GitHub's list endpoints return.
const pageSize = 100

// list fetches every page of a GitHub list endpoint.
func list[T any](ctx context.Context, endpoint string) ([]T, error) {
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	var all []T
	for page := 1; ; page++ {
		var items []T
		if _, err := gh(ctx, "GET", fmt.Sprintf("%s%sper_page=%d&page=%d", endpoint, separator, pageSize, page), nil, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < pageSize {
			return all, nil
		}
	}
}

type pull struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
	User   struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// Supersede keeps only the newest open proposal among the same-repository pull
// requests that author opened from branches named with prefix, and closes the
// rest. A proposal's age is the workflow run that last described it: its
// description must link to that run under repository's actions/runs. Run IDs
// grow with each new run and a rerun keeps its run's ID, so a rerun of an old
// run never closes a newer proposal; it closes its own instead. On a tie the
// proposal from branch wins, since this run just published it. Proposals
// without a run link cannot be ordered and stay open.
//
// Each superseded proposal gets one comment naming the survivor, loses its
// branch, and is closed, in that order: deleting the branch also closes the
// pull request, so a run that stops partway leaves the proposal open for the
// next run to finish. It returns the numbers it closed.
func Supersede(ctx context.Context, repository, prefix, branch, author string, run int64) ([]int, error) {
	api := "repos/" + repository
	pulls, err := list[pull](ctx, api+"/pulls?state=open")
	if err != nil {
		return nil, err
	}
	link := regexp.MustCompile(`/` + regexp.QuoteMeta(repository) + `/actions/runs/([0-9]+)\b`)
	type proposal struct {
		pull
		run int64
	}
	var current *proposal
	var others []proposal
	for _, p := range pulls {
		if p.Head.Repo.FullName != repository || !strings.HasPrefix(p.Head.Ref, prefix) || p.User.Login != author || p.User.Type != "Bot" {
			continue
		}
		m := link.FindStringSubmatch(p.Body)
		if m == nil {
			continue
		}
		id, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("pull request #%d links run %s: %w", p.Number, m[1], err)
		}
		if p.Head.Ref == branch {
			current = &proposal{p, id}
		} else {
			others = append(others, proposal{p, id})
		}
	}
	if current == nil {
		return nil, fmt.Errorf("no open pull request by %s from %s", author, branch)
	}
	if current.run != run {
		return nil, fmt.Errorf("pull request #%d describes run %d, not run %d", current.Number, current.run, run)
	}

	survivor := *current
	for _, p := range others {
		if p.run > survivor.run || (p.run == survivor.run && survivor.Head.Ref != branch && p.Number > survivor.Number) {
			survivor = p
		}
	}
	var closed []int
	for _, p := range append(others, *current) {
		if p.Number == survivor.Number {
			continue
		}
		if err := closeProposal(ctx, api, author, p.pull, fmt.Sprintf("Superseded by #%d.", survivor.Number)); err != nil {
			return closed, err
		}
		closed = append(closed, p.Number)
	}
	return closed, nil
}

func closeProposal(ctx context.Context, api, author string, p pull, comment string) error {
	comments, err := list[struct {
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}](ctx, fmt.Sprintf("%s/issues/%d/comments", api, p.Number))
	if err != nil {
		return err
	}
	commented := false
	for _, c := range comments {
		commented = commented || (c.User.Login == author && c.Body == comment)
	}
	if !commented {
		if _, err := gh(ctx, "POST", fmt.Sprintf("%s/issues/%d/comments", api, p.Number), map[string]string{"body": comment}, nil); err != nil {
			return err
		}
	}
	// A branch that is already gone answers 422 (or 404); either way the
	// proposal no longer has one.
	if _, err := gh(ctx, "DELETE", api+"/git/refs/heads/"+p.Head.Ref, nil, nil); err != nil &&
		!strings.Contains(err.Error(), "HTTP 422") && !strings.Contains(err.Error(), "HTTP 404") {
		return err
	}
	_, err = gh(ctx, "PATCH", fmt.Sprintf("%s/pulls/%d", api, p.Number), map[string]string{"state": "closed"}, nil)
	return err
}
