package scriptstest

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/cachepolicy"
	"github.com/katl-dev/katl/internal/flavour"
	"gopkg.in/yaml.v3"
)

const (
	ruleUnresolvableKey  = "unresolvable-key"
	ruleUndeclaredFamily = "undeclared-family"
	ruleSaveOffMain      = "save-off-main"
	ruleStaleSaver       = "stale-saver"
	ruleStaleViolation   = "stale-violation"
)

const maxResolutionDepth = 8

var (
	expression     = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)
	stepOutput     = regexp.MustCompile(`^steps\.([\w-]+)\.outputs\.([\w-]+)$`)
	githubContext  = regexp.MustCompile(`(^|[^\w.])github\.`)
	hashFilesCall  = regexp.MustCompile(`^hashFiles\([^()]*\)$`)
	ghaCacheScope  = regexp.MustCompile(`scope=([^,\s'"]*)`)
	literalScope   = regexp.MustCompile(`^[\w.-]+$`)
	localActionUse = regexp.MustCompile(`^\./(?:_build/workflow-actions/)?\.github/actions/([^/@]+)$`)
)

// The flavour expressions expand to each supported flavour, so one step can
// key one family per flavour.
var flavourExpressions = []string{"${{ inputs.flavour }}", "${{ env.KATL_FLAVOUR }}", "${{ matrix.flavour }}"}

func TestActionsCachesFollowPolicy(t *testing.T) {
	root := filepath.Join(repoRoot(t), ".github")
	policy, err := cachepolicy.Load(filepath.Join(root, "cache-policy.json"))
	if err != nil {
		t.Fatal(err)
	}

	for _, violation := range checkCachePolicy(t, os.DirFS(root), policy) {
		t.Error(violation)
	}
}

