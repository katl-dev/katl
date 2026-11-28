package scriptstest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/releasestate"
)

func TestReleaseCoverageFalsification(t *testing.T) {
	const shared = ".github/workflows/shared.yml"
	const composite = ".github/actions/helper/action.yml"
	leaf := workflowSite{composite, "(composite)", "leaf"}
	tests := []struct {
		name        string
		trigger     string
		forward     string
		input       string
		releaseWith string
		mergeWith   string
		concurrency string
		step        string
		jobs        string
		allow       []releaseException
		want        []string
	}{
		{name: "shared workflow and composite"},
		{
			name:        "commit is a caller value",
			input:       "commit",
			releaseWith: "release-sha",
			mergeWith:   "merge-sha",
		},
		{
			name:        "version is a mode",
			input:       "version",
			releaseWith: "2026.12.1-rc.1",
			mergeWith:   "2026.12.1-dev.0",
			want:        []string{releasestate.ReleaseWorkflow + " / call / (job): reach", "version=\"2026.12.1-rc.1\""},
		},
		{
			name: "tag-only step",
			step: "if: github.ref_type == 'tag'",
			want: []string{leaf.String() + ": if", "mode outcomes absent before merge"},
		},
		{
			name: "new step under a tag-only job",
			jobs: "  legacy:\n    if: github.ref_type == 'tag'\n    runs-on: ubuntu-24.04\n    steps:\n      - id: new-step\n        run: scripts/work\n",
			allow: []releaseException{
				{workflowSite{shared, "legacy", "(job)"}, "gap", 907, "", []string{"if github.ref_type == 'tag'"}},
			},
			want: []string{shared + " / legacy / new-step: inactive"},
		},
		{
			name:    "dispatch-only input",
			trigger: "  workflow_dispatch:\n    inputs:\n      live:\n        type: boolean\n",
			forward: "        with:\n          live: ${{ inputs.live }}\n",
			step:    "if: inputs.live == 'true'",
			want: []string{
				shared + " / (on) / workflow_dispatch.live: input",
				leaf.String() + ": if \"inputs.live\"",
			},
		},
		{
			name: "actor-gated step",
			step: "if: github['actor'] != 'release-bot[bot]'",
			want: []string{leaf.String() + ": if", "mode outcomes absent before merge"},
		},
		{
			name: "triggering actor guard",
			step: "if: github.triggering_actor != 'katl-release[bot]'",
			want: []string{leaf.String() + ": if \"github.triggering_actor\": mode outcomes absent before merge"},
		},
		{
			name: "workflow name in step env",
			step: "env:\n  DRY: ${{ github.workflow == 'Release' && '0' || '1' }}",
			want: []string{leaf.String() + ": env.DRY", leaf.String() + ": reach: unreached release step or job with env.DRY=\"0\""},
		},
		{
			name: "workflow name in job env",
			jobs: "  other:\n    runs-on: ubuntu-24.04\n    env:\n      DRY: ${{ github.workflow == 'Release' && '0' || '1' }}\n    steps:\n      - id: dry\n        run: scripts/work\n",
			want: []string{
				shared + " / other / (job): env.DRY",
				shared + " / other / dry: reach: unreached release step or job with env.DRY=\"0\"",
			},
		},
		{
			name:        "step env without a mode reference",
			input:       "version",
			releaseWith: "2026.12.1-rc.1",
			mergeWith:   "2026.12.1-dev.0",
			step:        "env:\n  VERSION: ${{ env.ARG }}",
			want:        []string{leaf.String() + ": reach: unreached release step or job with env.VERSION=\"2026.12.1-rc.1\""},
		},
		{
			name: "mode branch in run body",
			step: "run: |\n  if [[ \"$GITHUB_REF_TYPE\" == tag ]]; then scripts/work; fi",
			want: []string{leaf.String() + ": run-reference \"GITHUB_REF_TYPE\"", leaf.String() + ": shell"},
		},
		{
			name: "mode expression in with",
			step: "uses: actions/checkout@v7\nwith:\n  ref: ${{ github.event_name == 'workflow_dispatch' && 'release' || 'premerge' }}",
			want: []string{leaf.String() + ": with.ref", leaf.String() + ": reach"},
		},
		{
			name:        "mode expression in concurrency",
			concurrency: "concurrency:\n  group: ${{ github.event_name == 'workflow_dispatch' && 'release' || 'premerge' }}\n",
			want:        []string{shared + " / (workflow) / (workflow): concurrency.group"},
		},
		{
			name: "mode expression in env",
			step: "env:\n  MODE: ${{ github.ref_type == 'tag' && 'release' || 'premerge' }}",
			want: []string{leaf.String() + ": env.MODE"},
		},
		{
			name: "mode expression in run interpolation",
			step: "run: scripts/work ${{ github.event_name == 'workflow_dispatch' && 'release' || 'premerge' }}",
			want: []string{leaf.String() + ": run-interpolation"},
		},
		{
			name: "interpolation adds shell control flow",
			step: "run: scripts/work ${{ '&&' }} scripts/other",
			want: []string{leaf.String() + ": shell"},
		},
		{
			name:  "unused exception",
			allow: []releaseException{{leaf, "gap", 123, "", []string{"reach"}}},
			want:  []string{leaf.String() + ": unused allowlist entry for reach"},
		},
		{
			name:  "missing substitute test",
			step:  "run: scripts/work | cat",
			allow: []releaseException{{leaf, "external write", 0, "TestMissing", []string{"shell"}}},
			want:  []string{leaf.String() + ": substitute \"TestMissing\" does not name a test"},
		},
		{
			name:  "existing substitute test",
			step:  "run: scripts/work | cat",
			allow: []releaseException{{leaf, "external write", 0, "TestRehearsal", []string{"shell"}}},
		},
		{
			name:  "existing shell gap cannot hide a new mode branch",
			step:  "run: |\n  if [[ \"$GITHUB_EVENT_NAME\" == push ]]; then scripts/work; fi",
			allow: []releaseException{{leaf, "gap", 123, "", []string{"shell"}}},
			want:  []string{leaf.String() + ": run-reference"},
		},
		{
			name:  "existing run-reference gap cannot hide a new mode read",
			step:  "run: |\n  if [[ \"$GITHUB_EVENT_NAME\" == push ]]; then scripts/work; fi\n  echo \"$GITHUB_TRIGGERING_ACTOR\"",
			allow: []releaseException{{leaf, "gap", 123, "", []string{"run-reference GITHUB_EVENT_NAME", "shell"}}},
			want:  []string{leaf.String() + ": run-reference \"GITHUB_TRIGGERING_ACTOR\""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			call := "\njobs:\n  call:\n    uses: ./" + shared + "\n"
			releaseCall, mergeCall := call, call
			var inputs, env string
			if tc.input != "" {
				inputs = fmt.Sprintf("    inputs:\n      %s:\n        type: string\n        required: true\n", tc.input)
				env = fmt.Sprintf("env:\n  ARG: ${{ inputs.%s }}\n", tc.input)
				releaseCall += fmt.Sprintf("    with:\n      %s: %s\n", tc.input, tc.releaseWith)
				mergeCall += fmt.Sprintf("    with:\n      %s: %s\n", tc.input, tc.mergeWith)
			}
			writeCoverageFixture(t, root, releasestate.ReleaseWorkflow, "name: Release\non: workflow_dispatch\n"+releaseCall)
			writeCoverageFixture(t, root, ciWorkflowPath, "name: CI\non: [merge_group, pull_request]\n"+mergeCall)
			writeCoverageFixture(t, root, shared, "on:\n  workflow_call:\n"+inputs+tc.trigger+tc.concurrency+env+
				"jobs:\n  work:\n    runs-on: ubuntu-24.04\n    steps:\n      - uses: ./.github/actions/helper\n"+tc.forward+tc.jobs)
			step := tc.step
			if !strings.HasPrefix(step, "uses:") && !strings.HasPrefix(step, "run:") {
				step = "run: scripts/work\n" + step
			}
			var shell string
			if strings.HasPrefix(step, "run:") {
				shell = "      shell: bash\n"
			}
			writeCoverageFixture(t, root, composite, "name: Helper\ndescription: Exercise a release step\n"+
				"inputs:\n  live:\n    description: Select the live mode\n    default: ''\n"+
				"runs:\n  using: composite\n  steps:\n    - id: leaf\n"+shell+"      "+
				strings.ReplaceAll(strings.TrimSpace(step), "\n", "\n      ")+"\n")
			writeCoverageFixture(t, root, "fixture_test.go", "package fixture\nimport \"testing\"\nfunc TestRehearsal(t *testing.T) {}\n")

			releaseRuns := []map[string]string{workflowEvent("workflow_dispatch", "refs/heads/release/2026.12.1")}
			release := traceWorkflows(t, root, releasestate.ReleaseWorkflow, releaseRuns, nil)
			merge := traceWorkflows(t, root, ciWorkflowPath, ciCoverageRuns(), nil)
			diagnostics := checkReleaseCoverage(root, release, merge, tc.allow)
			if len(tc.want) == 0 && len(diagnostics) != 0 {
				t.Fatalf("expected coverage, got:\n%s", strings.Join(diagnostics, "\n"))
			}
			for _, want := range tc.want {
				if !strings.Contains(strings.Join(diagnostics, "\n"), want) {
					t.Errorf("expected a diagnostic containing %q, got:\n%s", want, strings.Join(diagnostics, "\n"))
				}
			}
		})
	}
}

func writeCoverageFixture(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
