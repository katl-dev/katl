package scriptstest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCIGate(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ Run string }
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	script := workflow.Jobs["result"].Steps[0].Run
	for _, tc := range []struct {
		name, checks, vms, drivers, vmFlavours, driverFlavours string
		pass                                                   bool
	}{
		{"documentation", "success", "skipped", "skipped", `[]`, `[]`, true},
		{"product", "success", "success", "skipped", `["standard"]`, `[]`, true},
		{"kernel", "success", "success", "success", `["lts"]`, `["lts"]`, true},
		{"fast failure", "failure", "skipped", "skipped", `[]`, `[]`, false},
		{"fast cancelled", "cancelled", "skipped", "skipped", `[]`, `[]`, false},
		{"VM failure", "success", "failure", "success", `["lts"]`, `["lts"]`, false},
		{"VM cancelled", "success", "cancelled", "success", `["lts"]`, `["lts"]`, false},
		{"selected VM skipped", "success", "skipped", "skipped", `["standard"]`, `[]`, false},
		{"driver failure", "success", "success", "failure", `["lts"]`, `["lts"]`, false},
		{"selected driver skipped", "success", "success", "skipped", `["lts"]`, `["lts"]`, false},
		{"missing plan", "success", "skipped", "skipped", ``, ``, false},
		{"invalid plan", "success", "skipped", "skipped", `false`, `[]`, false},
		{"unexpected execution", "success", "success", "skipped", `[]`, `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs, err := json.Marshal(map[string]any{
				"checks": map[string]any{"result": tc.checks, "outputs": map[string]string{
					"vm-flavours": tc.vmFlavours, "driver-flavours": tc.driverFlavours,
				}},
				"vms": map[string]string{"result": tc.vms}, "drivers": map[string]string{"result": tc.drivers},
			})
			if err != nil {
				t.Fatal(err)
			}

			command := exec.Command("bash", "-euo", "pipefail", "-c", script)
			command.Env = append(os.Environ(), "NEEDS="+string(needs))
			output, err := command.CombinedOutput()
			if (err == nil) != tc.pass {
				t.Fatalf("CI passed = %v, want %v: %s", err == nil, tc.pass, output)
			}
		})
	}
}