// Each fixture under testdata/cache-policy seeds the violations of one rule
// beside steps that must pass, and the test requires exactly the seeded set.
func TestCachePolicyRules(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    []string
	}{
		// Passing requires resolving cache-primary-key and action outputs,
		// including through _build/workflow-actions, and expanding one
		// flavour step into each flavour's family. Literal-prefix matching
		// reports the saves as unresolvable; folding flavours into one
		// family reports rpm- as undeclared.
		{fixture: "conforming"},
		// Treating a key that begins with an expression as matching no
		// family would report undeclared-family instead. Treating a scope
		// expression as no scope reports Layers as undeclared scope=buildkit.
		{fixture: "unresolvable-key", want: []string{
			"unresolvable-key - ci.yml job j step Layers",
			"unresolvable-key - ci.yml job j step Restore",
			"unresolvable-key - ci.yml job j step Save",
		}},
		// Checking only the first flavour misses rpm-lts-; ignoring
		// restore-keys misses Mixed; skipping type=gha misses Layers; treating
		// a missing scope as unresolvable misses buildkit's default scope.
		{fixture: "undeclared-family", want: []string{
			"undeclared-family other- ci.yml job j step Other",
			"undeclared-family rpm-lts- ci.yml job j step RPMs",
			"undeclared-family rpm-standard- ci.yml job j step Mixed",
			"undeclared-family scope=buildkit ci.yml job j step Default scope",
			"undeclared-family scope=other ci.yml job j step Layers",
		}},
		// The fixture declares its github expressions as key inputs, which
		// never excuses them. Checking the save's written key text misses the
		// resolved github.sha; matching only github.* accepts
		// needs.plan.outputs.source; matching only a leading github. misses
		// format(); flagging every non-literal rejects the declared week input.
		// Exempting any expression that starts with hashFiles( accepts job
		// hash's disjunction; exempting none rejects job week's hashFiles call.
		{fixture: "commit-in-key", want: []string{
			"commit-in-key go ci.yml job format step Restore",
			"commit-in-key go ci.yml job hash step Restore",
			"commit-in-key go ci.yml job sha step Restore",
			"commit-in-key go ci.yml job sha step Save",
			"commit-in-key go ci.yml job source step Restore",
		}},
		// Accepting the job's guard, a trailing guard or a disjunction would
		// let these steps save off the save ref.
		{fixture: "save-off-main", want: []string{
			"save-off-main - ci.yml job j step Buildx",
			"save-off-main - ci.yml job j step Combined",
			"save-off-main - ci.yml job j step Disjunction",
			"save-off-main - ci.yml job j step Export",
			"save-off-main - ci.yml job j step Setup Go",
			"save-off-main - ci.yml job j step Trailing guard",
			"save-off-main - ci.yml job j step Unguarded",
		}},
		// A guard hard-coded to refs/heads/main passes Main and fails Trunk.
		{fixture: "save-ref", want: []string{
			"save-off-main - ci.yml job j step Export",
			"save-off-main - ci.yml job j step Main",
		}},
		// Counting an action's save once misses the two jobs that use the
		// builder; counting matrix legs or flavours as savers flags job d. Counting
		// savers alone accepts job f, the sole but undeclared saver of kernel.
		{fixture: "multiple-savers", want: []string{
			"multiple-savers builder action builder step docker/build-push-action@v7 #0",
			"multiple-savers go ci.yml job a step Save Go",
			"multiple-savers go ci.yml job b step Save Go",
			"multiple-savers kernel ci.yml job f step Save kernel",
			"stale-saver kernel ci.yml job a step -",
		}},
		// Reading every YAML file under actions lets zz-other.yml replace
		// builder's steps, reporting builder stale, and reads nested as an
		// action whose save is undeclared.
		{fixture: "action-files", want: []string{
			"save-off-main - action builder step Save",
		}},
		{fixture: "stale-saver", want: []string{
			"stale-saver go action unused step -",
			"stale-saver go ci.yml job gone step -",
		}},
		// Matching an entry by family and site without its rule would
		// accept job c's commit-in-key and report nothing stale.
		{fixture: "known-violations", want: []string{
			"commit-in-key go ci.yml job c step Restore",
			"stale-violation go ci.yml job c step -",
		}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			root := filepath.Join("testdata", "cache-policy", tc.fixture)
			policy, err := cachepolicy.Load(filepath.Join(root, "cache-policy.json"))
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for _, violation := range checkCachePolicy(t, os.DirFS(root), policy) {
				got = append(got, violation.id())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("violations:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// cacheSite is where a cache step is written. A step in a composite action
// has no job of its own; it runs in every job that uses the action.
type cacheSite struct {
	workflow, job, action string
}

func (s cacheSite) String() string {
	if s.action != "" {
		return "action " + s.action
	}
	return s.workflow + " job " + s.job
}

// cacheViolation names its subject: the family for family rules, or the
// unmatched key prefix or scope for undeclared-family.
type cacheViolation struct {
	rule, subject string
	site          cacheSite
	step, detail  string
}

func (v cacheViolation) id() string {
	return fmt.Sprintf("%s %s %s step %s", v.rule, v.subject, v.site, v.step)
}

func (v cacheViolation) String() string {
	return fmt.Sprintf("%s step %s: %s %s: %s", v.site, v.step, v.rule, v.subject, v.detail)
}

type cacheAction struct {
	outputs map[string]string
	steps   []workflowStep
}

type cacheSave struct {
	family string
	site   cacheSite
	step   string
}

type cacheChecker struct {
	policy        *cachepolicy.Policy
	workflows     map[string]map[string][]workflowStep
	actions       map[string]cacheAction
	saveGuard     *regexp.Regexp
	saveExport    *regexp.Regexp
	saveCondition string
	violations    map[string]cacheViolation
	saves         []cacheSave
}

// checkCachePolicy reads every workflow and composite action under root,
// which holds the layout of .github, and returns the violations of policy
// that no known violation accepts.
func checkCachePolicy(t *testing.T, root fs.FS, policy *cachepolicy.Policy) []cacheViolation {
	t.Helper()
	saveCondition := "github.ref == '" + policy.SaveRef + "'"
	c := &cacheChecker{
		policy:        policy,
		workflows:     map[string]map[string][]workflowStep{},
		actions:       map[string]cacheAction{},
		saveCondition: saveCondition,
		saveGuard:     regexp.MustCompile(`^github\.ref\s*==\s*'` + regexp.QuoteMeta(policy.SaveRef) + `'(\s*&&|$)`),
		saveExport:    regexp.MustCompile(`^\$\{\{ ` + regexp.QuoteMeta(saveCondition) + ` && '[^']+' \|\| '' \}\}$`),
		violations:    map[string]cacheViolation{},
	}
	c.read(t, root)

	for name, jobs := range c.workflows {
		for job, steps := range jobs {
			c.checkSteps(cacheSite{workflow: name, job: job}, steps)
		}
	}
	for name, action := range c.actions {
		c.checkSteps(cacheSite{action: name}, action.steps)
	}
	c.checkSavers()
	return c.unaccepted()
}

func (c *cacheChecker) read(t *testing.T, root fs.FS) {
	t.Helper()
	parse := func(name string, file any) {
		data, err := fs.ReadFile(root, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(data, file); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
	}

	err := fs.WalkDir(root, "workflows", func(name string, entry fs.DirEntry, err error) error {
		if name == "workflows" && errors.Is(err, fs.ErrNotExist) {
			return fs.SkipDir
		}
		if err != nil {
			return err
		}
		ext := path.Ext(name)
		if entry.IsDir() || (ext != ".yml" && ext != ".yaml") {
			return nil
		}
		var file struct {
			Jobs map[string]struct{ Steps []workflowStep } `yaml:"jobs"`
		}
		parse(name, &file)
		jobs := map[string][]workflowStep{}
		for job, config := range file.Jobs {
			jobs[job] = config.Steps
		}
		c.workflows[path.Base(name)] = jobs
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A composite action is the action.yml, else action.yaml, directly in
	// its own directory, as the runner resolves a local action. Other files
	// and nested directories are not that action.
	for _, metadata := range []string{"action.yml", "action.yaml"} {
		names, err := fs.Glob(root, path.Join("actions", "*", metadata))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			actionName := path.Base(path.Dir(name))
			if _, ok := c.actions[actionName]; ok {
				continue
			}
			var file struct {
				Outputs map[string]struct{ Value string } `yaml:"outputs"`
				Runs    struct{ Steps []workflowStep }    `yaml:"runs"`
			}
			parse(name, &file)
			action := cacheAction{outputs: map[string]string{}, steps: file.Runs.Steps}
			for output, config := range file.Outputs {
				action.outputs[output] = config.Value
			}
			c.actions[actionName] = action
		}
	}
}

func (c *cacheChecker) report(rule, subject string, site cacheSite, step, detail string) {
	v := cacheViolation{rule: rule, subject: subject, site: site, step: step, detail: detail}
	c.violations[v.id()] = v
}

func stepLabel(step workflowStep, index int) string {
	switch {
	case step.Name != "":
		return step.Name
	case step.ID != "":
		return step.ID
	}
	return step.Uses + " #" + strconv.Itoa(index)
}

func usedAction(step workflowStep) string {
	action, _, _ := strings.Cut(strings.ToLower(step.Uses), "@")
	return action
}

// localAction names the composite action a step uses, including the copy
// that workflows check out at the workflow revision under _build/workflow-actions.
func localAction(uses string) (string, bool) {
	match := localActionUse.FindStringSubmatch(uses)
	if match == nil {
		return "", false
	}
	return match[1], true
}

func (c *cacheChecker) checkSteps(site cacheSite, steps []workflowStep) {
	for index, step := range steps {
		label := stepLabel(step, index)
		c.checkSaveRef(site, label, step)

		switch usedAction(step) {
		case "actions/cache", "actions/cache/restore", "actions/cache/save":
			c.checkKey(site, label, step, steps)
		}
		for _, input := range []string{"cache-to", "cache-from"} {
			if value := step.With[input]; strings.Contains(value, "type=gha") {
				c.checkScope(site, label, value, input == "cache-to")
			}
		}
	}
}

// checkSaveRef holds every implicit or explicit save to the policy's save
// ref, through the step's own condition. Job conditions and workflow triggers
// do not count: reusable workflows and dispatches can bypass both.
func (c *cacheChecker) checkSaveRef(site cacheSite, label string, step workflowStep) {
	fail := func(detail string) { c.report(ruleSaveOffMain, "-", site, label, detail) }
	switch usedAction(step) {
	case "actions/cache":
		fail(step.Uses + " saves implicitly; use actions/cache/restore and guard actions/cache/save")
	case "actions/cache/save":
		condition := strings.TrimSpace(step.If)
		condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
		// Require the guard as a leading conjunction. A disjunction could
		// otherwise let an event bypass the guard.
		if !c.saveGuard.MatchString(condition) || strings.Contains(condition, "||") {
			fail(fmt.Sprintf("condition %q must begin with %q and use only conjunctions", step.If, c.saveCondition))
		}
	case "actions/setup-go":
		if step.With["cache"] != "false" {
			fail(step.Uses + " must disable its implicit cache save")
		}
	case "docker/setup-buildx-action":
		if binary := step.With["cache-binary"]; binary != "${{ "+c.saveCondition+" }}" && binary != "false" {
			fail(step.Uses + " must guard or disable its Buildx binary cache")
		}
	}
	if export := step.With["cache-to"]; export != "" && !c.saveExport.MatchString(export) {
		fail(fmt.Sprintf("cache-to %q must be guarded by %q", export, c.saveCondition))
	}
}

// resolve replaces each step output that names another cache key with that
// key, within the steps of the job or action that wrote it. It leaves every
// other expression in canonical ${{ expr }} form.
func (c *cacheChecker) resolve(value string, steps []workflowStep, depth int) string {
	return expression.ReplaceAllStringFunc(value, func(match string) string {
		inner := expression.FindStringSubmatch(match)[1]
		output := stepOutput.FindStringSubmatch(inner)
		if output != nil && depth < maxResolutionDepth {
			for _, step := range steps {
				if step.ID != output[1] {
					continue
				}
				if usedAction(step) == "actions/cache/restore" && output[2] == "cache-primary-key" {
					return c.resolve(step.With["key"], steps, depth+1)
				}
				if name, ok := localAction(step.Uses); ok {
					action := c.actions[name]
					if value, ok := action.outputs[output[2]]; ok {
						return c.resolve(value, action.steps, depth+1)
					}
				}
			}
		}
		return "${{ " + inner + " }}"
	})
}

func (c *cacheChecker) checkKey(site cacheSite, label string, step workflowStep, steps []workflowStep) {
	key := c.resolve(step.With["key"], steps, 0)
	var restoreKeys []string
	for _, line := range strings.Split(step.With["restore-keys"], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			restoreKeys = append(restoreKeys, c.resolve(line, steps, 0))
		}
	}

	flavours := []string{""}
	for _, value := range append([]string{key}, restoreKeys...) {
		for _, flavourExpression := range flavourExpressions {
			if strings.Contains(value, flavourExpression) {
				flavours = flavour.Supported()
			}
		}
	}
	for _, kernel := range flavours {
		expand := func(value string) string {
			for _, flavourExpression := range flavourExpressions {
				value = strings.ReplaceAll(value, flavourExpression, kernel)
			}
			return value
		}

		family, ok := c.keyFamily(site, label, expand(key))
		if !ok {
			continue
		}
		c.checkKeyInputs(site, label, family, expand(key))
		for _, restoreKey := range restoreKeys {
			if restoreFamily, ok := c.keyFamily(site, label, expand(restoreKey)); ok && restoreFamily.Name != family.Name {
				literal, _, _ := strings.Cut(expand(restoreKey), "${{")
				c.report(ruleUndeclaredFamily, literal, site, label, fmt.Sprintf("restore key %q is in family %s, not the key's family %s", restoreKey, restoreFamily.Name, family.Name))
			}
		}
		if usedAction(step) != "actions/cache/restore" {
			c.saves = append(c.saves, cacheSave{family: family.Name, site: site, step: label})
		}
	}
}

// keyFamily matches the literal prefix of a resolved key, because only the
// literal part of a key is known before the workflow runs.
func (c *cacheChecker) keyFamily(site cacheSite, label, key string) (cachepolicy.Family, bool) {
	literal, _, _ := strings.Cut(key, "${{")
	if literal == "" {
		c.report(ruleUnresolvableKey, "-", site, label, fmt.Sprintf("key %q does not begin with a literal", key))
		return cachepolicy.Family{}, false
	}
	family, ok := c.policy.Family(literal)
	if !ok {
		c.report(ruleUndeclaredFamily, literal, site, label, fmt.Sprintf("key %q matches no declared family", key))
	}
	return family, ok
}

// checkKeyInputs refuses commit identifiers in keys: a key that changes on
// every commit saves a new generation per commit.
func (c *cacheChecker) checkKeyInputs(site cacheSite, label string, family cachepolicy.Family, key string) {
	for _, match := range expression.FindAllStringSubmatch(key, -1) {
		inner := match[1]
		switch {
		case githubContext.MatchString(inner):
		case inner == "runner.os", inner == "runner.arch", hashFilesCall.MatchString(inner):
			continue
		case slices.Contains(family.KeyInputs, inner):
			continue
		}
		c.report(cachepolicy.RuleCommitInKey, family.Name, site, label, fmt.Sprintf("key %q uses %s, which is not a declared key input", key, inner))
	}
}

// checkScope maps a docker type=gha cache, which names no key, to the
// families that declare its scope. Without scope=, buildkit uses its default
// scope; a scope that is not a literal names no family before the run.
func (c *cacheChecker) checkScope(site cacheSite, label, value string, save bool) {
	scope := "buildkit"
	if match := ghaCacheScope.FindStringSubmatch(value); match != nil {
		if !literalScope.MatchString(match[1]) {
			c.report(ruleUnresolvableKey, "-", site, label, fmt.Sprintf("type=gha cache %q has a scope that is not a literal", value))
			return
		}
		scope = match[1]
	}
	found := false
	for _, family := range c.policy.Families {
		if family.GHAScope != scope {
			continue
		}
		found = true
		if save {
			c.saves = append(c.saves, cacheSave{family: family.Name, site: site, step: label})
		}
	}
	if !found {
		c.report(ruleUndeclaredFamily, "scope="+scope, site, label, fmt.Sprintf("type=gha scope %q matches no declared family", scope))
	}
}

// actionUsers returns, for each composite action, the workflow jobs that run
// it directly or through another action.
func (c *cacheChecker) actionUsers() map[string][]string {
	users := map[string][]string{}
	var visit func(identity string, steps []workflowStep, seen map[string]bool)
	visit = func(identity string, steps []workflowStep, seen map[string]bool) {
		for _, step := range steps {
			name, ok := localAction(step.Uses)
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			users[name] = append(users[name], identity)
			visit(identity, c.actions[name].steps, seen)
		}
	}
	for name, jobs := range c.workflows {
		for job, steps := range jobs {
			visit(name+"/"+job, steps, map[string]bool{})
		}
	}
	return users
}

func declaredSaver(saver cachepolicy.Saver, site cacheSite) bool {
	return saver.Workflow == site.workflow && saver.Job == site.job && saver.Action == site.action
}

// checkSavers holds each family to the savers it declares. Matrix legs and
// callers of a reusable workflow share one saver, the job that writes the
// step; a save in an action is saved by every job that uses the action.
func (c *cacheChecker) checkSavers() {
	users := c.actionUsers()
	for _, family := range c.policy.Families {
		identities := map[string]bool{}
		var saves []cacheSave
		for _, save := range c.saves {
			if save.family != family.Name {
				continue
			}
			saves = append(saves, save)
			if save.site.action == "" {
				identities[save.site.workflow+"/"+save.site.job] = true
			}
			for _, user := range users[save.site.action] {
				identities[user] = true
			}
		}
		savers := slices.Sorted(maps.Keys(identities))

		for _, save := range saves {
			if !slices.ContainsFunc(family.Savers, func(saver cachepolicy.Saver) bool { return declaredSaver(saver, save.site) }) {
				c.report(cachepolicy.RuleMultipleSavers, family.Name, save.site, save.step, "not a declared saver of the family")
			} else if len(savers) > 1 {
				c.report(cachepolicy.RuleMultipleSavers, family.Name, save.site, save.step, "family is saved by "+strings.Join(savers, ", "))
			}
		}
		for _, saver := range family.Savers {
			site := cacheSite{workflow: saver.Workflow, job: saver.Job, action: saver.Action}
			if !slices.ContainsFunc(saves, func(save cacheSave) bool { return save.site == site }) {
				c.report(ruleStaleSaver, family.Name, site, "-", "declared saver no longer saves the family")
			}
		}
	}
}

// unaccepted removes the violations that a known violation accepts and adds
// a stale-violation for every entry that accepts nothing, so the list only
// shrinks.
func (c *cacheChecker) unaccepted() []cacheViolation {
	used := make([]bool, len(c.policy.KnownViolations))
	var result []cacheViolation
	for _, v := range c.violations {
		accepted := false
		for i, known := range c.policy.KnownViolations {
			if known.Family == v.subject && known.Rule == v.rule && (cacheSite{workflow: known.Workflow, job: known.Job, action: known.Action}) == v.site {
				used[i] = true
				accepted = true
			}
		}
		if !accepted {
			result = append(result, v)
		}
	}
	for i, known := range c.policy.KnownViolations {
		if !used[i] {
			result = append(result, cacheViolation{
				rule:    ruleStaleViolation,
				subject: known.Family,
				site:    cacheSite{workflow: known.Workflow, job: known.Job, action: known.Action},
				step:    "-",
				detail:  "known " + known.Rule + " violation no longer occurs",
			})
		}
	}
	slices.SortFunc(result, func(a, b cacheViolation) int { return strings.Compare(a.id(), b.id()) })
	return result
}
