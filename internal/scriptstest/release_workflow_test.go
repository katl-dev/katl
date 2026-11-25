package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflowStep struct {
	Name            string            `yaml:"name"`
	If              string            `yaml:"if"`
	Run             string            `yaml:"run"`
	Uses            string            `yaml:"uses"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	Env             map[string]string `yaml:"env"`
	With            map[string]string `yaml:"with"`
}

type workflowJob struct {
	If              string            `yaml:"if"`
	Needs           yaml.Node         `yaml:"needs"`
	Uses            string            `yaml:"uses"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	Permissions     yaml.Node         `yaml:"permissions"`
	Env             map[string]string `yaml:"env"`
	With            map[string]string `yaml:"with"`
	Strategy        struct {
		Matrix map[string]yaml.Node `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []workflowStep `yaml:"steps"`
}

type workflowFile struct {
	On          yaml.Node              `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Concurrency yaml.Node              `yaml:"concurrency"`
	Env         map[string]string      `yaml:"env"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) (workflowFile, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", name))
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
	dependencies := job.Needs.Content
	if job.Needs.Kind == yaml.ScalarNode {
		dependencies = []*yaml.Node{&job.Needs}
	}
	for _, node := range dependencies {
		if node.Value == dependency {
			return true
		}
	}
	return false
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
	bundlesArtifact    = "katl-extension-bundles-${{ inputs.flavour }}-${{ github.sha }}"
	tagPushCondition   = "github.event_name == 'push' && github.ref_type == 'tag'"
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
		if _, ok := job.Strategy.Matrix["flavour"]; ok {
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

	matrix := workflow.Jobs["extensions"].Strategy.Matrix
	if len(matrix) != 1 || matrix["extension"].Value != extensionsMatrixFn {
		t.Error("extension matrix must come from the runtime job's recipe inventory and nothing else")
	}
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
		if step.With["name"] == bundlesArtifact {
			paths := step.With["path"]
			carried = strings.Contains(paths, "_build/mkosi/extension-bundles/") && strings.Contains(paths, "_build/mkosi/release-extensions.json")
			if step.With["retention-days"] != "30" {
				t.Error("extension publication inputs must survive the 30-day publication retention window")
			}
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

func TestProductionBuildPublicationOnlyGatesRetention(t *testing.T) {
	workflow, _ := readWorkflow(t, productionBuild)

	for name, job := range workflow.Jobs {
		for key, value := range job.Env {
			if strings.Contains(value, "inputs.publication") {
				t.Errorf("%s env %s depends on publication", name, key)
			}
		}
		for _, step := range job.Steps {
			uploads := strings.HasPrefix(step.Uses, "actions/upload-artifact@")
			publicationNamesOutput := step.Name == "Resolve artifact names"
			for context, value := range map[string]string{"run": step.Run, "name": step.Name, "uses": step.Uses} {
				if context != "name" && strings.Contains(value, "inputs.publication") {
					t.Errorf("%s step %q reads publication in %s", name, step.Name, context)
				}
			}
			for key, value := range step.With {
				if strings.Contains(value, "inputs.publication") && !(uploads && key == "retention-days") {
					t.Errorf("%s step %q passes publication in with.%s", name, step.Name, key)
				}
			}
			for key, value := range step.Env {
				if strings.Contains(value, "inputs.publication") && !publicationNamesOutput {
					t.Errorf("%s step %q passes publication in env %s", name, step.Name, key)
				}
			}
			if strings.Contains(step.If, "inputs.publication") && !uploads {
				t.Errorf("%s step %q is conditional on publication but is not an upload", name, step.Name)
			}
		}
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
		if build.With["flavour"] != flavour || build.With["publication"] != "true" || !version.MatchString(build.With["version"]) {
			t.Errorf("build-%s inputs = %v, want literal flavour %s with publication", flavour, build.With, flavour)
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
		if job.If != tagPushCondition || job.ContinueOnError {
			t.Errorf("%s must run only on tag pushes and block release assets on failure", name)
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
			if step.If != tagPushCondition {
				t.Errorf("attestation condition = %q, want tag pushes only", step.If)
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
		if len(job.Strategy.Matrix) != 0 {
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
				command.Env = append(os.Environ(), "UNIT=success", "BUNDLE=success", "REGRESSION=success", "PRODUCTION_STANDARD=success", "PRODUCTION_LTS=success", "QUALIFIED=success", dependency+"="+tc.state)
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
	root := repoRoot(t)
	checker := filepath.Join(t.TempDir(), "katl-vmtest-ran")
	build := exec.Command("go", "build", "-o", checker, "./cmd/katl-vmtest-ran")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build katl-vmtest-ran: %v\n%s", err, out)
	}

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
