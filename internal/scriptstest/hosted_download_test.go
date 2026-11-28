package scriptstest

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/katl-dev/katl/internal/flavour"
	"github.com/katl-dev/katl/internal/workstationtest"
)

var goTestTimeout = regexp.MustCompile(`-timeout (\d+)m\b`)
var directDownload = regexp.MustCompile(`(?m)^[ \t]*gh[ \t]+(run|release)[ \t]+download\b`)

type budgetStep struct {
	Name            string            `yaml:"name"`
	If              string            `yaml:"if"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	Env             map[string]string `yaml:"env"`
	Timeout         int               `yaml:"timeout-minutes"`
	ContinueOnError string            `yaml:"continue-on-error"`
}

type budgetFile struct {
	Jobs map[string]struct {
		Uses    string       `yaml:"uses"`
		Timeout int          `yaml:"timeout-minutes"`
		Steps   []budgetStep `yaml:"steps"`
	} `yaml:"jobs"`
	Runs struct {
		Steps []budgetStep `yaml:"steps"`
	} `yaml:"runs"`
}

func (s budgetStep) label(i int) string {
	switch {
	case s.Name != "":
		return s.Name
	case s.Uses != "":
		return s.Uses
	}
	return "step " + strconv.Itoa(i)
}

// perBundle matches a run script that iterates the extension bundle
// inventory, which the release workflow carries from katl-release
// extension-bundles as EXTENSION_BUNDLES.
var perBundle = regexp.MustCompile(`\.bundles\[\][^\n]*<<< "\$EXTENSION_BUNDLES"`)

// downloadBudget is the longest a step's hosted-download calls may run. A
// step that loops over the extension bundle inventory downloads once per
// bundle, so its worst case grows with the inventory.
func (s budgetStep) downloadBudget() int {
	if !strings.Contains(s.Run, "hosted-download") {
		return 0
	}
	worst := downloadWorstCase(s.Env)
	if perBundle.MatchString(s.Run) {
		worst *= len(flavour.Supported())
	}
	return worst
}

// budgetViolations reports how a workflow or composite action breaks the time
// budget policy: every artifact download goes through hosted-download, every
// job step has a timeout that fits the job's, go test's own timeout fires
// before its step's and job's, and a cache restore that stalls is a miss.
func budgetViolations(data []byte, compositeDownloads map[string]int) ([]string, error) {
	var file budgetFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, err
	}

	var violations []string
	common := func(where string, i int, s budgetStep) {
		if strings.HasPrefix(s.Uses, "actions/download-artifact@") {
			violations = append(violations, fmt.Sprintf("%s: %s downloads with %s; use .github/scripts/hosted-download", where, s.label(i), s.Uses))
		}
		if command := strings.TrimSpace(directDownload.FindString(s.Run)); command != "" {
			violations = append(violations, fmt.Sprintf("%s: %s downloads with %s; use .github/scripts/hosted-download", where, s.label(i), command))
		}
		if strings.HasPrefix(s.Uses, "actions/cache/restore@") && s.ContinueOnError != "true" {
			violations = append(violations, fmt.Sprintf("%s: cache restore %s must continue on error so a stall is a miss", where, s.label(i)))
		}
	}
	for i, s := range file.Runs.Steps {
		common("composite", i, s)
	}

	for jobName, job := range file.Jobs {
		where := "job " + jobName
		if job.Uses != "" {
			continue
		}
		if job.Timeout <= 0 {
			violations = append(violations, where+": no timeout-minutes")
		}
		for i, s := range job.Steps {
			label := s.label(i)
			common(where, i, s)
			if s.Timeout <= 0 {
				violations = append(violations, fmt.Sprintf("%s: %s has no timeout-minutes", where, label))
			}
			if strings.HasPrefix(s.Uses, "actions/cache/restore@") && s.Timeout > cacheRestoreMinutes {
				violations = append(violations, fmt.Sprintf("%s: cache restore %s has %d minutes; a restore gets at most %d", where, label, s.Timeout, cacheRestoreMinutes))
			}
			if need := s.downloadBudget() + compositeDownloads[s.Uses]; need > 0 && s.Timeout*60 <= need {
				violations = append(violations, fmt.Sprintf("%s: %s has %d minutes for downloads that may take %d seconds", where, label, s.Timeout, need))
			}
			if m := goTestTimeout.FindStringSubmatch(s.Run); m != nil {
				minutes, _ := strconv.Atoi(m[1])
				if s.Timeout <= minutes {
					violations = append(violations, fmt.Sprintf("%s: %s has %d minutes for go test -timeout %dm", where, label, s.Timeout, minutes))
				}
				if setup := budgetSum(job.Steps[:i]); job.Timeout <= setup+minutes {
					violations = append(violations, fmt.Sprintf("%s: job timeout %d does not exceed setup %d plus go test -timeout %dm", where, job.Timeout, setup, minutes))
				}
			}
		}
		if sum := budgetSum(job.Steps); job.Timeout > 0 && job.Timeout <= sum {
			violations = append(violations, fmt.Sprintf("%s: job timeout %d is not above the step budgets' sum %d", where, job.Timeout, sum))
		}
	}
	return violations, nil
}

const cacheRestoreMinutes = 5

var simpleComparison = regexp.MustCompile(`^(.+?)\s*(==|!=)\s*(.+)$`)

// budgetSum is the longest a run of steps may take. Steps guarded by the two
// sides of one comparison, such as x == 'a' and x != 'a', never both run, so
// only the larger side counts; every other step is assumed to run.
func budgetSum(steps []budgetStep) int {
	type sides struct{ equal, unequal int }
	pairs := map[string]*sides{}
	var sum int
	for _, s := range steps {
		sum += s.Timeout
		condition := strings.TrimSpace(s.If)
		condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
		m := simpleComparison.FindStringSubmatch(condition)
		if m == nil || strings.ContainsAny(m[1]+m[3], "&|!=") {
			continue
		}
		key := strings.TrimSpace(m[1]) + "\x00" + strings.TrimSpace(m[3])
		if pairs[key] == nil {
			pairs[key] = &sides{}
		}
		if m[2] == "==" {
			pairs[key].equal += s.Timeout
		} else {
			pairs[key].unequal += s.Timeout
		}
	}
	for _, p := range pairs {
		sum -= min(p.equal, p.unequal)
	}
	return sum
}

var hostedDownloadCall = regexp.MustCompile(`hosted-download (?:--id \S+ )?(artifacts?|release) (\S+) (\S+)`)

// hostedDownload reports what a step asks hosted-download for, with the
// step's environment substituted, so handoff tests can follow an artifact
// from its upload to its consumers.
func hostedDownload(step workflowStep) (kind, subject, dest string, ok bool) {
	m := hostedDownloadCall.FindStringSubmatch(step.Run)
	if m == nil {
		return "", "", "", false
	}
	resolve := func(word string) string {
		word = strings.Trim(word, `"`)
		name := strings.Trim(strings.TrimPrefix(word, "$"), "{}")
		if value, found := step.Env[name]; found && strings.HasPrefix(word, "$") {
			return value
		}
		return word
	}
	return m[1], resolve(m[2]), resolve(m[3]), true
}

