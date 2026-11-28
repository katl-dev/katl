package scriptstest

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/releasestate"
)

const ciWorkflowPath = ".github/workflows/ci.yml"

type releaseCoverageGap struct {
	modeSite
	detail string
}

func (gap releaseCoverageGap) String() string {
	source := ""
	if gap.source != "" {
		source = fmt.Sprintf(" %q", gap.source)
	}
	return fmt.Sprintf("%s: %s%s: %s; reach it from a merge-group path or add an allowlist entry with a substitute",
		gap.workflowSite, gap.field, source, gap.detail)
}

type releaseException struct {
	site       workflowSite
	category   string
	issue      int
	substitute string
	// The obligations are exact. An exemption for an existing condition or
	// shell body must not also exempt a new mode expression on the same step.
	obligations []string
}

const releaseRehearsal = "rehearsal #780"

func TestReleaseWorkflowCoverage(t *testing.T) {
	root := repoRoot(t)
	release := traceWorkflows(t, root, releasestate.ReleaseWorkflow, releaseCoverageRuns(), releaseCoverageFixtures())
	merge := traceWorkflows(t, root, ciWorkflowPath, ciCoverageRuns(), releaseCoverageFixtures())
	for _, diagnostic := range checkReleaseCoverage(root, release, merge, releaseExceptions) {
		t.Error(diagnostic)
	}
	checkCIResultCoverage(t, root, merge)
}

func releaseCoverageRuns() []map[string]string {
	var runs []map[string]string
	for _, action := range releasestate.Actions() {
		ctx := workflowEvent("workflow_dispatch", "refs/heads/release/2026.12.1")
		ctx["needs.decide.outputs.action"] = string(action)
		ctx["needs.decide.outputs.attempt"] = "1"
		ctx["needs.decide.outputs.commit"] = calledCommit
		ctx["needs.decide.outputs.version"] = "2026.12.1-rc.1"
		switch action {
		case releasestate.Final, releasestate.RetryFinal:
			ctx["needs.decide.outputs.version"] = "2026.12.1"
		case releasestate.Refuse:
			ctx["needs.decide.result"] = "failure"
			ctx["steps.decision.outcome"] = "failure"
		}
		if action == releasestate.RetryCandidate || action == releasestate.RetryFinal {
			ctx["inputs.retry"] = "transient build failure"
		}
		runs = append(runs, ctx)

		switch action {
		case releasestate.Candidate, releasestate.RetryCandidate, releasestate.Final, releasestate.RetryFinal:
			failed := maps.Clone(ctx)
			failed["needs.build.result"] = "failure"
			failed["needs.qualify.result"] = "failure"
			failed["steps.validation.outcome"] = "failure"
			runs = append(runs, failed)
		}
	}
	return runs
}

func workflowEvent(event, ref string) map[string]string {
	return map[string]string{
		"github.event_name": event, "github.ref": ref, "github.ref_type": "branch",
		"github.ref_name": strings.TrimPrefix(ref, "refs/heads/"),
		"github.actor":    "katl-contributor", "github.sha": calledCommit,
		"github.run_id": "42", "github.run_attempt": "1",
	}
}

const hostedCoverageMatrix = `{"include":[{"name":"cluster","pattern":"TestCluster"}]}`

// These are representative selected-plan outputs. The planner tests exercise
// how CI selects them from the change, and the graph uses the actual job
// conditions and matrix expressions to consume them.
func ciCoverageRuns() []map[string]string {
	group := workflowEvent("merge_group", "refs/heads/gh-readonly-queue/main/pr-1")
	group["github.event.merge_group.base_ref"] = "refs/heads/main"
	pr := workflowEvent("pull_request", "refs/pull/1/merge")
	pr["github.base_ref"] = "release/2026.12.1"
	pr["github.event.pull_request.number"] = "1"
	for _, ctx := range []map[string]string{group, pr} {
		ctx["needs.plan.outputs.vm-flavours"] = `["standard","lts"]`
		ctx["needs.plan.outputs.driver-flavours"] = `["standard"]`
		ctx["needs.plan.outputs.production-flavours"] = `["standard","lts"]`
		ctx["needs.plan.outputs.driver-digests"] = `{"standard":"v1:fixture"}`
		ctx["needs.plan.outputs.scenarios"] = hostedCoverageMatrix
		ctx["needs.plan.outputs.kubernetes-version"] = ""
		ctx["needs.plan.outputs.production-version"] = "2026.12.1-dev.0"
		ctx["needs.plan.outputs.production-previous-tag"] = "v2026.12.0"
		ctx["steps.plan.outputs.full-plan"] = "true"
	}
	return []map[string]string{group, pr}
}

