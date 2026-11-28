package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
	"gopkg.in/yaml.v3"
)

type workflowStep struct {
	ID              string            `yaml:"id"`
	Name            string            `yaml:"name"`
	If              string            `yaml:"if"`
	Run             string            `yaml:"run"`
	Uses            string            `yaml:"uses"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	Env             map[string]string `yaml:"env"`
	With            map[string]string `yaml:"with"`
}

type workflowJob struct {
	Name            string            `yaml:"name"`
	Environment     yaml.Node         `yaml:"environment"`
	If              string            `yaml:"if"`
	Needs           yaml.Node         `yaml:"needs"`
	Concurrency     yaml.Node         `yaml:"concurrency"`
	Uses            string            `yaml:"uses"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	Permissions     yaml.Node         `yaml:"permissions"`
	Outputs         map[string]string `yaml:"outputs"`
	Env             map[string]string `yaml:"env"`
	With            map[string]string `yaml:"with"`
	Strategy        struct {
		Matrix yaml.Node `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []workflowStep `yaml:"steps"`
}

type workflowFile struct {
	Name        string                 `yaml:"name"`
	RunName     string                 `yaml:"run-name"`
	On          yaml.Node              `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Concurrency yaml.Node              `yaml:"concurrency"`
	Env         map[string]string      `yaml:"env"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) (workflowFile, string) {
	t.Helper()
	return readWorkflowAt(t, repoRoot(t), filepath.Join(".github", "workflows", name))
}

func readWorkflowAt(t *testing.T, root, name string) (workflowFile, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	var workflow workflowFile
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return workflow, string(data)
}

func (job workflowJob) needs(dependency string) bool {
	for _, node := range job.dependencies() {
		if node.Value == dependency {
			return true
		}
	}
	return false
}

func (job workflowJob) dependencies() []*yaml.Node {
	if job.Needs.Kind == yaml.ScalarNode {
		return []*yaml.Node{&job.Needs}
	}
	return job.Needs.Content
}

func requireNeeds(t *testing.T, workflow workflowFile, edges [][2]string) {
	t.Helper()
	for _, edge := range edges {
		if !workflow.Jobs[edge[0]].needs(edge[1]) {
			t.Errorf("%s can run before %s succeeds", edge[0], edge[1])
		}
	}
}

const (
	productionBuild    = "production-build.yml"
	runtimeInputsName  = "katl-runtime-${{ inputs.flavour }}-${{ github.sha }}"
	bundlesArtifact    = "${{ steps.names.outputs.extension-bundles }}"
	extensionsMatrixFn = "${{ fromJSON(needs.runtime.outputs.extensions) }}"
)

func TestProductionBuildGraph(t *testing.T) {
	workflow, _ := readWorkflow(t, productionBuild)

	for _, job := range []string{"runtime", "installer", "extensions", "assemble"} {
		if _, ok := workflow.Jobs[job]; !ok {
			t.Fatalf("production build has no %s job", job)
		}
	}
	requireNeeds(t, workflow, [][2]string{
		{"assemble", "extensions"},
		{"assemble", "installer"},
		{"assemble", "runtime"},
		{"extensions", "runtime"},
	})
	if workflow.Jobs["installer"].needs("runtime") {
		t.Error("installer build must not wait for the runtime build")
	}
	if got := workflow.Env["KATL_FLAVOUR"]; got != "${{ inputs.flavour }}" {
		t.Errorf("KATL_FLAVOUR = %q, want the flavour input", got)
	}
	for name, job := range workflow.Jobs {
		if _, ok := matrixAxes(t, job)["flavour"]; ok {
			t.Errorf("%s matrixes over flavour; callers choose the flavour", name)
		}
	}
}

func TestProductionBuildRuntimeHandoff(t *testing.T) {
	workflow, _ := readWorkflow(t, productionBuild)

	runtimeStep, dependencyStep, uploadStep := -1, -1, -1
	for index, step := range workflow.Jobs["runtime"].Steps {
		if strings.Contains(step.Run, "scripts/mkosi build-runtime") {
			runtimeStep = index
		}
		if strings.Contains(step.Run, "scripts/build-endpoint-advertiser-sysext") {
			dependencyStep = index
		}
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && step.With["name"] == runtimeInputsName {
			if strings.Contains(step.With["path"], "_build/mkosi/katl-endpoint-advertiser.raw*") {
				uploadStep = index
			}
			if !strings.Contains(step.With["path"], "_build/mkosi/katl-kernel-inputs.tar*") {
				t.Error("runtime handoff omits prepared kernel inputs and their integrity metadata")
			}
		}
	}
	if runtimeStep < 0 || dependencyStep <= runtimeStep || uploadStep <= dependencyStep {
		t.Error("runtime job must build and transport the endpoint advertiser while its runtime base tree is available")
	}

	for _, consumer := range []string{"extensions", "assemble"} {
		received := false
		for _, step := range workflow.Jobs[consumer].Steps {
			received = received || (strings.HasPrefix(step.Uses, "actions/download-artifact@") && step.With["name"] == runtimeInputsName)
		}
		if !received {
			t.Errorf("%s job must consume its runtime's build inputs", consumer)
		}
	}
	for _, step := range workflow.Jobs["assemble"].Steps {
		if strings.Contains(step.Run, "scripts/build-endpoint-advertiser-sysext") {
			t.Error("packaging must not require the runtime builder's private base tree")
		}
	}
}

func TestProductionBuildExtensionMatrix(t *testing.T) {
	workflow, _ := readWorkflow(t, productionBuild)

	matrix := matrixAxes(t, workflow.Jobs["extensions"])
	if len(matrix) != 1 || matrix["extension"].Value != extensionsMatrixFn {
		t.Error("extension matrix must come from the runtime job's recipe inventory and nothing else")
	}
}

func matrixAxes(t *testing.T, job workflowJob) map[string]yaml.Node {
	t.Helper()
	var axes map[string]yaml.Node
	if job.Strategy.Matrix.Kind != 0 {
		if err := job.Strategy.Matrix.Decode(&axes); err != nil {
			t.Fatal(err)
		}
	}
	return axes
}

func TestProductionBuildEmbedsKernelExtensions(t *testing.T) {
	workflow, _ := readWorkflow(t, productionBuild)

	composition, packaging, staging := -1, -1, -1
	carried := false
	for i, step := range workflow.Jobs["assemble"].Steps {
		if strings.Contains(step.Run, "scripts/assemble-release-extensions") {
			composition = i
			if step.ContinueOnError {
				t.Error("composition failure must block packaging")
			}
		}
		if strings.Contains(step.Run, "scripts/build-katlos-install-image") {
			packaging = i
			if step.Env["KATL_EXTENSION_RELEASE"] != "_build/mkosi/release-extensions.json" {
				t.Error("packaging must consume the aggregate release mapping")
			}
		}
		if strings.Contains(step.Run, "scripts/katl-release-artifacts stage") {
			staging = i
		}
		paths := step.With["path"]
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && strings.Contains(paths, "_build/mkosi/extension-bundles/") &&
			strings.Contains(paths, "_build/mkosi/release-extensions.json") {
			carried = true
		}
	}
	if composition < 0 || packaging <= composition || staging <= packaging {
		t.Error("complete composition must be verified before packaging, and packaging before staging")
	}
	if !carried {
		t.Error("assemble loses the release mapping or extension closures for publication")
	}
}

func TestProductionBuildIsUnprivileged(t *testing.T) {
	workflow, source := readWorkflow(t, productionBuild)

	if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" {
		t.Errorf("workflow permissions = %v, want contents: read only", workflow.Permissions)
	}
	if workflow.Concurrency.Kind != 0 {
		t.Error("callers own concurrency; the reusable build must not set it")
	}
	for name, job := range workflow.Jobs {
		if job.Permissions.Kind != 0 {
			t.Errorf("%s widens permissions", name)
		}
		for _, step := range job.Steps {
			if strings.HasPrefix(step.Uses, "actions/attest") {
				t.Errorf("%s attests; attestation belongs to the release workflow", name)
			}
		}
	}
	for _, forbidden := range []string{"docker login", "secrets.", "publish-release-extensions", "gh release", "packages: write"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("production build must not contain %q", forbidden)
		}
	}
}

func TestProductionBuildArtifactRetention(t *testing.T) {
	workflow, _ := readWorkflow(t, productionBuild)
	uploads := 0
	for _, step := range workflow.Jobs["assemble"].Steps {
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			uploads++
			if got := step.With["retention-days"]; got != "${{ inputs.retention-days }}" {
				t.Errorf("%s retention-days = %q, want the caller input", step.Name, got)
			}
		}
	}
	if uploads == 0 {
		t.Fatal("assemble has no artifact uploads")
	}
}

func TestProductionBuildExtensionBundles(t *testing.T) {
	workflow, _ := readWorkflow(t, productionBuild)
	var on struct {
		Call struct {
			Outputs map[string]struct{ Value string }
		} `yaml:"workflow_call"`
	}
	if err := workflow.On.Decode(&on); err != nil {
		t.Fatal(err)
	}
	if got := on.Call.Outputs["extension-bundles"].Value; got != "${{ jobs.assemble.outputs.extension-bundles }}" {
		t.Errorf("workflow extension-bundles output = %q, want the assemble output", got)
	}

	assemble := workflow.Jobs["assemble"]
	if got := assemble.Outputs["extension-bundles"]; got != bundlesArtifact {
		t.Errorf("assemble extension-bundles output = %q, want the upload name", got)
	}
	var names workflowStep
	var upload workflowStep
	for _, step := range assemble.Steps {
		if step.ID == "names" {
			names = step
		}
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && step.With["name"] == bundlesArtifact {
			upload = step
		}
	}
	if upload.Uses == "" || upload.If != "" {
		t.Fatal("assemble must upload the named extension bundles on every successful build")
	}
	if names.If != "" || names.Run == "" {
		t.Fatal("assemble must resolve the extension bundles name on every successful build")
	}

	outputPath := filepath.Join(t.TempDir(), "output")
	command := exec.Command("bash", "-euo", "pipefail", "-c", names.Run)
	command.Env = []string{"KATL_FLAVOUR=lts", "GITHUB_SHA=0123456789abcdef0123456789abcdef01234567", "GITHUB_OUTPUT=" + outputPath}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("resolve artifact names: %v\n%s", err, output)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "\nextension-bundles=katl-extension-bundles-lts-0123456789abcdef0123456789abcdef01234567\n") {
		t.Errorf("artifact outputs = %q, want the extension bundle named for the flavour and commit", output)
	}
}

func TestReleaseBuildsBothKernelFlavours(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")
	version := regexp.MustCompile(`needs\.identity\.outputs\.version`)

	for _, flavour := range []string{"standard", "lts"} {
		build := workflow.Jobs["build-"+flavour]
		if build.Uses != "./.github/workflows/"+productionBuild {
			t.Errorf("build-%s uses %q", flavour, build.Uses)
		}
		if build.With["flavour"] != flavour || !version.MatchString(build.With["version"]) {
			t.Errorf("build-%s inputs = %v, want literal flavour %s and the release version", flavour, build.With, flavour)
		}
		if !build.needs("identity") {
			t.Errorf("build-%s does not wait for release identity", flavour)
		}

		qualification := workflow.Jobs["qualification-"+flavour]
		if qualification.With["flavour"] != flavour {
			t.Errorf("qualification-%s qualifies flavour %q", flavour, qualification.With["flavour"])
		}
		if want := "${{ needs.build-" + flavour + ".outputs.artifact }}"; qualification.With["artifact-name"] != want {
			t.Errorf("qualification-%s artifact = %q, want %q", flavour, qualification.With["artifact-name"], want)
		}
		requireNeeds(t, workflow, [][2]string{{"qualification-" + flavour, "build-" + flavour}})
		if other := map[string]string{"standard": "lts", "lts": "standard"}[flavour]; qualification.needs("build-" + other) {
			t.Errorf("qualification-%s waits for the %s build", flavour, other)
		}
	}
}

func TestReleasePublishesKernelExtensions(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")

	requireNeeds(t, workflow, [][2]string{
		{"publish-tag", "publish-extensions"},
		{"publish-extensions", "build-standard"},
		{"publish-extensions", "build-lts"},
		{"publish-extensions", "release-validation"},
		{"release-validation", "bundle"},
		{"release-validation", "vmtests"},
		{"release-validation", "qualification-standard"},
		{"release-validation", "qualification-lts"},
		{"release-validation", "unit"},
		{"release-validation", "qualified-bytes"},
		{"bundle", "build-standard"},
		{"bundle", "build-lts"},
		{"qualified-bytes", "qualification-standard"},
		{"qualified-bytes", "qualification-lts"},
	})
	for _, name := range []string{"publish-extensions", "publish-tag"} {
		job := workflow.Jobs[name]
		if job.ContinueOnError {
			t.Errorf("%s must block release assets on failure", name)
		}
		for trigger, ctx := range releaseTriggers {
			if got, want := conditionHolds(t, job.If, ctx), trigger == "tag" || trigger == "tag, CI passed"; got != want {
				t.Errorf("%s runs on %s = %v, want %v: only tag pushes publish", name, trigger, got, want)
			}
		}
	}
	found := false
	for _, step := range workflow.Jobs["publish-extensions"].Steps {
		found = found || strings.Contains(step.Run, "publish-release-extensions")
	}
	if !found {
		t.Error("release does not publish its extension closures")
	}
}

func TestReleaseKeepsAttestationAndBuildsOutOfLine(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")

	for _, removed := range []string{"runtime", "extensions", "images", "installer", "katlctl", "assemble", "extension-inventory", "qualification"} {
		if _, ok := workflow.Jobs[removed]; ok {
			t.Errorf("release workflow still defines %s", removed)
		}
	}
	attested := false
	for _, step := range workflow.Jobs["bundle"].Steps {
		if strings.HasPrefix(step.Uses, "actions/attest@") {
			attested = true
			for trigger, ctx := range releaseTriggers {
				want := strings.HasPrefix(trigger, "tag") || strings.HasPrefix(trigger, "call")
				if got := conditionHolds(t, step.If, ctx); got != want {
					t.Errorf("attestation on %s = %v, want %v: tag pushes and calls attest the bytes they hand to publication", trigger, got, want)
				}
			}
		}
	}
	if !attested {
		t.Error("the bundle job must attest so provenance names the release workflow")
	}
}

func TestProductionQualificationTakesFlavour(t *testing.T) {
	workflow, _ := readWorkflow(t, "production-vmtests.yml")

	var on struct {
		Dispatch struct {
			Inputs map[string]struct {
				Required bool `yaml:"required"`
			} `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
		Call struct {
			Inputs map[string]struct {
				Required bool `yaml:"required"`
			} `yaml:"inputs"`
		} `yaml:"workflow_call"`
	}
	if err := workflow.On.Decode(&on); err != nil {
		t.Fatal(err)
	}
	if !on.Dispatch.Inputs["flavour"].Required || !on.Call.Inputs["flavour"].Required {
		t.Error("flavour input must be required for dispatch and calls")
	}
	for name, job := range workflow.Jobs {
		if len(matrixAxes(t, job)) != 0 {
			t.Errorf("%s has a matrix; each call qualifies one flavour", name)
		}
		if job.Env["KATL_FLAVOUR"] != "${{ inputs.flavour }}" {
			t.Errorf("%s KATL_FLAVOUR = %q", name, job.Env["KATL_FLAVOUR"])
		}
	}
}

