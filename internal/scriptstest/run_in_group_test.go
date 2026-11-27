package scriptstest

import (
	"bytes"
	"context"
	"errors"
	"io"
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
		timedOut bool
	}{
		{"timeout", `sleep 600 >/dev/null 2>&1 & echo $!; wait`, true},
		{"exit", `sleep 600 >/dev/null 2>&1 & echo $!`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pid int
			t.Run("run", func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var output io.Writer = io.Discard
				if tc.timedOut {
					output = &expireOnPID{expire: cancel}
				}
				out, err := runInGroupContext(t, ctx, exec.Command("bash", "-c", tc.script), 10*time.Second, output)
				if tc.timedOut && !errors.Is(err, errScriptTimeout) || !tc.timedOut && err != nil {
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

// expireOnPID triggers the timeout branch after the shell has reported the
// descendant that cleanup must kill.
type expireOnPID struct {
	expire func()
}

func (w *expireOnPID) Write(data []byte) (int, error) {
	if bytes.IndexByte(data, '\n') >= 0 {
		w.expire()
	}
	return len(data), nil
}
