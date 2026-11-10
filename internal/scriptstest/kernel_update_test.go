package scriptstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKernelUpdateIsolation(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/kernel-updates.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var caller struct {
		Jobs map[string]struct {
			Uses     string
			Strategy struct {
				FailFast *bool `yaml:"fail-fast"`
			} `yaml:"strategy"`
		}
	}
	if err := yaml.Unmarshal(data, &caller); err != nil {
		t.Fatal(err)
	}
	update := caller.Jobs["update"]
	if update.Strategy.FailFast == nil || *update.Strategy.FailFast {
		t.Fatal("one flavour failure can cancel the other update")
	}
	if !strings.HasPrefix(update.Uses, "./.github/workflows/") {
		t.Fatal("the flavour matrix must run the complete pipeline independently")
	}

	data, err = os.ReadFile(filepath.Join(repoRoot(t), strings.TrimPrefix(update.Uses, "./")))
	if err != nil {
		t.Fatal(err)
	}
	var pipeline struct {
		Concurrency struct{ Group string }
		Jobs        map[string]struct {
			Steps []struct {
				Uses string
				With map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &pipeline); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pipeline.Concurrency.Group, "inputs.flavour") {
		t.Fatal("flavours share a concurrency group")
	}
	for name, job := range pipeline.Jobs {
		for _, step := range job.Steps {
			if strings.HasPrefix(step.Uses, "actions/upload-artifact@") || strings.HasPrefix(step.Uses, "actions/download-artifact@") {
				if !strings.Contains(step.With["name"], "inputs.flavour") || !strings.Contains(step.With["path"], "inputs.flavour") {
					t.Fatalf("%s shares candidate artifacts between flavours", name)
				}
			}
		}
	}
}