func TestReleaseValidation(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")

	var scripts []string
	for _, step := range workflow.Jobs["release-validation"].Steps {
		scripts = append(scripts, step.Run)
	}
	wiring := map[string]string{
		"UNIT":                "unit",
		"BUNDLE":              "bundle",
		"REGRESSION":          "vmtests",
		"PRODUCTION_STANDARD": "qualification-standard",
		"PRODUCTION_LTS":      "qualification-lts",
		"QUALIFIED":           "qualified-bytes",
	}
	for _, step := range workflow.Jobs["release-validation"].Steps {
		for name, job := range wiring {
			if want := "${{ needs." + job + ".result }}"; step.Env[name] != want {
				t.Errorf("%s = %q, want %q", name, step.Env[name], want)
			}
		}
	}
	for _, dependency := range []string{"UNIT", "BUNDLE", "REGRESSION", "PRODUCTION_STANDARD", "PRODUCTION_LTS", "QUALIFIED"} {
		for _, tc := range []struct {
			state string
			pass  bool
		}{{"success", true}, {"failure", false}, {"skipped", false}, {"cancelled", false}} {
			t.Run(dependency+"/"+tc.state, func(t *testing.T) {
				command := exec.Command("bash", "-euo", "pipefail", "-c", strings.Join(scripts, "\n"))
				command.Env = append(os.Environ(), "REGRESSION_DECISION=run", "UNIT=success", "BUNDLE=success", "REGRESSION=success", "PRODUCTION_STANDARD=success", "PRODUCTION_LTS=success", "QUALIFIED=success", dependency+"="+tc.state)
				output, err := command.CombinedOutput()
				if (err == nil) != tc.pass {
					t.Fatalf("validation result = %v, want pass=%v: %s", err, tc.pass, output)
				}
			})
		}
	}
}

