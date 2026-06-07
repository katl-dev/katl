package installer

import (
	"context"
	"strings"
	"testing"
)

func TestExecCommandRunnerReportsFailedCommandOutput(t *testing.T) {
	err := NewExecCommandRunner().RunInput(context.Background(), "stdin text", "sh", "-c", "cat; echo stderr text >&2; exit 7")
	if err == nil {
		t.Fatal("RunInput() error = nil, want failure")
	}
	got := err.Error()
	if !strings.Contains(got, "stdin text") || !strings.Contains(got, "stderr text") || !strings.Contains(got, "exit status 7") {
		t.Fatalf("RunInput() error = %q, want command output and exit status", got)
	}
}