// Outputs used to expand dynamic matrices and conditional artifacts stand in
// for results of commands. The graph never executes workflow steps.
func releaseCoverageFixtures() map[string]map[string]string {
	return map[string]map[string]string{
		productionWorkflowPath: {
			"needs.runtime.outputs.extensions": `["endpoint-advertiser"]`,
		},
		hostedVMWorkflowPath: {
			"needs.plan.outputs.matrix":   hostedCoverageMatrix,
			"needs.plan.outputs.prepared": "false",
			"needs.plan.outputs.source":   calledCommit,
		},
		artifactsWorkflowPath: {
			"needs.identity.outputs.version":        "${{ inputs.version }}",
			"needs.identity.outputs.previous-tag":   "v2026.12.0",
			"needs.build-standard.outputs.artifact": "katl-assembled-standard-" + calledCommit,
			"needs.build-lts.outputs.artifact":      "katl-assembled-lts-" + calledCommit,
		},
	}
}

func coverageFindings(release, merge *workflowTrace) []releaseCoverageGap {
	var problems []releaseCoverageGap
	add := func(site workflowSite, field, source, detail string) {
		problems = append(problems, releaseCoverageGap{modeSite{site, field, source}, detail})
	}
	for file, triggers := range release.triggers {
		for name, trigger := range triggers {
			site := workflowSite{file, "(on)", name}
			if !release.used[file][name] && !merge.used[file][name] {
				add(site, "trigger", "", "unused trigger")
			}
			for input := range trigger.Inputs {
				if (!release.used[file][name] && !merge.used[file][name]) || !release.reads[file][input] {
					add(workflowSite{file, "(on)", name + "." + input}, "input", "", "unused input")
				}
			}
		}
	}
	unmatched := map[workflowSite]map[string]bool{}
	missingCalls := map[workflowSite]bool{}
	counterparts := map[workflowSite]map[workflowSite]bool{}
	mergeSites := map[workflowSite][]workflowVisit{}
	mergeOwners := map[string][]workflowVisit{}
	for _, visit := range merge.visits {
		mergeSites[visit.site] = append(mergeSites[visit.site], visit)
		if visit.owner != "" {
			mergeOwners[visit.owner] = append(mergeOwners[visit.owner], visit)
		}
	}
	for _, visit := range release.visits {
		sameOwner := mergeOwners[visit.owner]
		if visit.call && !slices.ContainsFunc(sameOwner, func(other workflowVisit) bool {
			return other.call
		}) {
			missingCalls[visit.site] = true
		}
		matched := false
		for _, candidates := range [][]workflowVisit{mergeSites[visit.site], sameOwner} {
			for _, other := range candidates {
				if !sameWorkflowVisit(visit, other) {
					continue
				}
				matched = true
				if counterparts[visit.site] == nil {
					counterparts[visit.site] = map[workflowSite]bool{}
				}
				counterparts[visit.site][other.site] = true
			}
		}
		if matched {
			continue
		}
		if unmatched[visit.site] == nil {
			unmatched[visit.site] = map[string]bool{}
		}
		var with []string
		for _, key := range slices.Sorted(maps.Keys(visit.with)) {
			if !workflowValueInput(key) {
				with = append(with, fmt.Sprintf("%s=%q", key, visit.with[key]))
			}
		}
		unmatched[visit.site][strings.Join(with, ", ")] = true
	}
	for site := range missingCalls {
		add(site, "call", "", "local workflow is never called before merge")
	}
	for site := range release.inactiveSteps {
		job := workflowSite{site.file, site.job, "(job)"}
		if !release.activeJobs[job] && !merge.activeJobs[job] {
			add(site, "inactive", "", "job never starts on a release or pre-merge path")
		}
	}
	for site, variants := range unmatched {
		detail := "unreached release step or job"
		if settings := strings.Join(slices.Sorted(maps.Keys(variants)), "; "); settings != "" {
			detail += " with " + settings
		}
		add(site, "reach", "", detail)
	}

	for site, outcomes := range release.mode {
		beforeMerge := map[string]bool{}
		maps.Copy(beforeMerge, merge.mode[site])
		for counterpart := range counterparts[site.workflowSite] {
			maps.Copy(beforeMerge, merge.mode[modeSite{counterpart, site.field, site.source}])
		}
		switch site.field {
		case "version":
			add(site.workflowSite, site.field, site.source, "name the tests that cover each version shape")
		case "shell":
			add(site.workflowSite, site.field, site.source, "requires a single owner-command invocation")
		case "run-reference":
			if outcomes["inline shell"] {
				add(site.workflowSite, site.field, site.source, "mode-dependent inline shell must move to an owner command")
				continue
			}
			fallthrough
		default:
			all := maps.Clone(outcomes)
			maps.Copy(all, release.probes[site])
			var missing []string
			for _, value := range slices.Sorted(maps.Keys(all)) {
				if !beforeMerge[value] {
					missing = append(missing, fmt.Sprintf("%q", value))
				}
			}
			if len(missing) > 0 {
				add(site.workflowSite, site.field, site.source, "mode outcomes absent before merge: "+strings.Join(missing, ", "))
			} else if release.guards[site] && !outcomes["true"] && !beforeMerge["true"] {
				add(site.workflowSite, site.field, site.source, "mode condition never reaches this step or job")
			}
		}
	}
	slices.SortFunc(problems, func(a, b releaseCoverageGap) int {
		return strings.Compare(a.String(), b.String())
	})
	return problems
}