func workflowNames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.yml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	names := make([]string, len(paths))
	for i, path := range paths {
		names[i] = filepath.Base(path)
	}
	return names
}

// readWorkflowSteps reads only the steps of each job, which every workflow
// shares regardless of how its jobs are otherwise configured.
func readWorkflowSteps(t *testing.T, name string) map[string]struct{ Steps []workflowStep } {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []workflowStep } `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return workflow.Jobs
}

// No workflow pushes with git, so a checkout leaves its token on disk for every
// later step to read without any benefit.
func TestWorkflowCheckoutsDropCredentials(t *testing.T) {
	for _, name := range workflowNames(t) {
		for jobName, job := range readWorkflowSteps(t, name) {
			for _, step := range job.Steps {
				if strings.HasPrefix(step.Uses, "actions/checkout@") && step.With["persist-credentials"] != "false" {
					t.Errorf("%s job %s: checkout must set persist-credentials: false", name, jobName)
				}
			}
		}
	}
}

// scripts/vmtest-run fails a run whose requested tests did not pass, so a
// workflow that re-derives that from the go test log owns a second, weaker copy.
func TestWorkflowsLeaveTestOutcomesToVMTestRun(t *testing.T) {
	for _, name := range workflowNames(t) {
		for jobName, job := range readWorkflowSteps(t, name) {
			for _, step := range job.Steps {
				if strings.Contains(step.Run, "go-test.jsonl") && strings.Contains(step.Run, "jq") {
					t.Errorf("%s job %s checks test outcomes with jq; scripts/vmtest-run owns that", name, jobName)
				}
			}
		}
	}
}

var hostedRunPattern = regexp.MustCompile(`-run\s+(?:'([^']*)'|"([^"]*)")`)

