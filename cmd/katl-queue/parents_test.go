package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"testing"
	"time"
)

func TestParentsReport(t *testing.T) {
	raw, err := os.ReadFile("testdata/parents.json")
	if err != nil {
		t.Fatal(err)
	}
	var responses map[string]json.RawMessage
	if err := json.Unmarshal(raw, &responses); err != nil {
		t.Fatal(err)
	}
	var issues struct {
		Repository struct{ Issues issueConnection[trackerIssue] }
	}
	if err := decodeGraphQL(responses["issues"], &issues); err != nil {
		t.Fatal(err)
	}
	var parents struct {
		Repository map[string]struct{ SubIssues issueConnection[subIssue] }
	}
	if err := decodeGraphQL(responses["subIssues"], &parents); err != nil {
		t.Fatal(err)
	}
	states, err := decodeReferences(responses["references"], []int{8, 10, 100, 101})
	if err != nil {
		t.Fatal(err)
	}

	facts := trackerFacts{
		issues: issues.Repository.Issues.Nodes, children: map[int][]subIssue{},
		references: map[issueLink]externalRef{},
	}
	for _, n := range []int{1, 2, 3, 4} {
		facts.children[n] = parents.Repository[fmt.Sprintf("parent%d", n)].SubIssues.Nodes
	}
	for n, state := range states {
		facts.references[issueLink{number: n}] = state
	}
	facts.references[issueLink{repo: "katl-dev/katl", number: 100}] = states[100]

	start := time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	report := evaluateParents(facts, start, now)

	for _, section := range []struct {
		name string
		got  []parentFinding
		want []int
	}{
		{"all children closed", report.finished, []int{1}},
		{"condition references resolved", report.conditionsMet, []int{5}},
		{"stalled for seven days", report.stalled, []int{3}},
		{"condition missing", report.missingCondition, []int{1, 3, 7}},
	} {
		t.Run(section.name, func(t *testing.T) {
			var numbers []int
			for _, finding := range section.got {
				numbers = append(numbers, finding.number)
			}
			if !slices.Equal(numbers, section.want) {
				t.Errorf("issues = %v, want %v", numbers, section.want)
			}
		})
	}

	t.Run("daily burndown", func(t *testing.T) {
		got := map[string]issueCounts{}
		for day, counts := range report.days {
			got[day.Format(time.DateOnly)] = counts
		}
		want := map[string]issueCounts{
			"2026-01-13": {opened: 1, closed: 1, open: 12},
			"2026-01-14": {opened: 0, closed: 2, open: 10},
			"2026-01-15": {opened: 0, closed: 0, open: 11},
		}
		if !maps.Equal(got, want) {
			t.Errorf("daily counts = %+v, want %+v", got, want)
		}
	})
	t.Run("current milestone burndown", func(t *testing.T) {
		want := map[string]issueCounts{
			"Roadmap": {opened: 1, closed: 2, open: 1},
			"":        {opened: 0, closed: 1, open: 10},
		}
		if !maps.Equal(report.milestones, want) {
			t.Errorf("milestone counts = %+v, want %+v", report.milestones, want)
		}
	})
}