func workflowValueInput(name string) bool {
	return name == "commit" || name == "retention-days"
}

func sameWorkflowVisit(a, b workflowVisit) bool {
	// A deployment environment is a job guard, so the name alone is
	// insufficient to match gates from two different jobs.
	if a.site.step == "(environment)" {
		return a.site == b.site && a.owner == b.owner
	}
	return (a.site == b.site || a.owner != "" && a.owner == b.owner) && equalWorkflowWith(a.with, b.with)
}

func equalWorkflowWith(a, b map[string]string) bool {
	for name, value := range a {
		if other, ok := b[name]; !workflowValueInput(name) && (!ok || other != value) {
			return false
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok && !workflowValueInput(name) {
			return false
		}
	}
	return true
}

func coverageObligation(site modeSite) string {
	if site.source == "" {
		return site.field
	}
	return site.field + " " + site.source
}

func checkReleaseCoverage(root string, release, merge *workflowTrace, allow []releaseException) []string {
	var diagnostics []string
	findings := map[modeSite]releaseCoverageGap{}
	for _, problem := range coverageFindings(release, merge) {
		findings[problem.modeSite] = problem
	}
	tests, err := workflowTestNames(root)
	if err != nil {
		diagnostics = append(diagnostics, err.Error())
	}
	for _, entry := range allow {
		switch entry.category {
		case "gap":
			if entry.issue <= 0 {
				diagnostics = append(diagnostics, fmt.Sprintf("%s: gap has no tracking issue", entry.site))
			}
		case "credential", "external write", "approval":
			if entry.substitute == "" {
				diagnostics = append(diagnostics, fmt.Sprintf("%s: %s has no substitute", entry.site, entry.category))
			}
		default:
			diagnostics = append(diagnostics, fmt.Sprintf("%s: unknown allowlist category %q", entry.site, entry.category))
		}
		for _, substitute := range strings.Split(entry.substitute, ", ") {
			if substitute != "" && substitute != releaseRehearsal && !tests[substitute] && !merge.jobs[substitute] {
				diagnostics = append(diagnostics, fmt.Sprintf("%s: substitute %q does not name a test or a reached CI job", entry.site, substitute))
			}
		}
		if len(entry.obligations) == 0 {
			diagnostics = append(diagnostics, fmt.Sprintf("%s: allowlist entry has no obligations", entry.site))
		}
		for _, obligation := range entry.obligations {
			field, source, _ := strings.Cut(obligation, " ")
			key := modeSite{entry.site, field, source}
			if _, used := findings[key]; !used {
				diagnostics = append(diagnostics, fmt.Sprintf("%s: unused allowlist entry for %s; remove the entry or restore its tracked obligation", entry.site, obligation))
			}
			delete(findings, key)
		}
	}
	for _, problem := range findings {
		diagnostics = append(diagnostics, problem.String())
	}
	slices.Sort(diagnostics)
	return diagnostics
}

func workflowTestNames(root string) (map[string]bool, error) {
	names := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() && path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "vendor") {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Body != nil && fn.Type.Results == nil && strings.HasPrefix(fn.Name.Name, "Test") &&
				len(fn.Type.Params.List) == 1 && len(fn.Type.Params.List[0].Names) <= 1 {
				param, _ := fn.Type.Params.List[0].Type.(*ast.StarExpr)
				if param != nil {
					if selector, ok := param.X.(*ast.SelectorExpr); ok && selector.Sel.Name == "T" {
						if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "testing" {
							names[fn.Name.Name] = true
						}
					}
				}
			}
		}
		return nil
	})
	return names, err
}

