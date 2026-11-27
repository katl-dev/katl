package scriptstest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scripts/retry is the shell retry owner. Wrong implementations this
// separates: retrying without a per-attempt limit, making more or fewer
// attempts than asked, retrying silently, and passing a malformed override
// through to arithmetic or to the command.
func TestRetry(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "retry")

	// The command fails, stalls or succeeds per attempt as its plan lists.
	run := func(t *testing.T, plan []string, args ...string) (int, string, int) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "plan"), []byte(strings.Join(plan, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		command := writeFakeExecutable(t, dir, "attempt", `echo >> "$1/calls"
case "$(sed -n "$(wc -l < "$1/calls")p" "$1/plan")" in
    ok) exit 0 ;;
    stall) exec sleep 60 ;;
    *) exit 3 ;;
esac
`)
		cmd := exec.Command(script, append(args, "--title", "Fetch retry", "--what", "fetch thing", "--", command, dir)...)
		cmd.Env = append(os.Environ(), "GITHUB_ACTIONS=true")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		err := cmd.Run()
		exit := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		calls := 0
		if data, err := os.ReadFile(filepath.Join(dir, "calls")); err == nil {
			calls = strings.Count(string(data), "\n")
		}
		return exit, stderr.String(), calls
	}
	limits := []string{"--attempts", "3", "--backoff", "0", "--timeout", "1"}

	t.Run("retries until an attempt succeeds", func(t *testing.T) {
		exit, stderr, calls := run(t, []string{"fail", "ok"}, limits...)
		if exit != 0 || calls != 2 {
			t.Fatalf("exit %d after %d attempts:\n%s", exit, calls, stderr)
		}
		if !strings.Contains(stderr, "::warning title=Fetch retry::fetch thing: attempt 1/3 exited 3") {
			t.Errorf("retry has no warning annotation:\n%s", stderr)
		}
	})

	t.Run("bounds each attempt", func(t *testing.T) {
		started := time.Now()
		exit, stderr, calls := run(t, []string{"stall", "ok"}, limits...)
		if exit != 0 || calls != 2 {
			t.Fatalf("exit %d after %d attempts:\n%s", exit, calls, stderr)
		}
		if elapsed := time.Since(started); elapsed > 20*time.Second {
			t.Errorf("a 1-second attempt limit took %s", elapsed)
		}
		if !strings.Contains(stderr, "timed out after 1s") {
			t.Errorf("stalled attempt not reported as a timeout:\n%s", stderr)
		}
	})

	t.Run("gives up after its attempts", func(t *testing.T) {
		exit, stderr, calls := run(t, []string{"fail", "fail", "fail", "ok"}, limits...)
		if exit != 1 || calls != 3 {
			t.Fatalf("exit %d after %d attempts, want exit 1 after 3:\n%s", exit, calls, stderr)
		}
		if strings.Count(stderr, "::warning") != 2 {
			t.Errorf("want a retry annotation before each of the two retries:\n%s", stderr)
		}
	})

	t.Run("rejects malformed limits", func(t *testing.T) {
		for _, bad := range [][]string{
			{"--attempts", "0", "--backoff", "0", "--timeout", "1"},
			{"--attempts", "three", "--backoff", "0", "--timeout", "1"},
			{"--attempts", "2", "--backoff", "-1", "--timeout", "1"},
			{"--attempts", "2", "--backoff", "0", "--timeout", "0"},
			{"--attempts", "2", "--backoff", "0", "--timeout", ""},
		} {
			exit, stderr, calls := run(t, []string{"ok"}, bad...)
			if exit != 2 || calls != 0 {
				t.Errorf("%v: exit %d after %d attempts, want exit 2 before any:\n%s", bad, exit, calls, stderr)
			}
		}
	})
}