// katl-vmtest-ran can only hold a run to the tests it can read from -run, and
// a pattern it cannot parse names no tests, so the run passes unchecked.
func TestWorkflowsRunPatternsAreCheckable(t *testing.T) {
	checker := buildCommand(t, t.TempDir(), "katl-vmtest-ran")

	// The log passes only an unrelated test, so every requested test is missing.
	log := filepath.Join(t.TempDir(), "go-test.log")
	if err := os.WriteFile(log, []byte("--- PASS: TestUnrelated (0.01s)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var patterns int
	for _, name := range workflowNames(t) {
		for jobName, job := range readWorkflowSteps(t, name) {
			for _, step := range job.Steps {
				if !strings.Contains(step.Run, "hosted-vmtest-run") {
					continue
				}
				for _, match := range hostedRunPattern.FindAllStringSubmatch(step.Run, -1) {
					pattern := strings.ReplaceAll(match[1]+match[2], "$TEST_NAMES", "TestAlpha|TestBeta")
					patterns++
					cmd := exec.Command(checker, "-log", log, "-run", pattern)
					if err := cmd.Run(); err == nil {
						t.Errorf("%s job %s: -run %q selects no test katl-vmtest-ran can require", name, jobName, pattern)
					}
				}
			}
		}
	}
	if patterns == 0 {
		t.Fatal("no hosted-vmtest-run -run patterns found")
	}
}

// Re-running every job of a tag rebuilds its artifacts, which are not
// byte-identical, so replacing the assets of a published release would change
// what operators already downloaded. The upload may replace assets only after
// the published checksums are shown to match the rebuilt ones.
func TestReleaseUploadGuardsPublishedAssets(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")

	var found bool
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if !strings.Contains(step.Run, "gh release upload") {
				continue
			}
			found = true
			upload := strings.Index(step.Run, "gh release upload")
			download := strings.Index(step.Run, "gh release download")
			compare := strings.Index(step.Run, "cmp -s")
			fail := strings.Index(step.Run, "exit 1")
			if download < 0 || compare < download || fail < compare || upload < fail {
				t.Errorf("replacing release assets must follow a failing comparison with the published SHA256SUMS: %q", step.Run)
			}
			if !strings.Contains(step.Run, "SHA256SUMS") || !strings.Contains(step.Run, "dist/SHA256SUMS") {
				t.Errorf("published checksums must be compared with dist/SHA256SUMS: %q", step.Run)
			}
		}
	}
	if !found {
		t.Fatal("no step uploads to an existing release")
	}
}

