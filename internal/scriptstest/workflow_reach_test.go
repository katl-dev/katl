package scriptstest

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflowInput struct {
	Type     string
	Default  string
	Required bool
}

type workflowTrigger struct {
	Inputs         map[string]workflowInput
	Branches       []string
	BranchesIgnore []string `yaml:"branches-ignore"`
}

func workflowTriggers(t *testing.T, on yaml.Node) map[string]workflowTrigger {
	t.Helper()
	triggers := map[string]workflowTrigger{}
	switch on.Kind {
	case 0:
	case yaml.ScalarNode:
		triggers[on.Value] = workflowTrigger{}
	case yaml.SequenceNode:
		for _, event := range on.Content {
			triggers[event.Value] = workflowTrigger{}
		}
	case yaml.MappingNode:
		for i := 0; i < len(on.Content); i += 2 {
			var trigger workflowTrigger
			name, node := on.Content[i].Value, on.Content[i+1]
			if node.Kind == yaml.MappingNode {
				if err := node.Decode(&trigger); err != nil {
					t.Fatalf("decode trigger %s: %v", name, err)
				}
			}
			triggers[name] = trigger
		}
	}
	return triggers
}

type workflowSite struct {
	file string
	job  string
	step string
}

func (s workflowSite) String() string {
	return s.file + " / " + s.job + " / " + s.step
}

type workflowVisit struct {
	site  workflowSite
	owner string
	with  map[string]string
	env   map[string]string
	call  bool
}

type modeSite struct {
	workflowSite
	field  string
	source string
}

type workflowTrace struct {
	visits []workflowVisit
	mode   map[modeSite]map[string]bool
	probes map[modeSite]map[string]bool
	// Conditions are inspected even for skipped jobs and steps within a
	// visited workflow, so a tag-only step cannot disappear from both sets.
	guards        map[modeSite]bool
	triggers      map[string]map[string]workflowTrigger
	used          map[string]map[string]bool
	reads         map[string]map[string]bool
	jobs          map[string]bool
	rootRuns      []map[string]bool
	nonBlocking   map[workflowSite]bool
	activeJobs    map[workflowSite]bool
	inactiveSteps map[workflowSite]bool
	fixtures      map[string]map[string]string
	// Every run revisits the same files, so a defect in one is reported once.
	reported map[string]bool
}

func traceWorkflows(t *testing.T, root, file string, runs []map[string]string, fixtures map[string]map[string]string) *workflowTrace {
	t.Helper()
	trace := &workflowTrace{
		mode: map[modeSite]map[string]bool{}, probes: map[modeSite]map[string]bool{},
		guards: map[modeSite]bool{}, triggers: map[string]map[string]workflowTrigger{},
		used: map[string]map[string]bool{}, reads: map[string]map[string]bool{}, jobs: map[string]bool{},
		fixtures:    fixtures,
		reported:    map[string]bool{},
		nonBlocking: map[workflowSite]bool{},
		activeJobs:  map[workflowSite]bool{}, inactiveSteps: map[workflowSite]bool{},
	}
	for _, run := range runs {
		trace.rootRuns = append(trace.rootRuns, map[string]bool{})
		trace.workflow(t, root, file, run["github.event_name"], nil, run, nil)
	}
	return trace
}

func (trace *workflowTrace) errorf(t *testing.T, format string, args ...any) {
	t.Helper()
	message := fmt.Sprintf(format, args...)
	if !trace.reported[message] {
		trace.reported[message] = true
		t.Error(message)
	}
}

