// Package cachepolicy owns the declared GitHub Actions cache policy: which
// cache families exist, who saves them, and how much each may hold.
package cachepolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Rules that a known violation may name. Every other conformance rule is a
// hard failure.
const (
	RuleCommitInKey    = "commit-in-key"
	RuleMultipleSavers = "multiple-savers"
)

// Policy is the content of .github/cache-policy.json.
type Policy struct {
	Version         int              `json:"version"`
	SaveRef         string           `json:"save_ref"`
	BudgetBytes     int64            `json:"budget_bytes"`
	AlertBytes      int64            `json:"alert_bytes"`
	Families        []Family         `json:"families"`
	KnownViolations []KnownViolation `json:"known_violations"`
}

// Family is one group of cache entries that share a key prefix.
type Family struct {
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	// GHAScope maps a docker type=gha cache scope, which names no key, to
	// this family.
	GHAScope string `json:"gha_scope,omitempty"`
	Contents string `json:"contents"`
	// KeyInputs are the non-literal expressions a key may contain besides
	// the runner, hashFiles and flavour expressions.
	KeyInputs   []string `json:"key_inputs,omitempty"`
	Savers      []Saver  `json:"savers"`
	BudgetBytes int64    `json:"budget_bytes"`
	// MaxGenerations is the number of distinct keys kept; nil keeps every
	// key within the budget.
	MaxGenerations *int `json:"max_generations,omitempty"`
}

// Saver identifies where a saving step is written: a job in a workflow file,
// or a composite action under .github/actions.
type Saver struct {
	Workflow string `json:"workflow,omitempty"`
	Job      string `json:"job,omitempty"`
	Action   string `json:"action,omitempty"`
}

// KnownViolation is an accepted occurrence of a rule that a follow-up
// removes. A violation in an action is accepted for every job that uses it.
type KnownViolation struct {
	Family   string `json:"family"`
	Rule     string `json:"rule"`
	Workflow string `json:"workflow,omitempty"`
	Action   string `json:"action,omitempty"`
	Job      string `json:"job,omitempty"`
}

// Load reads and validates a policy file.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	policy, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return policy, nil
}

// Parse validates the content of a policy file.
func Parse(data []byte) (*Policy, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy Policy
	if err := decoder.Decode(&policy); err != nil {
		return nil, err
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	return &policy, nil
}

func (p *Policy) validate() error {
	var errs []error
	if p.Version != 1 {
		errs = append(errs, fmt.Errorf("version %d is not supported", p.Version))
	}
	if !strings.HasPrefix(p.SaveRef, "refs/") {
		errs = append(errs, fmt.Errorf("save_ref %q is not a full ref", p.SaveRef))
	}

	names := map[string]bool{}
	var budgets int64
	for i, family := range p.Families {
		switch {
		case family.Name == "":
			errs = append(errs, fmt.Errorf("family %d has no name", i))
		case names[family.Name]:
			errs = append(errs, fmt.Errorf("family %s is declared twice", family.Name))
		}
		names[family.Name] = true
		if family.KeyPrefix == "" {
			errs = append(errs, fmt.Errorf("family %s has no key_prefix", family.Name))
		}
		if family.BudgetBytes < 0 {
			errs = append(errs, fmt.Errorf("family %s has a negative budget", family.Name))
		}
		if family.MaxGenerations != nil && *family.MaxGenerations < 1 {
			errs = append(errs, fmt.Errorf("family %s keeps fewer than one generation", family.Name))
		}
		for _, saver := range family.Savers {
			if !validSite(saver.Workflow, saver.Job, saver.Action) {
				errs = append(errs, fmt.Errorf("family %s saver %+v must name a workflow and job, or an action", family.Name, saver))
			}
		}
		budgets += family.BudgetBytes
	}

	// Prefix-free keys make Family a plain prefix match with one answer.
	for i, a := range p.Families {
		for j, b := range p.Families {
			if i != j && a.KeyPrefix != "" && strings.HasPrefix(b.KeyPrefix, a.KeyPrefix) {
				errs = append(errs, fmt.Errorf("family %s key_prefix %q nests under family %s key_prefix %q", b.Name, b.KeyPrefix, a.Name, a.KeyPrefix))
			}
		}
	}

	commitKeyed := false
	for _, known := range p.KnownViolations {
		if known.Rule != RuleCommitInKey && known.Rule != RuleMultipleSavers {
			errs = append(errs, fmt.Errorf("known violation %+v: rule %q cannot be accepted", known, known.Rule))
		}
		if !names[known.Family] {
			errs = append(errs, fmt.Errorf("known violation %+v names an undeclared family", known))
		}
		if !validSite(known.Workflow, known.Job, known.Action) {
			errs = append(errs, fmt.Errorf("known violation %+v must name a workflow and job, or an action", known))
		}
		commitKeyed = commitKeyed || known.Rule == RuleCommitInKey
	}
	// Commit-keyed families keep a generation per commit, so their budgets
	// cannot fit until their keys change.
	if !commitKeyed && budgets > p.BudgetBytes {
		errs = append(errs, fmt.Errorf("family budgets total %d bytes, over the %d-byte budget", budgets, p.BudgetBytes))
	}
	return errors.Join(errs...)
}

// validSite accepts a job in a workflow, or an action without a job, because an
// action is attributed to every job that uses it.
func validSite(workflow, job, action string) bool {
	if action != "" {
		return workflow == "" && job == ""
	}
	return workflow != "" && job != ""
}

// Family returns the family whose key prefix begins key.
func (p *Policy) Family(key string) (Family, bool) {
	for _, family := range p.Families {
		if strings.HasPrefix(key, family.KeyPrefix) {
			return family, true
		}
	}
	return Family{}, false
}