func TestReleaseRegressionFollowsDecision(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")

	regression := workflow.Jobs["vmtests"]
	if !regression.needs("identity") || !strings.Contains(regression.If, "needs.identity.outputs.regression != 'skip'") {
		t.Errorf("VM regression must run unless the identity job decided skip: needs identity=%v if=%q", regression.needs("identity"), regression.If)
	}
	if !strings.Contains(regression.If, "inputs.qualify_run == 0") {
		t.Errorf("VM regression lost its requalification condition: %q", regression.If)
	}

	// Scheduled and dispatched runs have no decision (empty), so they always run the lanes.
	if strings.Contains(regression.If, "event_name") || strings.Contains(regression.If, "ref_type") {
		t.Errorf("VM regression is conditional on the trigger, so a schedule or dispatch could skip it: %q", regression.If)
	}

	// The release bytes embed the version, so no decision may skip building or qualifying them.
	for _, name := range []string{"build-standard", "build-lts", "qualification-standard", "qualification-lts", "bundle", "qualified-bytes"} {
		if job := workflow.Jobs[name]; strings.Contains(job.If, "regression") || strings.Contains(job.If, "decision") {
			t.Errorf("%s depends on the regression decision: %q", name, job.If)
		}
	}

	decided := false
	for _, step := range workflow.Jobs["identity"].Steps {
		if !strings.Contains(step.Run, "scripts/katl-release regression") {
			continue
		}
		decided = true
		if step.Env["GH_TOKEN"] != "${{ github.token }}" || !strings.Contains(step.Run, `"$GITHUB_SHA"`) ||
			!strings.Contains(step.Run, `decision="$(jq -er '.decision`) || !strings.Contains(step.Run, `echo "decision=$decision" >> "$GITHUB_OUTPUT"`) {
			t.Errorf("decision step must judge the tagged commit and export the JSON decision: %+v", step)
		}
		for trigger, ctx := range releaseTriggers {
			if got, want := conditionHolds(t, step.If, ctx), strings.HasPrefix(trigger, "tag"); got != want {
				t.Errorf("VM regression decision on %s = %v, want %v: only tag pushes may skip the lanes", trigger, got, want)
			}
		}
	}
	if !decided {
		t.Error("the identity job does not decide the VM regression")
	}
	if !strings.Contains(workflow.Jobs["identity"].Outputs["regression"], "steps.regression.outputs.decision") {
		t.Error("the identity job does not export the decision")
	}
}