func (trace *workflowTrace) workflow(t *testing.T, root, file, event string, supplied map[string]string, parent map[string]string, stack []string) {
	t.Helper()
	if slices.Contains(stack, file) {
		t.Fatalf("recursive local workflow: %v -> %s", stack, file)
	}
	workflow, source := readWorkflowAt(t, root, file)
	triggers := workflowTriggers(t, workflow.On)
	trace.triggers[file] = triggers
	trigger, ok := triggers[event]
	if !ok {
		trace.errorf(t, "%s / on / %s is missing; restore the trigger for this path", file, event)
		return
	}
	if trace.used[file] == nil {
		trace.used[file] = map[string]bool{}
		trace.reads[file] = workflowInputReads(t, source)
	}
	trace.used[file][event] = true
	ctx := map[string]string{"static-workflow": "true", "workflow-file": file, "expression-location": file + " / (workflow) / (workflow): "}
	for key, value := range parent {
		if len(stack) == 0 || strings.HasPrefix(key, "github.") || strings.HasPrefix(key, "runner.") {
			ctx[key] = value
		}
	}
	maps.Copy(ctx, trace.fixtures[file])
	dispatchInputs := map[string]string{}
	if event == "workflow_dispatch" {
		for key, value := range ctx {
			if name, ok := strings.CutPrefix(key, "inputs."); ok {
				dispatchInputs[name] = value
			}
		}
		supplied = dispatchInputs
	}
	ctx = trace.bindWorkflowInputs(t, file, ctx, trigger.Inputs, supplied)
	for name, value := range trace.fixtures[file] {
		ctx[name] = expandExpressions(t, value, ctx)
	}
	if len(stack) == 0 {
		ctx["github.workflow"] = workflow.Name
		branch := ctx["github.base_ref"]
		if branch == "" {
			branch = strings.TrimPrefix(ctx["github.ref"], "refs/heads/")
		}
		if !workflowBranchMatches(t, file, branch, trigger) {
			trace.errorf(t, "%s / on / %s excludes %s; make the trigger reach this path", file, event, branch)
			return
		}
	}
	trace.modeFields(t, workflowSite{file, "(workflow)", "(workflow)"}, ctx, nil, workflow.Env, workflow.Concurrency, "")
	ctx = workflowEnv(t, ctx, workflow.Env)

	runs := workflowJobRuns(t, workflow, ctx)
	for _, name := range slices.Sorted(maps.Keys(runs)) {
		job := workflow.Jobs[name]
		site := workflowSite{file, name, "(job)"}
		run := runs[name]
		trace.guard(t, site, run.context, job.If, run.reached)
		if !run.reached {
			if job.Uses != "" {
				trace.inactiveSteps[site] = true
			}
			labels := stepLabels(job.Steps)
			for index, step := range job.Steps {
				stepSite := workflowSite{file, name, labels[index]}
				trace.inactiveSteps[stepSite] = true
				// Prerelease selection also exists in the legacy tag graph.
				// Its version contract survives independently of the trigger.
				if strings.Contains(step.Run, "--prerelease") {
					trace.observe(modeSite{stepSite, "version", ""}, "prerelease")
				}
			}
			continue
		}
		for _, lane := range workflowMatrix(t, site, job.Strategy.Matrix, run.context) {
			trace.activeJobs[site] = true
			if len(stack) == 0 {
				trace.jobs[name] = true
				trace.rootRuns[len(trace.rootRuns)-1][name] = true
			}
			if job.ContinueOnError {
				trace.nonBlocking[site] = true
			}
			trace.modeFields(t, site, lane, job.With, job.Env, job.Concurrency, "")
			if !job.Environment.IsZero() {
				trace.visits = append(trace.visits, workflowVisit{site: workflowSite{file, name, "(environment)"}, owner: "environment:" + job.Environment.Value})
			}
			if job.Uses != "" {
				with := resolveWorkflowValues(t, lane, job.With)
				var target string
				if strings.HasPrefix(job.Uses, "./") {
					target = localWorkflowPath(t, site, job.Uses)
					child, _ := readWorkflowAt(t, root, target)
					with = trace.workflowInputValues(t, target, workflowTriggers(t, child.On)["workflow_call"].Inputs, with)
					trace.workflow(t, root, target, "workflow_call", with, lane, append(stack, file))
				}
				trace.visits = append(trace.visits, workflowVisit{site: site, owner: job.Uses, with: with, call: target != ""})
			} else {
				jobEnv := resolveWorkflowValues(t, maskValueInputs(lane), job.Env)
				trace.steps(t, root, file, name, job.Steps, workflowEnv(t, lane, job.Env), jobEnv, nil)
			}
		}
	}
}

