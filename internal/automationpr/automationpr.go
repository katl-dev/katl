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