func TestReleaseValidationRegressionSkip(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")

	var script string
	for _, step := range workflow.Jobs["release-validation"].Steps {
		script = step.Run
		if want := "${{ needs.identity.outputs.regression }}"; step.Env["REGRESSION_DECISION"] != want {
			t.Errorf("REGRESSION_DECISION = %q, want %q", step.Env["REGRESSION_DECISION"], want)
		}
	}
	for _, tc := range []struct {
		decision, result string
		pass             bool
	}{
		{"run", "success", true},
		{"run", "skipped", false},
		{"", "skipped", false},
		{"", "success", true},
		{"skip", "skipped", true},
		{"skip", "success", false},
		{"skip", "failure", false},
	} {
		t.Run(tc.decision+"/"+tc.result, func(t *testing.T) {
			command := exec.Command("bash", "-euo", "pipefail", "-c", script)
			command.Env = append(os.Environ(), "UNIT=success", "BUNDLE=success", "PRODUCTION_STANDARD=success", "PRODUCTION_LTS=success", "QUALIFIED=success",
				"REGRESSION="+tc.result, "REGRESSION_DECISION="+tc.decision)
			output, err := command.CombinedOutput()
			if (err == nil) != tc.pass {
				t.Fatalf("validation result = %v, want pass=%v: %s", err, tc.pass, output)
			}
		})
	}
}

