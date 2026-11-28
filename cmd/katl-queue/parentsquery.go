package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

type issueConnection[T any] struct {
	Nodes    []T
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
}

type issueStateEvent struct {
	Type      string `json:"__typename"`
	CreatedAt time.Time
}

type trackerIssue struct {
	Number           int
	Title            string
	URL              string
	Body             string
	State            string
	CreatedAt        time.Time
	Milestone        *struct{ Title string }
	SubIssuesSummary struct{ Total, Completed int }
	Timeline         issueConnection[issueStateEvent] `json:"timelineItems"`
}

type subIssue struct {
	CreatedAt time.Time
	Timeline  issueConnection[issueStateEvent] `json:"timelineItems"`
}

const issueStateEvents = `itemTypes: [CLOSED_EVENT, REOPENED_EVENT]`
const issueStateNodes = `nodes { __typename ... on ClosedEvent { createdAt } ... on ReopenedEvent { createdAt } }`
const issuePageInfo = `pageInfo { hasNextPage endCursor }`

func readIssuePages[T any](ctx context.Context, cursor string, load func(context.Context, string) (issueConnection[T], error)) ([]T, error) {
	var nodes []T
	for {
		page, err := load(ctx, cursor)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, page.Nodes...)
		if !page.PageInfo.HasNextPage {
			return nodes, nil
		}
		next := fmt.Sprintf("%q", page.PageInfo.EndCursor)
		if page.PageInfo.EndCursor == "" || next == cursor {
			return nil, fmt.Errorf("GitHub issue pagination did not advance")
		}
		cursor = next
	}
}

func (g *gh) parentFacts(ctx context.Context, since time.Time) (trackerFacts, error) {
	facts := trackerFacts{children: map[int][]subIssue{}, references: map[issueLink]externalRef{}}
	// GitHub's since filter is exclusive. Include events at UTC midnight.
	sinceArg := since.Add(-time.Second).Format(time.RFC3339)
	issues, err := readIssuePages(ctx, "null", func(ctx context.Context, cursor string) (issueConnection[trackerIssue], error) {
		query := fmt.Sprintf(`query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    issues(first: 100, after: %s) {
      %s
      nodes {
        number title url body state createdAt milestone { title } subIssuesSummary { total completed }
        timelineItems(first: 10, since: %q, %s) { %s %s }
      }
    }
  }
}`, cursor, issuePageInfo, sinceArg, issueStateEvents, issuePageInfo, issueStateNodes)
		var resp struct {
			Repository struct{ Issues issueConnection[trackerIssue] }
		}
		err := g.graphql(ctx, query, &resp)
		return resp.Repository.Issues, err
	})
	if err != nil {
		return trackerFacts{}, fmt.Errorf("list repository issues: %w", err)
	}
	facts.issues = issues

	local := strings.ToLower(g.owner + "/" + g.name)
	issueStates := map[int]externalRef{}
	for i := range issues {
		issue := &issues[i]
		issueStates[issue.Number] = externalRef{State: issue.State}
		if issue.Timeline.PageInfo.HasNextPage {
			if issue.Timeline.PageInfo.EndCursor == "" {
				return trackerFacts{}, fmt.Errorf("missing timeline cursor for issue #%d", issue.Number)
			}
			more, err := g.issueHistory(ctx, issue.Number, sinceArg, issue.Timeline.PageInfo.EndCursor)
			if err != nil {
				return trackerFacts{}, fmt.Errorf("read state changes for issue #%d: %w", issue.Number, err)
			}
			issue.Timeline.Nodes = append(issue.Timeline.Nodes, more...)
		}
	}

	pending := map[string]map[int][]issueLink{}
	for _, issue := range issues {
		if issue.State != "OPEN" {
			continue
		}
		if issue.SubIssuesSummary.Total > 0 {
			children, err := g.subIssues(ctx, issue.Number)
			if err != nil {
				return trackerFacts{}, fmt.Errorf("read sub-issues of #%d: %w", issue.Number, err)
			}
			facts.children[issue.Number] = children
		}
		parent := candidateParent(issue)
		if !parent.inScope {
			continue
		}
		for _, link := range parent.links {
			repo := link.repo
			if repo == local || repo == "" {
				if state, found := issueStates[link.number]; found {
					facts.references[link] = state
					continue
				}
				repo = local
			}
			if pending[repo] == nil {
				pending[repo] = map[int][]issueLink{}
			}
			pending[repo][link.number] = append(pending[repo][link.number], link)
		}
	}

	for _, repo := range slices.Sorted(maps.Keys(pending)) {
		client := *g
		client.owner, client.name, _ = strings.Cut(repo, "/")
		links := pending[repo]
		for numbers := range slices.Chunk(slices.Sorted(maps.Keys(links)), 100) {
			states, err := client.references(ctx, numbers)
			if err != nil {
				return trackerFacts{}, fmt.Errorf("resolve Closes when references in %s: %w", repo, err)
			}
			for n, state := range states {
				for _, link := range links[n] {
					facts.references[link] = state
				}
			}
		}
	}
	return facts, nil
}

func (g *gh) issueHistory(ctx context.Context, number int, since, cursor string) ([]issueStateEvent, error) {
	return readIssuePages(ctx, fmt.Sprintf("%q", cursor), func(ctx context.Context, cursor string) (issueConnection[issueStateEvent], error) {
		query := fmt.Sprintf(`query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    issue(number: %d) {
      timelineItems(first: 100, after: %s, since: %q, %s) { %s %s }
    }
  }
}`, number, cursor, since, issueStateEvents, issuePageInfo, issueStateNodes)
		var resp struct {
			Repository struct {
				Issue struct {
					TimelineItems issueConnection[issueStateEvent]
				}
			}
		}
		err := g.graphql(ctx, query, &resp)
		return resp.Repository.Issue.TimelineItems, err
	})
}

func (g *gh) subIssues(ctx context.Context, number int) ([]subIssue, error) {
	return readIssuePages(ctx, "null", func(ctx context.Context, cursor string) (issueConnection[subIssue], error) {
		query := fmt.Sprintf(`query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    issue(number: %d) {
      subIssues(first: 100, after: %s) {
        %s
        nodes { createdAt timelineItems(last: 1, %s) { %s } }
      }
    }
  }
}`, number, cursor, issuePageInfo, issueStateEvents, issueStateNodes)
		var resp struct {
			Repository struct {
				Issue struct{ SubIssues issueConnection[subIssue] }
			}
		}
		err := g.graphql(ctx, query, &resp)
		return resp.Repository.Issue.SubIssues, err
	})
}
