package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/katl-dev/katl/internal/workstationtest"
)

func TestMain(m *testing.M) {
	// TestVerifyReleaseImagesKeepsCallerPath re-executes this binary through
	// sudo as the verifier; report which tool the child resolves.
	if len(os.Args) == 5 && os.Args[1] == "verify-release-extensions" {
		resolved, err := exec.LookPath(os.Args[3])
		if err != nil {
			resolved = "missing: " + err.Error()
		}
		if err := os.WriteFile(os.Args[2], []byte(resolved), 0o644); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(workstationtest.Run(m, nil))
}