func TestNightlyCandidateScheduled(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")
	var on struct {
		Schedule []struct{ Cron string } `yaml:"schedule"`
	}
	if err := workflow.On.Decode(&on); err != nil {
		t.Fatal(err)
	}
	if len(on.Schedule) != 1 {
		t.Fatalf("release artifacts schedules = %v, want one nightly candidate", on.Schedule)
	}

	// A scheduled run has no inputs; an unset number input compares equal to zero.
	vmtests, ok := workflow.Jobs["vmtests"]
	if !ok {
		t.Fatal("release artifacts has no vmtests job")
	}
	condition := expandExpressions(t, "${{ "+vmtests.If+" }}", map[string]string{
		"github.event_name": "schedule", "inputs.qualify_run": "0",
	})
	if condition != "true" {
		t.Errorf("vmtests if %q evaluates to %s on schedule, want true so the nightly runs the VM regression suite", vmtests.If, condition)
	}
	if scenarios := vmtests.With["scenarios"]; scenarios != "" {
		t.Errorf("vmtests passes scenarios %q, want the complete suite so the VM images are built", scenarios)
	}
}

// The nightly candidate retains its artifacts for a week. Tags and dispatches
// keep them for the full publication window.
func TestNightlyArtifactRetention(t *testing.T) {
	release, _ := readWorkflow(t, "release-artifacts.yml")
	for _, job := range []string{"build-standard", "build-lts"} {
		if got := release.Jobs[job].With["retention-days"]; got != "${{ fromJSON(needs.identity.outputs.retention-days) }}" {
			t.Errorf("%s retention-days = %q, want the release retention as a number", job, got)
		}
	}
	bundleRetention := ""
	for _, step := range release.Jobs["bundle"].Steps {
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			bundleRetention = step.With["retention-days"]
		}
	}
	if bundleRetention != "${{ needs.identity.outputs.retention-days }}" {
		t.Errorf("bundle retention-days = %q, want the release retention", bundleRetention)
	}

	expression := release.Jobs["identity"].Outputs["retention-days"]
	for event, want := range map[string]string{"schedule": "7", "push": "30", "workflow_dispatch": "30"} {
		ctx := map[string]string{"github.event_name": event}
		if got := expandExpressions(t, expression, ctx); got != want {
			t.Errorf("release retention on %s = %s, want %s", event, got, want)
		}
	}
}

