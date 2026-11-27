package cachepolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validPolicy = `{
  "version": 1,
  "save_ref": "refs/heads/main",
  "budget_bytes": 300,
  "alert_bytes": 400,
  "families": [
    {"name": "go", "key_prefix": "go-", "contents": "Go", "savers": [{"workflow": "ci.yml", "job": "fast"}], "budget_bytes": 100, "max_generations": 2},
    {"name": "rpm-standard", "key_prefix": "rpm-standard-", "contents": "RPMs", "savers": [{"action": "build"}], "budget_bytes": 100},
    {"name": "rpm-lts", "key_prefix": "rpm-lts-", "contents": "RPMs", "savers": [{"action": "build"}], "budget_bytes": 100}
  ],
  "known_violations": [
    {"family": "rpm-lts", "rule": "multiple-savers", "action": "build"}
  ]
}`

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache-policy.json")
	if err := os.WriteFile(path, []byte(validPolicy), 0o644); err != nil {
		t.Fatal(err)
	}

	policy, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if policy.SaveRef != "refs/heads/main" || len(policy.Families) != 3 || *policy.Families[0].MaxGenerations != 2 || policy.Families[1].MaxGenerations != nil {
		t.Fatalf("Load() = %+v", policy)
	}
}

// Each case changes the valid policy in one way that the policy must refuse.
func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name, old, new, want string
	}{
		{"duplicate name", `"name": "rpm-lts"`, `"name": "go"`, "declared twice"},
		// Distinct names with nested prefixes would make Family's answer
		// depend on declaration order.
		{"nested prefix", `"key_prefix": "rpm-lts-"`, `"key_prefix": "rpm-"`, "nests under"},
		{"duplicate prefix", `"key_prefix": "rpm-lts-"`, `"key_prefix": "go-"`, "nests under"},
		{"unacceptable rule", `"rule": "multiple-savers"`, `"rule": "save-off-main"`, "cannot be accepted"},
		{"unresolvable rule", `"rule": "multiple-savers"`, `"rule": "unresolvable-key"`, "cannot be accepted"},
		{"undeclared violation family", `{"family": "rpm-lts"`, `{"family": "rpm"`, "undeclared family"},
		{"violation without job", `"action": "build"}
  ]`, `"workflow": "ci.yml"}
  ]`, "must name a workflow and job"},
		{"saver with action and job", `[{"action": "build"}], "budget_bytes": 100}
  ]`, `[{"action": "build", "job": "fast"}], "budget_bytes": 100}
  ]`, "must name a workflow and job"},
		{"no generations", `"max_generations": 2`, `"max_generations": 0`, "fewer than one generation"},
		{"short ref", `"save_ref": "refs/heads/main"`, `"save_ref": "main"`, "not a full ref"},
		{"unknown field", `"alert_bytes": 400`, `"alert_bytes": 400, "budget": 1`, "unknown field"},
		// Without a commit-in-key entry the families must fit the budget.
		{"over budget", `"budget_bytes": 300`, `"budget_bytes": 299`, "over the 299-byte budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(validPolicy, tc.old) {
				t.Fatalf("fixture lacks %q", tc.old)
			}
			_, err := parse([]byte(strings.Replace(validPolicy, tc.old, tc.new, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse() error = %v, want %q", err, tc.want)
			}
		})
	}

	if _, err := parse([]byte(validPolicy)); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
}

// A commit-in-key entry defers the budget sum, and only that rule does.
func TestValidateDefersBudgetForCommitKeys(t *testing.T) {
	overBudget := strings.Replace(validPolicy, `"budget_bytes": 300`, `"budget_bytes": 1`, 1)
	deferred := strings.Replace(overBudget, `"rule": "multiple-savers"`, `"rule": "commit-in-key"`, 1)

	if _, err := parse([]byte(deferred)); err != nil {
		t.Fatalf("commit-keyed policy: %v", err)
	}
	if _, err := parse([]byte(overBudget)); err == nil {
		t.Fatal("multiple-savers entry deferred the budget sum")
	}
}

func TestFamily(t *testing.T) {
	policy, err := parse([]byte(validPolicy))
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		"go-Linux-abc":           "go",
		"rpm-standard-Linux-abc": "rpm-standard",
		"rpm-lts-Linux-abc":      "rpm-lts",
		"rpm-Linux-abc":          "",
		"Go-Linux":               "",
		"xgo-Linux":              "",
	} {
		family, ok := policy.Family(key)
		if ok != (want != "") || family.Name != want {
			t.Errorf("Family(%q) = %q, %v; want %q", key, family.Name, ok, want)
		}
	}
}
