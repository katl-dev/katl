package scriptstest

import (
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

func (s budgetStep) downloadBudget() int {
	if strings.Contains(s.Run, "hosted-download") {
		return downloadWorstCase(s.Env)
	}
	return 0
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

var hostedDownloadCall = regexp.MustCompile(`hosted-download (artifacts?|release) (\S+) (\S+)`)

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

func TestHostedDownload(t *testing.T) {
	script := filepath.Join(repoRoot(t), ".github", "scripts", "hosted-download")

	// The fake gh behaves per attempt as listed in its plan file: "ok" writes a
	// complete payload, "partial" writes a fragment and fails, "stall" hangs,
	// and "deaf" writes a fragment and hangs ignoring SIGTERM, so only SIGKILL
	// ends it.
	runIn := func(t *testing.T, dir string, plan []string, args ...string) (string, string, error) {
		t.Helper()
		bin := filepath.Join(dir, "bin")
		if err := os.Mkdir(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plan"), []byte(strings.Join(plan, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeFakeExecutable(t, bin, "gh", `state=`+dir+`
printf '%s\n' "$*" >> "$state/calls"
n=$(wc -l < "$state/calls")
mode=$(sed -n "${n}p" "$state/plan")
out=${!#}
case "$mode" in
    ok) printf complete > "$out/payload"; mkdir -p "$out/sub"; printf new > "$out/sub/new" ;;
    partial) printf frag > "$out/fragment"; exit 1 ;;
    stall) exec sleep 60 ;;
    deaf) trap '' TERM; printf frag > "$out/fragment"; sleep 60 ;;
esac
`)
		cmd := exec.Command(script, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"),
			"GITHUB_ACTIONS=true",
			"GITHUB_REPOSITORY=katl-dev/katl",
			"GITHUB_RUN_ID=42",
			"KATL_DOWNLOAD_ATTEMPTS=2",
			"KATL_DOWNLOAD_ATTEMPT_SECONDS=1",
			"KATL_DOWNLOAD_KILL_AFTER_SECONDS=1",
			"KATL_DOWNLOAD_BACKOFF_SECONDS=0",
		)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		err := cmd.Run()
		return dir, stderr.String(), err
	}
	run := func(t *testing.T, plan []string, args ...string) (string, string, error) {
		t.Helper()
		return runIn(t, t.TempDir(), plan, args...)
	}

	t.Run("retries a stalled attempt", func(t *testing.T) {
		dir, stderr, err := run(t, []string{"stall", "ok"}, "artifact", "inputs-abc", "out")
		if err != nil {
			t.Fatalf("download failed: %v\n%s", err, stderr)
		}
		if got := string(mustReadFile(t, filepath.Join(dir, "out", "payload"))); got != "complete" {
			t.Errorf("payload = %q", got)
		}
		if !strings.Contains(stderr, "timed out") {
			t.Errorf("stalled attempt not reported as a timeout:\n%s", stderr)
		}
	})

	t.Run("discards a partial attempt", func(t *testing.T) {
		dir, stderr, err := run(t, []string{"partial", "ok"}, "release", "v1.0.0", "previous")
		if err != nil {
			t.Fatalf("download failed: %v\n%s", err, stderr)
		}
		entries, err := os.ReadDir(filepath.Join(dir, "previous"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "previous", "fragment")); !os.IsNotExist(err) {
			t.Errorf("destination holds %v; want only the complete attempt's files", entries)
		}
		siblings, _ := filepath.Glob(filepath.Join(dir, "previous.*"))
		if len(siblings) != 0 {
			t.Errorf("staging directories left behind: %v", siblings)
		}
	})

	t.Run("removes a killed attempt's staging", func(t *testing.T) {
		dir, stderr, err := run(t, []string{"deaf", "ok"}, "artifact", "inputs-abc", "out")
		if err != nil {
			t.Fatalf("download failed: %v\n%s", err, stderr)
		}
		if got := string(mustReadFile(t, filepath.Join(dir, "out", "payload"))); got != "complete" {
			t.Errorf("payload = %q", got)
		}
		siblings, _ := filepath.Glob(filepath.Join(dir, "out.*"))
		if len(siblings) != 0 {
			t.Errorf("killed attempt left staging behind: %v", siblings)
		}
	})

	t.Run("gives up within its attempt budget", func(t *testing.T) {
		started := time.Now()
		dir, stderr, err := run(t, []string{"stall", "stall", "ok"}, "artifact", "inputs-abc", "out", "7")
		if err == nil {
			t.Fatalf("download succeeded after exhausting its attempts:\n%s", stderr)
		}
		if elapsed := time.Since(started); elapsed > 20*time.Second {
			t.Errorf("two 1-second attempts took %s", elapsed)
		}
		if !strings.Contains(stderr, "::error title=Download::hosted-download: artifact inputs-abc from run 7: attempt 2/2 timed out after 1s; giving up") {
			t.Errorf("failure does not name the artifact and the reason in an error annotation:\n%s", stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
			t.Errorf("failed download left a destination: %v", err)
		}
		calls := strings.Count(string(mustReadFile(t, filepath.Join(dir, "calls"))), "\n")
		if calls != 2 {
			t.Errorf("gh ran %d times; want KATL_DOWNLOAD_ATTEMPTS=2", calls)
		}
	})

	t.Run("merges into an existing destination", func(t *testing.T) {
		dir := t.TempDir()
		writeArtifact(t, mkdirAll(t, filepath.Join(dir, "out", "sub")), "old", "kept")
		writeArtifact(t, filepath.Join(dir, "out"), "earlier", "kept")
		_, stderr, err := runIn(t, dir, []string{"ok"}, "artifact", "inputs-abc", "out")
		if err != nil {
			t.Fatalf("download failed: %v\n%s", err, stderr)
		}
		for path, want := range map[string]string{"earlier": "kept", "sub/old": "kept", "payload": "complete", "sub/new": "new"} {
			if got := string(mustReadFile(t, filepath.Join(dir, "out", path))); got != want {
				t.Errorf("%s = %q, want %q", path, got, want)
			}
		}
		if siblings, _ := filepath.Glob(filepath.Join(dir, "out.*")); len(siblings) != 0 {
			t.Errorf("staging directories left behind: %v", siblings)
		}
	})

	t.Run("refuses to replace an existing file", func(t *testing.T) {
		dir := t.TempDir()
		writeArtifact(t, mkdirAll(t, filepath.Join(dir, "out")), "payload", "earlier")
		_, stderr, err := runIn(t, dir, []string{"ok"}, "artifact", "inputs-abc", "out")
		if err == nil {
			t.Fatalf("download replaced an existing file:\n%s", stderr)
		}
		if !strings.Contains(stderr, "::error") || !strings.Contains(stderr, "out/payload") {
			t.Errorf("failure does not name the colliding path in an error annotation:\n%s", stderr)
		}
		if got := string(mustReadFile(t, filepath.Join(dir, "out", "payload"))); got != "earlier" {
			t.Errorf("payload = %q, want the earlier file", got)
		}
		if _, err := os.Stat(filepath.Join(dir, "out", "sub")); !os.IsNotExist(err) {
			t.Errorf("a refused download merged part of its files: %v", err)
		}
	})

	t.Run("asks gh for the named subject", func(t *testing.T) {
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"artifact", "inputs-abc", "out"}, "run download 42 --repo katl-dev/katl --name inputs-abc --dir "},
			{[]string{"artifact", "inputs-abc", "out", "7"}, "run download 7 --repo katl-dev/katl --name inputs-abc --dir "},
			{[]string{"artifacts", "katl-qualified-*", "out"}, "run download 42 --repo katl-dev/katl --pattern katl-qualified-* --dir "},
			{[]string{"release", "v1.0.0", "out"}, "release download v1.0.0 --repo katl-dev/katl --dir "},
			{[]string{"release", "v1.0.0", "out", "SHA256SUMS"}, "release download v1.0.0 --repo katl-dev/katl --pattern SHA256SUMS --dir "},
		} {
			dir, stderr, err := run(t, []string{"ok"}, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, stderr)
			}
			if calls := string(mustReadFile(t, filepath.Join(dir, "calls"))); !strings.HasPrefix(calls, tc.want) {
				t.Errorf("%v: gh %q; want prefix %q", tc.args, calls, tc.want)
			}
		}
	})
}

func mkdirAll(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