func checkCIResultCoverage(t *testing.T, root string, merge *workflowTrace) {
	t.Helper()
	workflow, _ := readWorkflowAt(t, root, ciWorkflowPath)
	result := workflow.Jobs["result"]
	for site := range merge.nonBlocking {
		t.Errorf("%s can fail without failing CI result; make the job's failure block the check", site)
	}
	for job := range merge.jobs {
		if job != "result" && !result.needs(job) {
			t.Errorf("%s / %s / (job) can escape the required check; require its success in CI result", ciWorkflowPath, job)
		}
	}
	if len(result.Steps) == 0 {
		t.Fatalf("%s / result / (job) must check the outcomes of planned jobs", ciWorkflowPath)
	}
	gate := result.Steps[0]
	gateSite := workflowSite{ciWorkflowPath, "result", stepLabel(gate, 1)}
	if gate.ContinueOnError || gate.Env["NEEDS"] != "${{ toJSON(needs) }}" {
		t.Errorf("%s must check every dependency's result and propagate failure", gateSite)
	}
	for index, ctx := range ciCoverageRuns() {
		event := ctx["github.event_name"]
		run := merge.rootRuns[index]
		if !run["result"] {
			t.Errorf("%s / result / (job) is not reached on %s; run the required CI result on this path", ciWorkflowPath, event)
			continue
		}
		if !conditionHolds(t, gate.If, ctx) {
			t.Errorf("%s is skipped on %s with successful dependencies; run the result check", gateSite, event)
		}
		outputs := map[string]string{}
		for key, value := range ctx {
			if name, ok := strings.CutPrefix(key, "needs.plan.outputs."); ok {
				outputs[name] = value
			}
		}
		type resultData struct {
			Result  string            `json:"result"`
			Outputs map[string]string `json:"outputs,omitempty"`
		}
		needs := map[string]resultData{}
		for _, dep := range result.dependencies() {
			needs[dep.Value] = resultData{Result: "skipped"}
			if run[dep.Value] {
				needs[dep.Value] = resultData{Result: "success"}
			}
		}
		withResult := func(job, conclusion string) ([]byte, error) {
			payload := maps.Clone(needs)
			if job != "" {
				payload[job] = resultData{Result: conclusion}
			}
			plan := payload["plan"]
			plan.Outputs = outputs
			payload["plan"] = plan
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			return runCIResult(result.Steps[0].Run, data)
		}
		if output, err := withResult("", ""); err != nil {
			t.Errorf("%s / result / %s rejects successful %s jobs: %v: %s", ciWorkflowPath, stepLabel(result.Steps[0], 1), event, err, output)
			continue
		}
		for job := range run {
			if job == "result" || !result.needs(job) {
				continue
			}
			for _, conclusion := range []string{"failure", "cancelled", "skipped"} {
				failed := maps.Clone(ctx)
				failed["needs."+job+".result"] = conclusion
				gateRun := workflowJobRuns(t, workflow, failed)["result"]
				gateRun.context["status"] = "success"
				if !gateRun.reached || !conditionHolds(t, gate.If, gateRun.context) {
					t.Errorf("%s can skip when %s reports %s on %s; run the result check despite dependency failures", gateSite, job, conclusion, event)
					continue
				}
				if _, err := withResult(job, conclusion); err == nil {
					t.Errorf("%s / %s / (job) can report %s on %s while CI passes; require its success in CI result", ciWorkflowPath, job, conclusion, event)
				}
			}
		}
	}
}
