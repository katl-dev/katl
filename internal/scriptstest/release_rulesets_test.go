package scriptstest

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	rulesetDir            = ".github/rulesets"
	finalTagRuleset       = "final-release-tags.json"
	branchCreationRuleset = "release-branch-creation.json"
	releaseTagRuleset     = "release-tags.json"
	releaseBranchRuleset  = "release-branches.json"
)

type rulesetBypass struct {
	ID   int    `json:"actor_id"`
	Type string `json:"actor_type"`
	Mode string `json:"bypass_mode"`
}

type rulesetRefNames struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

type releaseRuleset struct {
	Name        string `json:"name"`
	Target      string `json:"target"`
	Enforcement string `json:"enforcement"`
	Conditions  struct {
		RefName rulesetRefNames `json:"ref_name"`
	} `json:"conditions"`
	BypassActors []rulesetBypass `json:"bypass_actors"`
	Rules        []struct {
		Type       string          `json:"type"`
		Parameters json.RawMessage `json:"parameters"`
	} `json:"rules"`
}

func readReleaseRuleset(t *testing.T, name string) releaseRuleset {
	t.Helper()
	file := filepath.Join(repoRoot(t), rulesetDir, name)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var ruleset *releaseRuleset
	if err := decoder.Decode(&ruleset); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if ruleset == nil {
		t.Fatalf("%s: expected a ruleset object", name)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		t.Fatalf("%s: expected one JSON object, trailing data decode result: %v", name, err)
	}
	return *ruleset
}

func matchesRulesetRef(t *testing.T, patterns []string, ref string) bool {
	t.Helper()
	matched := false
	for _, pattern := range patterns {
		ok, err := path.Match(pattern, ref)
		if err != nil {
			t.Fatalf("invalid ref pattern %q: %v", pattern, err)
		}
		matched = matched || ok
	}
	return matched
}

func assertRulesetCoverage(t *testing.T, ruleset releaseRuleset, ref string, want bool) {
	t.Helper()
	names := ruleset.Conditions.RefName
	included := matchesRulesetRef(t, names.Include, ref)
	excluded := matchesRulesetRef(t, names.Exclude, ref)
	if covered := included && !excluded; covered != want {
		t.Errorf("%s covers %q = %t, want %t", ruleset.Name, ref, covered, want)
	}
}

func TestReleaseRulesetFinalTags(t *testing.T) {
	ruleset := readReleaseRuleset(t, finalTagRuleset)
	for _, ref := range []string{"refs/tags/v2026.12.1", "refs/tags/v2027.1.0"} {
		assertRulesetCoverage(t, ruleset, ref, true)
	}
	for _, ref := range []string{
		"refs/tags/v2026.12.1-rc.1",
		"refs/tags/v2026.12.1-rc.12",
		"refs/tags/v2026.12.1-alpha.1",
		"refs/tags/v2026.12.1-beta.1",
	} {
		assertRulesetCoverage(t, ruleset, ref, false)
	}

	names := ruleset.Conditions.RefName
	if want := []string{"refs/tags/v*"}; !slices.Equal(names.Include, want) {
		t.Errorf("final tag includes = %q, want %q", names.Include, want)
	}
	if want := []string{"refs/tags/v*-*"}; !slices.Equal(names.Exclude, want) {
		t.Errorf("final tag excludes = %q, want %q", names.Exclude, want)
	}
}

func TestReleaseRulesetBranchCreation(t *testing.T) {
	creation := readReleaseRuleset(t, branchCreationRuleset)
	protection := readReleaseRuleset(t, releaseBranchRuleset)
	for _, ref := range []string{
		"refs/heads/release/2026.12.1",
		"refs/heads/release/2027.1.0",
		"refs/heads/release/2026.12.x",
	} {
		assertRulesetCoverage(t, creation, ref, true)
		assertRulesetCoverage(t, protection, ref, true)
	}
	for _, ref := range []string{
		"refs/heads/rel/903-release-workflow",
		"refs/heads/main",
		"refs/heads/fix/release-notes",
	} {
		assertRulesetCoverage(t, creation, ref, false)
	}

	names := creation.Conditions.RefName
	if want := protection.Conditions.RefName.Include; !slices.Equal(names.Include, want) {
		t.Errorf("branch creation includes = %q, want branch protection includes %q", names.Include, want)
	}
	if len(names.Exclude) != 0 {
		t.Errorf("branch creation excludes = %q, want none", names.Exclude)
	}
}