// downloadWorstCase is the longest hosted-download may run with a step's
// settings, as its header states: each attempt can use its limit plus the
// SIGTERM grace period. Sleeps stay below twice their doubling base.
func downloadWorstCase(env map[string]string) int {
	setting := func(name string, fallback int) int {
		if v, err := strconv.Atoi(env[name]); err == nil {
			return v
		}
		return fallback
	}
	limit := setting("KATL_DOWNLOAD_ATTEMPT_SECONDS", 360)
	attempts := setting("KATL_DOWNLOAD_ATTEMPTS", 3)
	backoff := setting("KATL_DOWNLOAD_BACKOFF_SECONDS", limit/24)
	worst := attempts * (limit + setting("KATL_DOWNLOAD_KILL_AFTER_SECONDS", 5))
	for gap := 1; gap < attempts; gap++ {
		worst += 2 * backoff << (gap - 1)
	}
	return worst
}

// queueJobMinutes bounds every job that a pull request or merge group runs,
// with headroom under the merge queue's 120-minute group timeout, so a job's
// own budget names a stall before the queue cancels the whole group.
const queueJobMinutes = 100

// queueTimeoutViolations reports jobs over queueJobMinutes in the workflows
// that pull_request or merge_group start, and in the reusable workflows they
// call. files maps a workflow's file name to its contents.
func queueTimeoutViolations(files map[string][]byte) ([]string, error) {
	type workflow struct {
		On   yaml.Node `yaml:"on"`
		Jobs map[string]struct {
			Uses    string `yaml:"uses"`
			Timeout int    `yaml:"timeout-minutes"`
		} `yaml:"jobs"`
	}
	parsed := map[string]workflow{}
	var pending []string
	for name, data := range files {
		var w workflow
		if err := yaml.Unmarshal(data, &w); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		parsed[name] = w
		var events []string
		switch w.On.Kind {
		case yaml.ScalarNode:
			events = []string{w.On.Value}
		case yaml.SequenceNode:
			for _, n := range w.On.Content {
				events = append(events, n.Value)
			}
		case yaml.MappingNode:
			for i := 0; i < len(w.On.Content); i += 2 {
				events = append(events, w.On.Content[i].Value)
			}
		}
		if slices.Contains(events, "pull_request") || slices.Contains(events, "merge_group") {
			pending = append(pending, name)
		}
	}

	var violations []string
	reached := map[string]bool{}
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		if reached[name] {
			continue
		}
		reached[name] = true
		for jobName, job := range parsed[name].Jobs {
			if callee, ok := strings.CutPrefix(job.Uses, "./.github/workflows/"); ok {
				pending = append(pending, callee)
				continue
			}
			if job.Timeout > queueJobMinutes {
				violations = append(violations, fmt.Sprintf("%s job %s: timeout %d exceeds the %d minutes a queued job may take", name, jobName, job.Timeout, queueJobMinutes))
			}
		}
	}
	return violations, nil
}