func workflowBranchMatches(t *testing.T, file, branch string, trigger workflowTrigger) bool {
	t.Helper()
	matches := func(pattern string) bool {
		// Current filters need only the subset shared by path.Match and Actions.
		if strings.ContainsAny(pattern, "+!\\") || strings.Contains(pattern, "**") {
			t.Fatalf("%s has a branch filter the reachability test cannot evaluate: %s", file, pattern)
		}
		matched, err := path.Match(pattern, branch)
		if err != nil {
			t.Fatalf("%s branch filter %s: %v", file, pattern, err)
		}
		return matched
	}
	return (len(trigger.Branches) == 0 || slices.ContainsFunc(trigger.Branches, matches)) &&
		!slices.ContainsFunc(trigger.BranchesIgnore, matches)
}

func workflowInputReads(t *testing.T, source string) map[string]bool {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(source), &document); err != nil {
		t.Fatal(err)
	}
	reads := map[string]bool{}
	record := func(source string) {
		for _, ref := range modeReferences(source) {
			if input, ok := strings.CutPrefix(ref, "inputs."); ok {
				reads[input] = true
			}
		}
	}
	var walk func(*yaml.Node)
	walk = func(node *yaml.Node) {
		if node.Kind == yaml.ScalarNode {
			for _, expr := range expressionPattern.FindAllString(node.Value, -1) {
				record(expr)
			}
		}
		if node.Kind == yaml.MappingNode {
			for i := 0; i < len(node.Content); i += 2 {
				if node.Content[i].Value == "if" {
					record(node.Content[i+1].Value)
				}
			}
		}
		for _, child := range node.Content {
			walk(child)
		}
	}
	if len(document.Content) > 0 {
		fields := document.Content[0].Content
		for i := 1; i < len(fields); i += 2 {
			if fields[i-1].Value != "on" {
				walk(fields[i])
			}
		}
	}
	return reads
}

func (trace *workflowTrace) workflowInputValues(t *testing.T, file string, declarations map[string]workflowInput, supplied map[string]string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for name, input := range declarations {
		value, set := supplied[name]
		if !set {
			value = input.Default
			if input.Required && value == "" {
				trace.errorf(t, "%s / on / input %s is required but this path does not supply it", file, name)
			}
			if value == "" {
				switch input.Type {
				case "boolean":
					value = "false"
				case "number":
					value = "0"
				}
			}
		}
		values[name] = value
	}
	for name := range supplied {
		if _, ok := declarations[name]; !ok {
			trace.errorf(t, "%s / on / input %s is supplied but not declared", file, name)
		}
	}
	return values
}

func (trace *workflowTrace) bindWorkflowInputs(t *testing.T, file string, parent map[string]string, declarations map[string]workflowInput, supplied map[string]string) map[string]string {
	t.Helper()
	ctx := maps.Clone(parent)
	for key := range ctx {
		if strings.HasPrefix(key, "inputs.") || strings.HasPrefix(key, "type:inputs.") {
			delete(ctx, key)
		}
	}
	for name, value := range trace.workflowInputValues(t, file, declarations, supplied) {
		ctx["inputs."+name] = value
		ctx["type:inputs."+name] = declarations[name].Type
	}
	return ctx
}

func resolveWorkflowValues(t *testing.T, ctx, fields map[string]string) map[string]string {
	t.Helper()
	resolved := make(map[string]string, len(fields))
	for name, value := range fields {
		resolved[name] = expandExpressions(t, value, ctx)
	}
	return resolved
}

// maskValueInputs replaces each set caller value with a placeholder. Such a
// value changes on every commit or retention setting, but its presence can
// still select a mode.
func maskValueInputs(ctx map[string]string) map[string]string {
	masked := maps.Clone(ctx)
	for key, value := range ctx {
		if name, ok := strings.CutPrefix(key, "inputs."); ok && workflowValueInput(name) && value != "" {
			masked[key] = "(set)"
		}
	}
	return masked
}

func workflowEnv(t *testing.T, ctx, env map[string]string) map[string]string {
	t.Helper()
	layer := maps.Clone(ctx)
	for key, value := range resolveWorkflowValues(t, ctx, env) {
		layer["env."+strings.ToLower(key)] = value
	}
	return layer
}

