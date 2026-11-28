package main

import (
	"os"
	"testing"

	"github.com/katl-dev/katl/internal/workstationtest"
)

func TestMain(m *testing.M) {
	// TestVerifierKeepsCallerPath re-executes this binary through sudo as the
	// verifier; report where the child resolves the tool named as its runtime.
	if len(os.Args) == 5 && os.Args[1] == "verify-release-extensions" {
		resolved, err := workstationtest.LookupTool(os.Args[3])
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