// TestWorkflowBudgets holds every workflow and composite action to the
// budget policy, so one stalled step cannot consume its job's timeout and
// go test reports a hung scenario before the job is cancelled.
func TestWorkflowBudgets(t *testing.T) {
	t.Run("repository", func(t *testing.T) {
		root := repoRoot(t)
		paths, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
		if err != nil {
			t.Fatal(err)
		}
		actions, err := filepath.Glob(filepath.Join(root, ".github", "actions", "*", "action.yml"))
		if err != nil {
			t.Fatal(err)
		}
		queued := map[string][]byte{}
		for _, path := range paths {
			queued[filepath.Base(path)] = mustReadFile(t, path)
		}
		violations, err := queueTimeoutViolations(queued)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range violations {
			t.Error(v)
		}
		compositeDownloads := map[string]int{}
		for _, path := range actions {
			var action budgetFile
			if err := yaml.Unmarshal(mustReadFile(t, path), &action); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			for _, step := range action.Runs.Steps {
				compositeDownloads["./"+rel] += step.downloadBudget()
			}
		}
		paths = append(paths, actions...)
		if len(paths) < 10 {
			t.Fatalf("found only %d workflows and actions", len(paths))
		}
		for _, path := range paths {
			violations, err := budgetViolations(mustReadFile(t, path), compositeDownloads)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			rel, _ := filepath.Rel(root, path)
			for _, v := range violations {
				t.Errorf("%s %s", rel, v)
			}
		}
	})

	budgeted := `jobs:
  build:
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@v7
        timeout-minutes: 5
      - name: Download inputs
        timeout-minutes: 20
        run: .github/scripts/hosted-download artifact inputs _build/import
`
	for _, tc := range []struct {
		name     string
		workflow string
		want     string
	}{
		{"direct download", budgeted + `      - uses: actions/download-artifact@v8
        timeout-minutes: 2
`, "use .github/scripts/hosted-download"},
		{"direct download in a composite action", `runs:
  using: composite
  steps:
    - uses: actions/download-artifact@v8
`, "use .github/scripts/hosted-download"},
		{"direct release download", budgeted + `      - timeout-minutes: 2
        run: |
          if test -d dist; then
            gh release download v1 --pattern SHA256SUMS
          fi
`, "downloads with gh release download; use .github/scripts/hosted-download"},
		{"step without a timeout", budgeted + `      - run: make
`, "step 2 has no timeout-minutes"},
		{"job without a timeout", strings.Replace(budgeted, "    timeout-minutes: 30\n", "", 1), "job build: no timeout-minutes"},
		{"budgets reaching the job timeout", budgeted + `      - run: make
        timeout-minutes: 5
`, "job timeout 30 is not above the step budgets' sum 30"},
		{"go test timeout firing last", strings.Replace(budgeted, "30", "60", 1) + `      - run: go test -timeout 30m ./...
        timeout-minutes: 30
`, "has 30 minutes for go test -timeout 30m"},
		{"go test timeout beyond the job's", strings.Replace(budgeted, "30", "60", 1) + `      - run: go test -timeout 36m ./...
        timeout-minutes: 37
`, "job timeout 60 does not exceed setup 25 plus go test -timeout 36m"},
		{"download step shorter than its attempts", strings.Replace(budgeted, "timeout-minutes: 20", "timeout-minutes: 15", 1), "has 15 minutes for downloads that may take 1185 seconds"},
		{"ID download step shorter than its attempts", strings.NewReplacer("timeout-minutes: 20", "timeout-minutes: 15", "artifact inputs _build/import", "--id 9 artifact inputs _build/import 7").Replace(budgeted), "has 15 minutes for downloads that may take 1185 seconds"},
		{"inventory download step shorter than its bundles", strings.Replace(budgeted, "run: .github/scripts/hosted-download artifact inputs _build/import", `run: |
          jq -er '.bundles[].name' <<< "$EXTENSION_BUNDLES" |
          while IFS= read -r name; do
            .github/scripts/hosted-download artifact "$name" "artifacts/$name"
          done`, 1), fmt.Sprintf("has 20 minutes for downloads that may take %d seconds", 1185*len(flavour.Supported()))},
		{"cache restore failing the job", budgeted + `      - uses: actions/cache/restore@v6
        timeout-minutes: 5
`, "must continue on error"},
		{"budgets of steps with unrelated conditions", budgeted + `      - run: make
        if: inputs.ref != ''
        timeout-minutes: 3
      - run: make
        if: inputs.mode == 'fast'
        timeout-minutes: 3
`, "job timeout 30 is not above the step budgets' sum 31"},
		{"cache restore over its budget", strings.Replace(budgeted, "30", "60", 1) + `      - uses: actions/cache/restore@v6
        timeout-minutes: 10
        continue-on-error: true
`, "has 10 minutes; a restore gets at most 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			violations, err := budgetViolations([]byte(tc.workflow), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(violations, func(v string) bool { return strings.Contains(v, tc.want) }) {
				t.Errorf("violations %q; want one containing %q", violations, tc.want)
			}
		})
	}

	t.Run("composite download exceeds its caller's budget", func(t *testing.T) {
		workflow := `jobs:
  build:
    timeout-minutes: 30
    steps:
      - uses: ./.github/actions/combine
        timeout-minutes: 11
`
		violations, err := budgetViolations([]byte(workflow), map[string]int{"./.github/actions/combine": 1185})
		if err != nil {
			t.Fatal(err)
		}
		want := "job build: ./.github/actions/combine has 11 minutes for downloads that may take 1185 seconds"
		if !slices.Equal(violations, []string{want}) {
			t.Errorf("violations %q, want %q", violations, want)
		}
	})

	t.Run("queued jobs", func(t *testing.T) {
		long := `
jobs:
  build:
    timeout-minutes: 110
    steps: []
`
		violations, err := queueTimeoutViolations(map[string][]byte{
			"ci.yml":      []byte("on: [merge_group]\njobs:\n  call:\n    uses: ./.github/workflows/lanes.yml\n"),
			"lanes.yml":   []byte("on: workflow_call" + long),
			"nightly.yml": []byte("on:\n  schedule:\n    - cron: '0 3 * * *'" + long),
			"pr.yml":      []byte("on:\n  pull_request:" + long),
		})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(violations)
		want := []string{
			"lanes.yml job build: timeout 110 exceeds the 100 minutes a queued job may take",
			"pr.yml job build: timeout 110 exceeds the 100 minutes a queued job may take",
		}
		if !slices.Equal(violations, want) {
			t.Errorf("violations %q; want the called lane and the pull request job, not the nightly one", violations)
		}
	})

	t.Run("complementary steps count once", func(t *testing.T) {
		workflow := budgeted + `      - uses: ./a
        if: inputs.ref != ''
        timeout-minutes: 4
      - uses: ./b
        if: ${{ inputs.ref == '' }}
        timeout-minutes: 3
`
		violations, err := budgetViolations([]byte(workflow), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(violations) != 0 {
			t.Errorf("violations %q; want none, since only one of the 4- and 3-minute steps runs", violations)
		}
	})

	t.Run("compliant", func(t *testing.T) {
		workflow := strings.Replace(budgeted, "30", "40", 1) + `      - uses: actions/cache/restore@v6
        timeout-minutes: 5
        continue-on-error: true
      - name: Download a small input
        timeout-minutes: 5
        env:
          KATL_DOWNLOAD_ATTEMPT_SECONDS: 60
        run: .github/scripts/hosted-download artifact lock _build/lock
  call:
    uses: ./.github/workflows/other.yml
`
		violations, err := budgetViolations([]byte(workflow), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(violations) != 0 {
			t.Errorf("violations %q; want none", violations)
		}
	})
}

// fakeArtifact is one artifact of the fake run that downloadWorld serves.
type fakeArtifact struct {
	ID      int
	Name    string
	Created string
	Expired bool
	Files   map[string]string
}

// downloadWorld is a directory holding a fake gh and the run it serves:
// listing.json lists the artifacts in the given order, meta-<id>.json and
// zip-<id>.zip describe each one. Every download operation, a release
// download or an archive fetch, behaves as the next line of its plan says:
// "ok" completes, "partial" sends a fragment and fails, "truncated" sends a
// fragment and succeeds, "stall" hangs, "deaf" sends a fragment and hangs
// ignoring SIGTERM, so only SIGKILL ends it, and "gone" answers HTTP 410.
type downloadWorld struct {
	dir string
}

func newDownloadWorld(t *testing.T, run int, artifacts ...fakeArtifact) downloadWorld {
	t.Helper()
	dir := t.TempDir()
	bin := mkdirAll(t, filepath.Join(dir, "bin"))
	var listing []map[string]any
	for _, a := range artifacts {
		created := a.Created
		if created == "" {
			created = "2026-11-28T00:00:00Z"
		}
		entry := map[string]any{"id": a.ID, "name": a.Name, "created_at": created, "expired": a.Expired, "workflow_run": map[string]any{"id": run}}
		listing = append(listing, entry)
		metadata, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		writeArtifact(t, dir, fmt.Sprintf("meta-%d.json", a.ID), string(metadata))
		writeZip(t, filepath.Join(dir, fmt.Sprintf("zip-%d.zip", a.ID)), a.Files)
	}
	data, err := json.Marshal(map[string]any{"total_count": len(listing), "artifacts": listing})
	if err != nil {
		t.Fatal(err)
	}
	writeArtifact(t, dir, "listing.json", string(data))
	writeFakeExecutable(t, bin, "gh", `state=`+dir+`
printf '%s\n' "$*" >> "$state/calls"
step() {
    n=$(grep -cE '/zip$|^release download' "$state/calls")
    sed -n "${n}p" "$state/plan"
}
if [[ "$1" == release ]]; then
    out=${!#}
    case "$(step)" in
        ok) printf complete > "$out/payload"; mkdir -p "$out/sub"; printf new > "$out/sub/new" ;;
        partial) printf frag > "$out/fragment"; exit 1 ;;
        stall) exec sleep 60 ;;
        deaf) trap '' TERM; printf frag > "$out/fragment"; sleep 60 ;;
    esac
    exit 0
fi
endpoint=${!#}
case "$endpoint" in
    */runs/*/artifacts*) cat "$state/listing.json" ;;
    */zip)
        id=${endpoint%/zip}
        id=${id##*/}
        case "$(step)" in
            ok) cat "$state/zip-$id.zip" ;;
            partial) head -c 20 "$state/zip-$id.zip"; exit 1 ;;
            truncated) head -c 20 "$state/zip-$id.zip" ;;
            stall) exec sleep 60 ;;
            deaf) trap '' TERM; head -c 20 "$state/zip-$id.zip"; sleep 60 ;;
            gone) echo 'gh: Gone (HTTP 410)' >&2; exit 1 ;;
        esac ;;
    *)
        id=${endpoint##*/}
        if [[ ! -e "$state/meta-$id.json" ]]; then
            echo 'gh: Not Found (HTTP 404)' >&2
            exit 1
        fi
        cat "$state/meta-$id.json" ;;
esac
`)
	return downloadWorld{dir: dir}
}

// run executes hosted-download in the world with two 1-second attempts.
func (w downloadWorld) run(t *testing.T, plan []string, args ...string) (string, error) {
	t.Helper()
	writeArtifact(t, w.dir, "plan", strings.Join(plan, "\n")+"\n")
	cmd := exec.Command(filepath.Join(repoRoot(t), ".github", "scripts", "hosted-download"), args...)
	cmd.Dir = w.dir
	cmd.Env = w.env()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

// env is the environment of a job in run 42 that downloads from the world.
func (w downloadWorld) env(extra ...string) []string {
	return append(append(os.Environ(),
		"PATH="+filepath.Join(w.dir, "bin")+":"+os.Getenv("PATH"),
		"GITHUB_ACTIONS=true",
		"GITHUB_REPOSITORY=katl-dev/katl",
		"GITHUB_RUN_ID=42",
		"KATL_DOWNLOAD_ATTEMPTS=2",
		"KATL_DOWNLOAD_ATTEMPT_SECONDS=1",
		"KATL_DOWNLOAD_KILL_AFTER_SECONDS=1",
		"KATL_DOWNLOAD_BACKOFF_SECONDS=0",
	), extra...)
}

func (w downloadWorld) calls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(w.dir, "calls"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fetches counts the archive downloads gh was asked for.
func (w downloadWorld) fetches(t *testing.T) int {
	t.Helper()
	return strings.Count(w.calls(t), "/zip\n")
}

// delivered checks the files under DEST and that no staging is left.
func (w downloadWorld) delivered(t *testing.T, dest string, want map[string]string) {
	t.Helper()
	for path, content := range want {
		if got := string(mustReadFile(t, filepath.Join(w.dir, dest, path))); got != content {
			t.Errorf("%s/%s = %q, want %q", dest, path, got, content)
		}
	}
	if siblings, _ := filepath.Glob(filepath.Join(w.dir, dest+".*")); len(siblings) != 0 {
		t.Errorf("staging directories left behind: %v", siblings)
	}
}

// undelivered checks that a failed download left neither DEST nor staging.
func (w downloadWorld) undelivered(t *testing.T, dest string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(w.dir, dest)); !os.IsNotExist(err) {
		t.Errorf("failed download left a destination: %v", err)
	}
	if siblings, _ := filepath.Glob(filepath.Join(w.dir, dest+".*")); len(siblings) != 0 {
		t.Errorf("staging directories left behind: %v", siblings)
	}
}

var completeFiles = map[string]string{"payload": "complete", "sub/new": "new"}

func TestHostedDownload(t *testing.T) {
	inputs := fakeArtifact{ID: 99, Name: "inputs-abc", Files: completeFiles}

	t.Run("retries a stalled attempt", func(t *testing.T) {
		w := newDownloadWorld(t, 42, inputs)
		stderr, err := w.run(t, []string{"stall", "ok"}, "artifact", "inputs-abc", "out")
		if err != nil {
			t.Fatalf("download failed: %v\n%s", err, stderr)
		}
		w.delivered(t, "out", completeFiles)
		if !strings.Contains(stderr, "timed out") {
			t.Errorf("stalled attempt not reported as a timeout:\n%s", stderr)
		}
	})

	t.Run("discards a partial attempt", func(t *testing.T) {
		w := newDownloadWorld(t, 42)
		stderr, err := w.run(t, []string{"partial", "ok"}, "release", "v1.0.0", "previous")
		if err != nil {
			t.Fatalf("download failed: %v\n%s", err, stderr)
		}
		w.delivered(t, "previous", completeFiles)
		if _, err := os.Stat(filepath.Join(w.dir, "previous", "fragment")); !os.IsNotExist(err) {
			t.Error("destination holds the partial attempt's fragment")
		}
	})

	t.Run("removes a killed attempt's staging", func(t *testing.T) {
		w := newDownloadWorld(t, 42, inputs)
		stderr, err := w.run(t, []string{"deaf", "ok"}, "artifact", "inputs-abc", "out")
		if err != nil {
			t.Fatalf("download failed: %v\n%s", err, stderr)
		}
		w.delivered(t, "out", completeFiles)
	})

	t.Run("gives up within its attempt budget", func(t *testing.T) {
		w := newDownloadWorld(t, 7, inputs)
		started := time.Now()
		stderr, err := w.run(t, []string{"stall", "stall", "ok"}, "artifact", "inputs-abc", "out", "7")
		if err == nil {
			t.Fatalf("download succeeded after exhausting its attempts:\n%s", stderr)
		}
		if elapsed := time.Since(started); elapsed > 20*time.Second {
			t.Errorf("two 1-second attempts took %s", elapsed)
		}
		if !strings.Contains(stderr, "::error title=Download::hosted-download: artifact inputs-abc from run 7: attempt 2/2 timed out after 1s; giving up") {
			t.Errorf("failure does not name the artifact and the reason in an error annotation:\n%s", stderr)
		}
		w.undelivered(t, "out")
		if n := w.fetches(t); n != 2 {
			t.Errorf("fetched the archive %d times; want KATL_DOWNLOAD_ATTEMPTS=2", n)
		}
	})

	t.Run("rejects a truncated archive", func(t *testing.T) {
		w := newDownloadWorld(t, 42, inputs)
		stderr, err := w.run(t, []string{"truncated", "truncated"}, "artifact", "inputs-abc", "out")
		if err == nil {
			t.Fatalf("download accepted a truncated archive:\n%s", stderr)
		}
		w.undelivered(t, "out")
		if n := w.fetches(t); n != 2 {
			t.Errorf("fetched the archive %d times; a truncated archive is retried", n)
		}
	})

	t.Run("merges into an existing destination", func(t *testing.T) {
		w := newDownloadWorld(t, 42, inputs)
		writeArtifact(t, mkdirAll(t, filepath.Join(w.dir, "out", "sub")), "old", "kept")
		writeArtifact(t, filepath.Join(w.dir, "out"), "earlier", "kept")
		stderr, err := w.run(t, []string{"ok"}, "artifact", "inputs-abc", "out")
		if err != nil {
			t.Fatalf("download failed: %v\n%s", err, stderr)
		}
		w.delivered(t, "out", map[string]string{"earlier": "kept", "sub/old": "kept", "payload": "complete", "sub/new": "new"})
	})

	t.Run("refuses to replace an existing file", func(t *testing.T) {
		w := newDownloadWorld(t, 42, inputs)
		writeArtifact(t, mkdirAll(t, filepath.Join(w.dir, "out")), "payload", "earlier")
		stderr, err := w.run(t, []string{"ok"}, "artifact", "inputs-abc", "out")
		if err == nil {
			t.Fatalf("download replaced an existing file:\n%s", stderr)
		}
		if !strings.Contains(stderr, "::error") || !strings.Contains(stderr, "out/payload") {
			t.Errorf("failure does not name the colliding path in an error annotation:\n%s", stderr)
		}
		if got := string(mustReadFile(t, filepath.Join(w.dir, "out", "payload"))); got != "earlier" {
			t.Errorf("payload = %q, want the earlier file", got)
		}
		if _, err := os.Stat(filepath.Join(w.dir, "out", "sub")); !os.IsNotExist(err) {
			t.Errorf("a refused download merged part of its files: %v", err)
		}
	})

	t.Run("asks gh for the named subject", func(t *testing.T) {
		for _, tc := range []struct {
			args []string
			want []string
		}{
			{[]string{"artifact", "inputs-abc", "out"}, []string{"api --paginate repos/katl-dev/katl/actions/runs/42/artifacts?per_page=100", "api repos/katl-dev/katl/actions/artifacts/99/zip"}},
			{[]string{"artifact", "inputs-abc", "out", "7"}, []string{"api --paginate repos/katl-dev/katl/actions/runs/7/artifacts?per_page=100", "api repos/katl-dev/katl/actions/artifacts/99/zip"}},
			{[]string{"release", "v1.0.0", "out"}, []string{"release download v1.0.0 --repo katl-dev/katl --dir "}},
			{[]string{"release", "v1.0.0", "out", "SHA256SUMS"}, []string{"release download v1.0.0 --repo katl-dev/katl --pattern SHA256SUMS --dir "}},
		} {
			w := newDownloadWorld(t, 42, inputs)
			stderr, err := w.run(t, []string{"ok"}, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, stderr)
			}
			for _, want := range tc.want {
				if calls := w.calls(t); !strings.Contains(calls, want) {
					t.Errorf("%v: gh %q; want a call %q", tc.args, calls, want)
				}
			}
		}
	})

	t.Run("fails when no artifact matches", func(t *testing.T) {
		w := newDownloadWorld(t, 42, fakeArtifact{ID: 5, Name: "other", Files: completeFiles})
		stderr, err := w.run(t, []string{"ok", "ok"}, "artifact", "inputs-abc", "out")
		if err == nil {
			t.Fatalf("download succeeded without the artifact:\n%s", stderr)
		}
		if !strings.Contains(stderr, "no unexpired artifact matching inputs-abc") {
			t.Errorf("failure does not say the artifact is missing:\n%s", stderr)
		}
		w.undelivered(t, "out")
	})

	t.Run("reports a listing it cannot select from", func(t *testing.T) {
		w := newDownloadWorld(t, 42, inputs)
		writeArtifact(t, w.dir, "listing.json", `{"artifacts": [`)
		stderr, err := w.run(t, []string{"ok", "ok"}, "artifact", "inputs-abc", "out")
		if err == nil {
			t.Fatalf("download succeeded from a malformed listing:\n%s", stderr)
		}
		if !strings.Contains(stderr, "could not select artifacts from the listing of run 42") {
			t.Errorf("failure does not report the selection error:\n%s", stderr)
		}
		if strings.Contains(stderr, "no unexpired artifact") {
			t.Errorf("selection error reported as a missing artifact:\n%s", stderr)
		}
		w.undelivered(t, "out")
	})
}

// TestHostedDownloadSelection holds name and pattern downloads to
// actions/download-artifact's rule: each name selects its newest unexpired
// artifact in the run, whatever order the run lists them in, so a re-run
// attempt gets its own artifact rather than an earlier attempt's.
func TestHostedDownloadSelection(t *testing.T) {
	earlier := fakeArtifact{ID: 10, Name: "inputs-abc", Created: "2026-11-28T01:00:00Z", Files: map[string]string{"payload": "earlier attempt"}}
	newest := fakeArtifact{ID: 11, Name: "inputs-abc", Created: "2026-11-28T02:00:00Z", Files: map[string]string{"payload": "newest attempt"}}

	t.Run("by name", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			artifacts []fakeArtifact
			want      string
		}{
			{"earlier listed first", []fakeArtifact{earlier, newest}, "newest attempt"},
			{"newest listed first", []fakeArtifact{newest, earlier}, "newest attempt"},
			{"same time, higher ID wins", []fakeArtifact{
				{ID: 21, Name: "inputs-abc", Created: "2026-11-28T03:00:00Z", Files: map[string]string{"payload": "higher ID"}},
				{ID: 20, Name: "inputs-abc", Created: "2026-11-28T03:00:00Z", Files: map[string]string{"payload": "lower ID"}},
			}, "higher ID"},
			{"newest expired", []fakeArtifact{earlier, newest, {ID: 12, Name: "inputs-abc", Created: "2026-11-28T04:00:00Z", Expired: true, Files: map[string]string{"payload": "expired"}}}, "newest attempt"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := newDownloadWorld(t, 42, tc.artifacts...)
				stderr, err := w.run(t, []string{"ok"}, "artifact", "inputs-abc", "out")
				if err != nil {
					t.Fatalf("download failed: %v\n%s", err, stderr)
				}
				w.delivered(t, "out", map[string]string{"payload": tc.want})
			})
		}
	})

	t.Run("by pattern", func(t *testing.T) {
		lts := fakeArtifact{ID: 30, Name: "inputs-lts", Created: "2026-11-28T01:30:00Z", Files: map[string]string{"payload": "lts"}}
		unmatched := fakeArtifact{ID: 40, Name: "other", Files: map[string]string{"payload": "other"}}
		for _, order := range [][]fakeArtifact{{earlier, lts, newest, unmatched}, {newest, unmatched, lts, earlier}} {
			w := newDownloadWorld(t, 42, order...)
			stderr, err := w.run(t, []string{"ok", "ok"}, "artifacts", "inputs-*", "out")
			if err != nil {
				t.Fatalf("download failed: %v\n%s", err, stderr)
			}
			w.delivered(t, "out", map[string]string{"inputs-abc/payload": "newest attempt", "inputs-lts/payload": "lts"})
			if _, err := os.Stat(filepath.Join(w.dir, "out", "other")); !os.IsNotExist(err) {
				t.Errorf("delivered an artifact the pattern does not match: %v", err)
			}
			if n := w.fetches(t); n != 2 {
				t.Errorf("fetched %d archives; want one per matching name", n)
			}
		}
	})
}

