package scriptstest

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A descendant of a test subprocess must not outlive the test, whether the
// subprocess hit its timeout or exited normally leaving the descendant behind.
func TestRunInGroupKillsDescendants(t *testing.T) {
	cases := []struct {
		name     string
		script   string
		timeout  time.Duration
		timedOut bool
	}{
		{"timeout", `sleep 600 & echo $!; wait`, 500 * time.Millisecond, true},
		{"exit", `sleep 600 & echo $!`, 10 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pid int
			t.Run("run", func(t *testing.T) {
				out, err := runInGroup(t, exec.Command("bash", "-c", tc.script), tc.timeout)
				if errors.Is(err, errScriptTimeout) != tc.timedOut {
					t.Fatalf("err = %v, timedOut want %v", err, tc.timedOut)
				}
				var perr error
				if pid, perr = strconv.Atoi(strings.TrimSpace(string(out))); perr != nil {
					t.Fatalf("grandchild pid from %q: %v", out, perr)
				}
			})

			deadline := time.Now().Add(5 * time.Second)
			for {
				err := syscall.Kill(pid, 0)
				if errors.Is(err, syscall.ESRCH) {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("grandchild %d outlived its test (kill 0: %v)", pid, err)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}