// The nightly candidate must not cancel, queue behind or share a group with a
// tag run, a dispatch or another ref's run, and two nights must not overlap.
func TestNightlyCandidateConcurrency(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")
	var concurrency struct {
		Group            string
		CancelInProgress bool `yaml:"cancel-in-progress"`
	}
	if err := workflow.Concurrency.Decode(&concurrency); err != nil {
		t.Fatal(err)
	}
	group := func(event, ref, runID string) string {
		return expandExpressions(t, concurrency.Group, map[string]string{
			"github.workflow": "wf", "github.event_name": event, "github.ref": ref, "github.run_id": runID,
		})
	}

	night := group("schedule", "refs/heads/main", "1")
	if again := group("schedule", "refs/heads/main", "2"); night != again {
		t.Errorf("consecutive nightly runs use groups %q and %q, want one group so they never overlap", night, again)
	}
	for name, other := range map[string]string{
		"push to main":    group("push", "refs/heads/main", "3"),
		"tag push":        group("push", "refs/tags/v2026.11.2-rc.1", "4"),
		"dispatch":        group("workflow_dispatch", "refs/heads/main", "5"),
		"dispatch on tag": group("workflow_dispatch", "refs/tags/v2026.11.2", "6"),
	} {
		if night == other {
			t.Errorf("nightly candidate shares group %q with a %s", night, name)
		}
	}
	if concurrency.CancelInProgress {
		t.Error("a release artifacts run cancels runs in its group")
	}
}

// The scheduled run has no input and no tag, so it derives its development
// version from the pinned upgrade source; a tag run keeps the tag's version.
func TestReleaseIdentityVersion(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")
	steps := workflow.Jobs["identity"].Steps
	previous, release := -1, -1
	for i, step := range steps {
		switch {
		case strings.Contains(step.Run, "katl-release-artifacts upgrade-baseline"):
			previous = i
		case strings.Contains(step.Run, "katl-release-artifacts version"):
			release = i
		}
	}
	if previous < 0 || release < 0 || previous >= release {
		t.Fatalf("upgrade source step %d must exist and precede the identity step %d that derives the version from it", previous, release)
	}
	step := steps[release]
	if got, want := step.Env["PREVIOUS_TAG"], "${{ steps.previous.outputs.tag }}"; got != want || steps[previous].ID != "previous" {
		t.Errorf("identity step PREVIOUS_TAG = %q from step id %q, want %q from the upgrade source step", got, steps[previous].ID, want)
	}
	if got, want := step.Env["MANUAL_VERSION"], "${{ inputs.version }}"; got != want {
		t.Errorf("identity step MANUAL_VERSION = %q, want %q", got, want)
	}

	for _, tt := range []struct {
		name, event, refType, refName, manual string
		version                               string
	}{
		{"nightly", "schedule", "branch", "main", "", "2026.11.6-dev.0"},
		{"tag", "push", "tag", "v2026.11.6", "", "2026.11.6"},
		{"dispatch", "workflow_dispatch", "branch", "main", "2026.12.0-beta.1", "2026.12.0-beta.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			outputs := filepath.Join(dir, "output")
			command := exec.Command("bash", "-euo", "pipefail", "-c", step.Run)
			command.Dir = repoRoot(t)
			command.Env = append(gittest.Env(os.Environ(), dir), "PREVIOUS_TAG=v2026.11.5",
				"GITHUB_EVENT_NAME="+tt.event, "GITHUB_REF_TYPE="+tt.refType, "GITHUB_REF_NAME="+tt.refName, "MANUAL_VERSION="+tt.manual,
				"GITHUB_OUTPUT="+outputs, "GITHUB_SHA=0123456789abcdef0123456789abcdef01234567")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("identity step failed: %v\n%s", err, output)
			}
			written, err := os.ReadFile(outputs)
			if err != nil {
				t.Fatal(err)
			}
			want := "version=" + tt.version + "\nartifact-name=katl-" + tt.version + "-x86_64-0123456789ab\n"
			if string(written) != want {
				t.Errorf("identity outputs = %q, want %q", written, want)
			}
		})
	}
}