// TestReleaseExtensionDownloads runs the release workflow's this-run
// extension downloads against the real hosted-download, with the inventory
// katl-release prints, so each bundle it names must be selectable from a
// run's artifact listing and no other artifact is fetched.
func TestReleaseExtensionDownloads(t *testing.T) {
	workstationtest.Tool(t, "unzip")
	workstationtest.Tool(t, "jq")
	const commit = "0123456789abcdef0123456789abcdef01234567"
	inventory, err := exec.Command(buildCommand(t, t.TempDir(), "katl-release"), "extension-bundles", commit).Output()
	if err != nil {
		t.Fatalf("extension-bundles: %v", err)
	}
	var parsed struct{ Bundles []struct{ Name string } }
	if err := json.Unmarshal(inventory, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Bundles) < 2 {
		t.Fatalf("inventory %s names fewer than two bundles; the test needs several", inventory)
	}
	var bundles []fakeArtifact
	for i, bundle := range parsed.Bundles {
		bundles = append(bundles, fakeArtifact{ID: 10 + i, Name: bundle.Name, Files: map[string]string{"payload": bundle.Name}})
	}
	// Another commit's bundle of the same flavour.
	other := fakeArtifact{ID: 90, Name: strings.Replace(bundles[0].Name, commit, strings.Repeat("f", 40), 1), Files: map[string]string{"payload": "other"}}

	workflow, _ := readReleaseWorkflow(t)
	steps := map[string]string{}
	for job, name := range map[string]string{
		"publish-candidate": "Download candidate extension publication inputs",
		"publish":           "Download final extension publication inputs",
	} {
		for _, step := range workflow.Jobs[job].Steps {
			if step.Name == name {
				steps[job] = strings.ReplaceAll(step.Run, ".github/scripts/hosted-download", filepath.Join(repoRoot(t), ".github", "scripts", "hosted-download"))
			}
		}
		if steps[job] == "" {
			t.Fatalf("%s has no step %q", job, name)
		}
	}

	for job, script := range steps {
		t.Run(job, func(t *testing.T) {
			t.Run("delivers every bundle", func(t *testing.T) {
				w := newDownloadWorld(t, 42, append([]fakeArtifact{other}, bundles...)...)
				writeArtifact(t, w.dir, "plan", strings.Repeat("ok\n", len(bundles)))
				out, err := runStepIn(w, script, "EXTENSION_BUNDLES="+string(inventory), "RECORDED=", "BUILD_RUN=42")
				if err != nil {
					t.Fatalf("download failed: %v\n%s", err, out)
				}
				want := map[string]string{}
				for _, bundle := range bundles {
					want[bundle.Name+"/payload"] = bundle.Name
				}
				w.delivered(t, "artifacts", want)
				if n := w.fetches(t); n != len(bundles) {
					t.Errorf("fetched %d archives; want one per inventory bundle", n)
				}
			})

			t.Run("fails when a bundle is missing", func(t *testing.T) {
				w := newDownloadWorld(t, 42, other, bundles[0])
				writeArtifact(t, w.dir, "plan", strings.Repeat("ok\n", len(bundles)))
				out, err := runStepIn(w, script, "EXTENSION_BUNDLES="+string(inventory), "RECORDED=", "BUILD_RUN=42")
				if err == nil {
					t.Fatalf("download succeeded without %s:\n%s", bundles[1].Name, out)
				}
				if _, err := os.Stat(filepath.Join(w.dir, "artifacts", bundles[1].Name)); !os.IsNotExist(err) {
					t.Errorf("missing bundle left a destination: %v", err)
				}
			})
		})
	}
}