func workflowMatrix(t *testing.T, site workflowSite, matrix yaml.Node, ctx map[string]string) []map[string]string {
	t.Helper()
	if matrix.IsZero() {
		return []map[string]string{ctx}
	}
	// The workflows under test use axes, include-only matrices, or both.
	// A new matrix shape must be modeled before it supplies coverage.
	var axes map[string]json.RawMessage
	if matrix.Kind == yaml.ScalarNode {
		value := expandExpressions(t, matrix.Value, ctx)
		if err := json.Unmarshal([]byte(value), &axes); err != nil || axes == nil {
			t.Fatalf("%s cannot expand matrix %q: %v", site, value, err)
		}
	} else {
		for i := 0; i < len(matrix.Content); i += 2 {
			key, axis := matrix.Content[i].Value, matrix.Content[i+1]
			if axes == nil {
				axes = map[string]json.RawMessage{}
			}
			if axis.Kind == yaml.ScalarNode {
				axes[key] = json.RawMessage(expandExpressions(t, axis.Value, ctx))
			} else {
				var values any
				if err := axis.Decode(&values); err != nil {
					t.Fatalf("%s matrix.%s: %v", site, key, err)
				}
				axes[key], _ = json.Marshal(values)
			}
		}
	}
	if include, ok := axes["include"]; ok {
		var rows []map[string]any
		if len(axes) != 1 || json.Unmarshal(include, &rows) != nil {
			t.Fatalf("%s has an unsupported mixed matrix", site)
		}
		lanes := make([]map[string]string, 0, len(rows))
		for _, row := range rows {
			lane := maps.Clone(ctx)
			for key, value := range row {
				lane["matrix."+key] = fmt.Sprint(value)
			}
			lanes = append(lanes, lane)
		}
		return lanes
	}
	lanes := []map[string]string{ctx}
	for _, name := range slices.Sorted(maps.Keys(axes)) {
		var values []any
		if name == "exclude" || json.Unmarshal(axes[name], &values) != nil {
			t.Fatalf("%s has an unsupported matrix axis %s: %s", site, name, axes[name])
		}
		var next []map[string]string
		for _, lane := range lanes {
			for _, value := range values {
				row := maps.Clone(lane)
				row["matrix."+name] = fmt.Sprint(value)
				next = append(next, row)
			}
		}
		lanes = next
	}
	return lanes
}

var (
	// Every context that differs between a release dispatch and a pre-merge
	// run can select a mode: the event and its payload, the refs, the actors
	// and the workflow name.
	modeRef      = regexp.MustCompile(`(?i)'(?:[^']|'')*'|\b(?:github(?:\s*\.\s*|\s*\[\s*')(?:event_name|ref_type|ref_name|ref|base_ref|head_ref|triggering_actor|actor|workflow)\b(?:'\s*\])?|github(?:\s*\.\s*|\s*\[\s*')event(?:'\s*\])?(?:\s*\.\s*[\w-]+|\s*\[\s*'[\w-]+'\s*\])+|inputs(?:\s*\.\s*[\w-]+|\s*\[\s*'[\w-]+'\s*\]))`)
	shellModeRef = regexp.MustCompile(`\b(GITHUB_(?:EVENT_NAME|EVENT_PATH|REF[A-Z_0-9]*|BASE_REF|HEAD_REF|TRIGGERING_ACTOR|ACTOR|WORKFLOW))\b`)
	modeLiteral  = regexp.MustCompile(`'(?:[^']|'')*'`)
)

func modeReferences(source string) []string {
	var refs []string
	for _, match := range modeRef.FindAllString(source, -1) {
		if !strings.HasPrefix(match, "'") {
			ref := strings.ToLower(strings.NewReplacer("['", ".", "']", "", " ", "", "\t", "").Replace(match))
			if strings.HasPrefix(ref, "github.event.inputs.") {
				ref = strings.TrimPrefix(ref, "github.event.")
			}
			refs = append(refs, ref)
		}
	}
	return refs
}

