package main

import (
	"os"
	"testing"

	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/workstationtest"
)

func TestMain(m *testing.M) {
	os.Exit(workstationtest.Run(m, &workstation.TestPathCheck))
}