func TestReleaseRulesetCreationBypass(t *testing.T) {
	// Final tags come from the katl-release App and release branches from
	// the katl-release-cut App.
	for file, app := range map[string]int{finalTagRuleset: 5958368, branchCreationRuleset: 5964009} {
		t.Run(file, func(t *testing.T) {
			want := []rulesetBypass{{ID: app, Type: "Integration", Mode: "always"}}
			ruleset := readReleaseRuleset(t, file)
			if len(ruleset.Rules) != 1 || ruleset.Rules[0].Type != "creation" || len(ruleset.Rules[0].Parameters) != 0 {
				t.Errorf("rules = %+v, want a single creation rule without parameters", ruleset.Rules)
			}
			if !slices.Equal(ruleset.BypassActors, want) {
				t.Errorf("bypass actors = %+v, want %+v", ruleset.BypassActors, want)
			}
		})
	}
}

func TestReleaseRulesetExistingProtections(t *testing.T) {
	for _, tc := range []struct {
		file  string
		rules []string
	}{
		{releaseTagRuleset, []string{"deletion", "non_fast_forward", "update"}},
		{releaseBranchRuleset, []string{"deletion", "non_fast_forward", "pull_request", "required_status_checks"}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			ruleset := readReleaseRuleset(t, tc.file)
			if len(ruleset.BypassActors) != 0 {
				t.Errorf("bypass actors = %+v, want none", ruleset.BypassActors)
			}
			var types []string
			for _, rule := range ruleset.Rules {
				types = append(types, rule.Type)
			}
			for _, want := range tc.rules {
				if !slices.Contains(types, want) {
					t.Errorf("rules %q omit %q", types, want)
				}
			}
		})
	}

	tags := readReleaseRuleset(t, releaseTagRuleset)
	if excludes := tags.Conditions.RefName.Exclude; len(excludes) != 0 {
		t.Errorf("tag protection excludes = %q, want none", excludes)
	}
	assertRulesetCoverage(t, tags, "refs/tags/v2026.12.1-rc.1", true)
}

func TestReleaseRulesetEnforcement(t *testing.T) {
	for _, tc := range []struct {
		file, name, target string
	}{
		{finalTagRuleset, "final release tags", "tag"},
		{releaseTagRuleset, "release tags", "tag"},
		{branchCreationRuleset, "release branch creation", "branch"},
		{releaseBranchRuleset, "release branches", "branch"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			ruleset := readReleaseRuleset(t, tc.file)
			if ruleset.Name != tc.name || ruleset.Target != tc.target || ruleset.Enforcement != "active" {
				t.Errorf("name, target, enforcement = %q, %q, %q, want %q, %q, active",
					ruleset.Name, ruleset.Target, ruleset.Enforcement, tc.name, tc.target)
			}
		})
	}
}

func TestReleaseRulesetProcedureInputs(t *testing.T) {
	const doc = "docs/development/release-rulesets.md"
	data, err := os.ReadFile(filepath.Join(repoRoot(t), doc))
	if err != nil {
		t.Fatal(err)
	}

	// TestDocumentationLocalLinks verifies the payload links resolve.
	for _, file := range []string{finalTagRuleset, branchCreationRuleset} {
		input := "--input " + path.Join(rulesetDir, file)
		if !strings.Contains(string(data), input+"\n") {
			t.Errorf("%s does not use %q", doc, input)
		}
	}
}