// modeKey names a condition or field by the mode references it reads, so its
// obligation does not depend on the surrounding expression text.
func modeKey(source string) string {
	refs := modeReferences(source)
	slices.Sort(refs)
	return strings.Join(slices.Compact(refs), " ")
}

func (trace *workflowTrace) guard(t *testing.T, site workflowSite, ctx map[string]string, condition string, reached bool) {
	t.Helper()
	if refs := modeKey(condition); refs != "" {
		key := modeSite{site, "if", refs}
		trace.guards[key] = true
		trace.observe(key, fmt.Sprint(reached))
		for _, probe := range modeProbes(condition, ctx) {
			if conditionRuns(t, condition, ctx) != conditionRuns(t, condition, probe) {
				trace.probe(key, fmt.Sprint(conditionRuns(t, condition, probe)))
			}
		}
	}
}

// Probe direct selectors for latent branches, without crediting a hypothetical
// run as coverage. For example an actor exclusion can be true in every sample
// even though the literal actor changes which steps run.
func modeProbes(source string, ctx map[string]string) []map[string]string {
	var probes []map[string]string
	refs := modeReferences(source)
	literals := modeLiteral.FindAllString(source, -1)
	for _, ref := range refs {
		switch ref {
		case "github.event_name", "github.ref_type", "github.ref":
			for _, event := range [][3]string{
				{"push", "refs/tags/v2026.12.1", "tag"},
				{"schedule", "refs/heads/main", "branch"},
			} {
				probe := maps.Clone(ctx)
				probe["github.event_name"], probe["github.ref"], probe["github.ref_type"] = event[0], event[1], event[2]
				probes = append(probes, probe)
			}
		}
	}
	if slices.Contains(refs, "github.ref") {
		for _, literal := range literals {
			ref := literal[1 : len(literal)-1]
			if strings.HasPrefix(ref, "refs/") {
				probe := maps.Clone(ctx)
				probe["github.ref"] = ref
				if strings.HasPrefix(ref, "refs/tags/") {
					probe["github.event_name"], probe["github.ref_type"] = "push", "tag"
				}
				probes = append(probes, probe)
			}
		}
	}
	if slices.Contains(refs, "github.event_name") {
		for _, literal := range literals {
			probe := maps.Clone(ctx)
			probe["github.event_name"] = literal[1 : len(literal)-1]
			probes = append(probes, probe)
		}
	}
	for _, actor := range []string{"github.actor", "github.triggering_actor"} {
		if slices.Contains(refs, actor) {
			for _, base := range append(slices.Clone(probes), ctx) {
				for _, literal := range literals {
					probe := maps.Clone(base)
					probe[actor] = strings.ReplaceAll(literal[1:len(literal)-1], "''", "'")
					probes = append(probes, probe)
				}
			}
		}
	}
	return probes
}

func (trace *workflowTrace) observe(site modeSite, outcome string) {
	if trace.mode[site] == nil {
		trace.mode[site] = map[string]bool{}
	}
	trace.mode[site][outcome] = true
}

func (trace *workflowTrace) probe(site modeSite, outcome string) {
	if trace.probes[site] == nil {
		trace.probes[site] = map[string]bool{}
	}
	trace.probes[site][outcome] = true
}

