package vmtest

import (
	"os"
	"testing"

	"github.com/katl-dev/katl/internal/workstationtest"
)

func TestMain(m *testing.M) {
	os.Exit(workstationtest.Run(m, nil))
}
