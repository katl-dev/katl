package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type intakeIssue struct {
	issueText
	Number    int
	Parent    int
	Closed    bool
	Locked    bool
	Labels    []string
	Milestone string
}

func (i intakeIssue) bug() bool {
	return !i.audit() && (slices.Contains(i.Labels, "bug") || slices.Contains(i.Labels, "flaky-test"))
}

type issueRecord struct {
	Number    int
	Title     string
	Body      string
	State     string
	Locked    bool
	ParentURL string          `json:"parent_issue_url"`
	Pull      json.RawMessage `json:"pull_request"`
	Labels    []struct{ Name string }
	Milestone *struct{ Title string }
}

func (g *gh) intakeIssues(ctx context.Context) ([]intakeIssue, error) {
	raw, err := g.read(ctx, "--paginate", "--slurp", g.repoPath()+"/issues?state=all&per_page=100")
	if err != nil {
		return nil, err
	}
	var pages [][]issueRecord
	if err := decodeJSON(raw, &pages); err != nil {
		return nil, err
	}

	var issues []intakeIssue
	for _, page := range pages {
		for _, record := range page {
			if len(record.Pull) != 0 && string(record.Pull) != "null" {
				continue
			}
			i := intakeIssue{
				issueText: issueText{record.Title, record.Body}, Number: record.Number,
				Closed: record.State == "closed", Locked: record.Locked,
			}
			if _, parent, ok := strings.Cut(record.ParentURL, "/"+g.repoPath()+"/issues/"); ok {
				i.Parent, _ = strconv.Atoi(parent)
			}
			for _, label := range record.Labels {
				i.Labels = append(i.Labels, label.Name)
			}
			if record.Milestone != nil {
				i.Milestone = record.Milestone.Title
			}
			issues = append(issues, i)
		}
	}
	return issues, nil
}

func (g *gh) issueLabels(ctx context.Context) ([]string, error) {
	raw, err := g.read(ctx, "--paginate", "--slurp", g.repoPath()+"/labels?per_page=100")
	if err != nil {
		return nil, err
	}
	var pages [][]struct{ Name string }
	if err := decodeJSON(raw, &pages); err != nil {
		return nil, err
	}
	var labels []string
	for _, page := range pages {
		for _, label := range page {
			labels = append(labels, label.Name)
		}
	}
	return labels, nil
}

func (g *gh) addRecurrenceLabel(ctx context.Context) error {
	_, err := runGH(ctx, "label", "create", recurrenceLabel, "--repo", g.repo(),
		"--description", "Recurrence of an earlier failure class", "--color", "D4C5F9")
	if err != nil {
		// Another filer may have created the label since we read the catalog.
		if labels, readErr := g.issueLabels(ctx); readErr == nil && slices.Contains(labels, recurrenceLabel) {
			return nil
		}
	}
	return err
}

type issueDraft struct {
	issueText
	Kind, Area, Priority, Milestone string
	Recurrence                      bool
}

func (d issueDraft) labels() []string {
	labels := []string{d.Kind, d.Area, d.Priority}
	if d.Recurrence {
		labels = append(labels, recurrenceLabel)
	}
	return labels
}

func (g *gh) createIntakeIssue(ctx context.Context, draft issueDraft) (intakeIssue, error) {
	// Issue creation is not safe to retry after a lost response.
	args := []string{"issue", "create", "--repo", g.repo(), "--title", draft.Title, "--body-file", "-"}
	for _, label := range draft.labels() {
		args = append(args, "--label", label)
	}
	if draft.Milestone != "" {
		args = append(args, "--milestone", draft.Milestone)
	}
	raw, err := runGHWithInput(ctx, draft.Body, args...)
	if err != nil {
		return intakeIssue{}, err
	}
	issueURL := strings.TrimSpace(string(raw))
	_, number, ok := strings.Cut(issueURL, "/issues/")
	n, err := strconv.Atoi(number)
	if !ok || err != nil || n <= 0 {
		return intakeIssue{}, fmt.Errorf("could not identify the created issue from %q; check GitHub before retrying", issueURL)
	}
	return intakeIssue{Number: n}, nil
}

func (g *gh) addSubIssue(ctx context.Context, parent, child int) error {
	// GitHub can answer 404 for an issue it has just created.
	var raw []byte
	err := g.awaitReadback(ctx, func(ctx context.Context) (bool, error) {
		var err error
		raw, err = g.read(ctx, fmt.Sprintf("%s/issues/%d", g.repoPath(), child), "--jq", ".id")
		return err == nil, err
	}, notFoundGH)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("could not read the database ID of issue #%d", child)
	}
	_, err = g.api(ctx, "--method", "POST", fmt.Sprintf("%s/issues/%d/sub_issues", g.repoPath(), parent),
		"-F", "sub_issue_id="+strconv.FormatInt(id, 10))
	return err
}