func (trace *workflowTrace) modeFields(t *testing.T, site workflowSite, ctx, with, env map[string]string, concurrency yaml.Node, run string) {
	t.Helper()
	check := func(field, value string) {
		for _, expression := range expressionPattern.FindAllStringSubmatch(value, -1) {
			match, body := expression[0], strings.TrimSpace(expression[1])
			if refs := modeReferences(body); len(refs) != 0 {
				key := modeSite{site, field, match}
				outcome := expandExpressions(t, match, ctx)
				// A caller value changes on every commit or retention
				// setting. Presence can still select a mode.
				if name, ok := strings.CutPrefix(body, "inputs."); ok && workflowValueInput(name) && outcome != "" {
					outcome = "(set)"
				}
				trace.observe(key, outcome)
				// A bare reference already exposes its concrete value. Probe
				// expressions that can choose a different outcome.
				if strings.ContainsAny(match, "=!&|(") {
					for _, probe := range modeProbes(match, ctx) {
						if alternative := expandExpressions(t, match, probe); alternative != outcome {
							trace.probe(key, alternative)
						}
					}
				}
			}
		}
	}
	for name, value := range with {
		check("with."+name, value)
	}
	for name, value := range env {
		check("env."+name, value)
	}
	// A concurrency value is one outcome however many expressions build it.
	group := func(field, value string) {
		expressions := strings.Join(expressionPattern.FindAllString(value, -1), " ")
		refs := modeKey(expressions)
		if refs == "" {
			return
		}
		key := modeSite{site, field, refs}
		masked := maskValueInputs(ctx)
		outcome := expandExpressions(t, value, masked)
		trace.observe(key, outcome)
		if strings.ContainsAny(expressions, "=!&|(") {
			for _, probe := range modeProbes(expressions, masked) {
				if alternative := expandExpressions(t, value, probe); alternative != outcome {
					trace.probe(key, alternative)
				}
			}
		}
	}
	if concurrency.Kind == yaml.ScalarNode {
		group("concurrency", concurrency.Value)
	}
	for i := 0; i < len(concurrency.Content); i += 2 {
		group("concurrency."+concurrency.Content[i].Value, concurrency.Content[i+1].Value)
	}
	check("run-interpolation", run)
	// The shell is never executed by this test. Mode reads in inline shell
	// need a substitute unless the read is a plain owner-command argument.
	single := ownerCommand(expandExpressions(t, run, ctx)) != ""
	for _, line := range strings.Split(run, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, match := range shellModeRef.FindAllStringSubmatch(line, -1) {
			key := modeSite{site, "run-reference", match[1]}
			if single {
				trace.observe(key, ctx["github."+strings.ToLower(strings.TrimPrefix(match[1], "GITHUB_"))])
			} else {
				trace.observe(key, "inline shell")
			}
		}
	}
}

var versionCommand = regexp.MustCompile(`\bkatl-release(?:-artifacts[ \t]+(?:stage|notes)|[ \t]+bundle-name)\b|--prerelease\b`)

// steps traces a job's steps. jobEnv is the job's resolved env, which every
// step's visit carries with its own.
func (trace *workflowTrace) steps(t *testing.T, root, file, job string, steps []workflowStep, ctx, jobEnv map[string]string, stack []string) {
	t.Helper()
	ctx = maps.Clone(ctx)
	ctx["status"] = "success"
	seen := map[string]bool{}
	labels := stepLabels(steps)
	for index, step := range steps {
		label := labels[index]
		site := workflowSite{file, job, label}
		ctx["expression-location"] = site.String() + ": "
		if seen[label] {
			t.Fatalf("%s has an ambiguous step identity; give the steps distinct names or IDs", site)
		}
		seen[label] = true
		reached := conditionRuns(t, step.If, ctx)
		trace.guard(t, site, ctx, step.If, reached)
		if step.ID != "" {
			key := "steps." + step.ID + ".outcome"
			if reached {
				if ctx[key] == "" {
					ctx[key] = "success"
				}
			} else {
				ctx[key] = "skipped"
				dropOutputs(ctx, "steps."+step.ID+".outputs.")
			}
			if ctx[key] == "failure" && !step.ContinueOnError {
				ctx["status"] = "failure"
			}
		}
		if !reached {
			continue
		}
		trace.modeFields(t, site, ctx, step.With, step.Env, yaml.Node{}, step.Run)
		for _, command := range versionCommand.FindAllString(step.Run, -1) {
			trace.observe(modeSite{site, "version", ""}, command)
		}
		owner := step.Uses
		if owner == "" {
			owner = ownerCommand(expandExpressions(t, step.Run, ctx))
			if owner == "" {
				trace.observe(modeSite{site, "shell", ""}, "inline shell")
			}
		}
		with := resolveWorkflowValues(t, ctx, step.With)
		env := maps.Clone(jobEnv)
		if env == nil {
			env = map[string]string{}
		}
		maps.Copy(env, resolveWorkflowValues(t, maskValueInputs(ctx), step.Env))
		if strings.HasPrefix(step.Uses, "./") {
			target := localWorkflowPath(t, site, step.Uses)
			// The hosted VM's explicit-ref path checks out these actions into
			// a second directory. It still uses the current workflow revision.
			if name, ok := localAction(step.Uses); ok {
				target = filepath.Join(".github", "actions", name)
			}
			owner = "./" + filepath.ToSlash(target)
			if slices.Contains(stack, target) {
				t.Fatalf("%s recursively uses %s", site, target)
			}
			file, action := readComposite(t, root, target)
			with = trace.workflowInputValues(t, file, action.Inputs, with)
			child := trace.bindWorkflowInputs(t, file, workflowEnv(t, ctx, step.Env), action.Inputs, with)
			dropOutputs(child, "steps.")
			dropOutputs(child, "needs.")
			trace.steps(t, root, file, "(composite)", action.Runs.Steps, child, nil, append(stack, target))
		}
		trace.visits = append(trace.visits, workflowVisit{site: site, owner: owner, with: with, env: env})
	}
}