// runStepIn runs a workflow step's script in the world as a job's default
// bash shell does.
func runStepIn(w downloadWorld, script string, env ...string) ([]byte, error) {
	cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", script)
	cmd.Dir = w.dir
	cmd.Env = w.env(env...)
	return cmd.CombinedOutput()
}

// TestHostedDownloadByID covers --id, which binds a download to one artifact
// of one run and checks that the artifact is the one the caller named.
func TestHostedDownloadByID(t *testing.T) {
	workstationtest.Tool(t, "unzip")
	inputs := fakeArtifact{ID: 99, Name: "inputs-abc", Files: completeFiles}
	args := []string{"--id", "99", "artifact", "inputs-abc", "out", "7"}

	t.Run("delivers the artifact with that ID", func(t *testing.T) {
		w := newDownloadWorld(t, 7, inputs)
		stderr, err := w.run(t, []string{"ok"}, args...)
		if err != nil {
			t.Fatalf("download failed: %v\n%s", err, stderr)
		}
		w.delivered(t, "out", completeFiles)
		if got := w.calls(t); !strings.Contains(got, "api repos/katl-dev/katl/actions/artifacts/99/zip\n") {
			t.Errorf("gh calls %q do not fetch artifact 99", got)
		}
	})

	t.Run("retries a failed attempt", func(t *testing.T) {
		w := newDownloadWorld(t, 7, inputs)
		stderr, err := w.run(t, []string{"partial", "ok"}, args...)
		if err != nil {
			t.Fatalf("download failed: %v\n%s", err, stderr)
		}
		w.delivered(t, "out", completeFiles)
		if !strings.Contains(stderr, "attempt 1/2 exited") {
			t.Errorf("failed attempt not reported:\n%s", stderr)
		}
	})

	t.Run("delivers nothing from killed attempts", func(t *testing.T) {
		w := newDownloadWorld(t, 7, inputs)
		started := time.Now()
		stderr, err := w.run(t, []string{"deaf", "deaf"}, args...)
		if err == nil {
			t.Fatalf("download succeeded after its attempts were killed:\n%s", stderr)
		}
		if elapsed := time.Since(started); elapsed > 20*time.Second {
			t.Errorf("two killed 1-second attempts took %s", elapsed)
		}
		if !strings.Contains(stderr, "hosted-download: artifact 99 (inputs-abc) from run 7: attempt 2/2 timed out") {
			t.Errorf("failure does not name the artifact and the timeout:\n%s", stderr)
		}
		w.undelivered(t, "out")
	})

	t.Run("fails once on an artifact it cannot have", func(t *testing.T) {
		for _, tc := range []struct {
			name, id, want string
			run            int
			artifact       fakeArtifact
			plan           []string
			fetches        int
		}{
			{"other name", "99", "the ID names inputs-def from run 7", 7, fakeArtifact{ID: 99, Name: "inputs-def", Files: completeFiles}, []string{"ok", "ok"}, 0},
			{"other run", "99", "the ID names inputs-abc from run 8", 8, inputs, []string{"ok", "ok"}, 0},
			{"expired", "99", "artifact 99 has expired", 7, fakeArtifact{ID: 99, Name: "inputs-abc", Expired: true, Files: completeFiles}, []string{"ok", "ok"}, 0},
			{"missing", "98", "artifact 98 does not exist", 7, inputs, []string{"ok", "ok"}, 0},
			{"archive gone", "99", "artifact 99 is missing or has expired", 7, inputs, []string{"gone", "ok"}, 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := newDownloadWorld(t, tc.run, tc.artifact)
				stderr, err := w.run(t, tc.plan, "--id", tc.id, "artifact", "inputs-abc", "out", "7")
				if err == nil {
					t.Fatalf("download succeeded:\n%s", stderr)
				}
				if !strings.Contains(stderr, "::error") || !strings.Contains(stderr, tc.want) {
					t.Errorf("failure does not report %q in an error annotation:\n%s", tc.want, stderr)
				}
				if strings.Contains(stderr, "attempt 2/2") {
					t.Errorf("a permanent failure was retried:\n%s", stderr)
				}
				if n := w.fetches(t); n != tc.fetches {
					t.Errorf("fetched the archive %d times; want %d", n, tc.fetches)
				}
				w.undelivered(t, "out")
			})
		}
	})

	t.Run("rejects an ID with another selector", func(t *testing.T) {
		for _, tc := range [][]string{
			{"--id", "99", "artifacts", "inputs-*", "out", "7"},
			{"--id", "99", "release", "v1.0.0", "out"},
			{"--id", "99", "artifact", "inputs-abc", "out"},
			{"--id", "inputs-abc", "artifact", "inputs-abc", "out", "7"},
			{"--id", "99", "artifact", "inputs-abc", "out", "7", "extra"},
		} {
			w := newDownloadWorld(t, 7, inputs)
			stderr, err := w.run(t, []string{"ok"}, tc...)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Errorf("%v: err %v; want usage error (exit 2)\n%s", tc, err, stderr)
			}
			if got := w.calls(t); got != "" {
				t.Errorf("%v: called gh %q", tc, got)
			}
		}
	})
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, archive.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
