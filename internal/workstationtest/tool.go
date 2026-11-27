package workstationtest

import (
	"os"
	"os/exec"
	"testing"
)

// Tool returns the path to a test prerequisite. Inside a Katl shell, a
// missing tool fails the test because the shell declares its dependencies.
func Tool(t testing.TB, name string) string {
	t.Helper()
	path, err := LookupTool(name)
	if err != nil {
		if os.Getenv("KATL_REQUIRE_TOOLS") == "1" {
			t.Fatalf("required tool %q: %v", name, err)
		}
		t.Skipf("required tool %q: %v", name, err)
	}
	return path
}

// LookupTool reports availability to callers that record VM host prerequisites
// or link optional programs into fixtures. Use Tool for test prerequisites.
func LookupTool(name string) (string, error) {
	return exec.LookPath(name)
}
