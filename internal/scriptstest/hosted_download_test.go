package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var goTestTimeout = regexp.MustCompile(`-timeout (\d+)m\b`)

// TestVMLaneBudgets checks that every VM lane takes its inputs through
// hosted-download, and that a lane that does so bounds every step so that
// go test's own timeout fires before the job timeout.
func TestVMLaneBudgets(t *testing.T) {
	type step struct {
		Name    string `yaml:"name"`
		Uses    string `yaml:"uses"`
		Run     string `yaml:"run"`
		Timeout int    `yaml:"timeout-minutes"`
	}
	var budgeted int
	for _, name := range workflowNames(t) {
		var workflow struct {
			Jobs map[string]struct {
				Timeout int    `yaml:"timeout-minutes"`
				Steps   []step `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(mustReadFile(t, filepath.Join(repoRoot(t), ".github", "workflows", name)), &workflow); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for jobName, job := range workflow.Jobs {
			where := name + " job " + jobName
			var vmLane, owned bool
			for _, s := range job.Steps {
				vmLane = vmLane || strings.Contains(s.Run, "hosted-vmtest-run")
				owned = owned || strings.Contains(s.Run, "hosted-download")
			}
			if vmLane {
				for _, s := range job.Steps {
					if strings.HasPrefix(s.Uses, "actions/download-artifact@") {
						t.Errorf("%s: VM inputs must be downloaded with .github/scripts/hosted-download, not %s", where, s.Uses)
					}
				}
			}
			if !owned {
				continue
			}
			budgeted++

			var setup, sum int
			for i, s := range job.Steps {
				label := s.Name
				if label == "" {
					label = s.Uses
				}
				if s.Timeout <= 0 {
					t.Errorf("%s: step %d (%s) has no timeout-minutes", where, i, label)
				}
				sum += s.Timeout
				if m := goTestTimeout.FindStringSubmatch(s.Run); m != nil {
					minutes, _ := strconv.Atoi(m[1])
					if s.Timeout <= minutes {
						t.Errorf("%s: step %s has %d minutes for go test -timeout %dm", where, label, s.Timeout, minutes)
					}
					setup = sum - s.Timeout
					if job.Timeout <= setup+minutes {
						t.Errorf("%s: job timeout %d does not exceed setup %d plus go test -timeout %dm", where, job.Timeout, setup, minutes)
					}
				}
			}
			if job.Timeout < sum {
				t.Errorf("%s: job timeout %d is below the step budgets' sum %d", where, job.Timeout, sum)
			}
		}
	}
	if budgeted < 2 {
		t.Fatalf("found %d jobs using hosted-download; expected the hosted and production VM lanes", budgeted)
	}
}

func TestHostedDownload(t *testing.T) {
	script := filepath.Join(repoRoot(t), ".github", "scripts", "hosted-download")

	// The fake gh behaves per attempt as listed in its plan file: "ok" writes a
	// complete payload, "partial" writes a fragment and fails, "stall" hangs,
	// and "deaf" writes a fragment and hangs ignoring SIGTERM, so only SIGKILL
	// ends it.
	run := func(t *testing.T, plan []string, args ...string) (string, string, error) {
		t.Helper()
		dir := t.TempDir()
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
    ok) printf complete > "$out/payload" ;;
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
		if len(entries) != 1 || entries[0].Name() != "payload" {
			t.Errorf("destination holds %v; want only the complete attempt's payload", entries)
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

	t.Run("asks gh for the named subject", func(t *testing.T) {
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"artifact", "inputs-abc", "out"}, "run download 42 --repo katl-dev/katl --name inputs-abc --dir "},
			{[]string{"artifact", "inputs-abc", "out", "7"}, "run download 7 --repo katl-dev/katl --name inputs-abc --dir "},
			{[]string{"release", "v1.0.0", "out"}, "release download v1.0.0 --repo katl-dev/katl --dir "},
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