func dropOutputs(ctx map[string]string, prefix string) {
	for key := range ctx {
		if strings.HasPrefix(key, prefix) {
			delete(ctx, key)
		}
	}
}

func localWorkflowPath(t *testing.T, site workflowSite, uses string) string {
	t.Helper()
	path := filepath.Clean(uses)
	if !filepath.IsLocal(path) {
		t.Fatalf("%s uses a path outside the repository: %s", site, uses)
	}
	return path
}

type workflowComposite struct {
	Inputs map[string]workflowInput
	Runs   struct {
		Using string
		Steps []workflowStep
	}
}

func readComposite(t *testing.T, root, target string) (string, workflowComposite) {
	t.Helper()
	for _, name := range []string{"action.yml", "action.yaml"} {
		file := filepath.Join(target, name)
		data, err := os.ReadFile(filepath.Join(root, file))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var action workflowComposite
		if err := yaml.Unmarshal(data, &action); err != nil || action.Runs.Using != "composite" {
			t.Fatalf("%s must be a local composite action: %v", file, err)
		}
		return file, action
	}
	t.Fatalf("%s is missing action.yml or action.yaml", target)
	return "", workflowComposite{}
}

// Recognize only a simple command with literal arguments and parameter
// expansions after Actions interpolates the run body. Anything requiring shell grammar
// (including substitutions within double quotes) needs an explicit gap.
var ownerWord = regexp.MustCompile(`^(?:[-./\w:@%+=,~^]+(?:#[\w-]+)?|'[^'\n]*'|"(?:[^"\x60$\\\n]|\\["\\$]|\$[A-Za-z_]\w*|\$\{[A-Za-z_]\w*(?::[0-9]+){0,2}\})*"|\$[A-Za-z_]\w*|\$\{[A-Za-z_]\w*(?::[0-9]+){0,2}\})+`)

func ownerCommand(run string) string {
	run = strings.ReplaceAll(run, "\\\n", "")
	var argv []string
	for rest := strings.TrimSpace(run); rest != ""; rest = strings.TrimLeft(rest, " \t") {
		word := ownerWord.FindString(rest)
		if word == "" || len(word) < len(rest) && !strings.ContainsRune(" \t", rune(rest[len(word)])) {
			return ""
		}
		argv = append(argv, word)
		rest = rest[len(word):]
	}
	command := argv
	if len(command) > 2 && command[0] == "nix" && command[1] == "develop" {
		if index := slices.Index(command, "-c"); index >= 0 {
			command = command[index+1:]
		}
	}
	if len(command) == 0 {
		return ""
	}
	entry := strings.TrimPrefix(command[0], "./")
	switch {
	case strings.HasPrefix(entry, "scripts/"), strings.HasPrefix(entry, ".github/scripts/"):
	case len(command) > 2 && command[0] == "go" && command[1] == "run" && strings.HasPrefix(command[2], "./cmd/"):
	case len(command) > 2 && command[0] == "go" && command[1] == "test":
	default:
		return ""
	}
	return strings.Join(argv, " ")
}
