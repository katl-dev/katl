package generation

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestPublishedModuleSetRequiresMatchingIndexes(t *testing.T) {
	root := t.TempDir()
	record, err := NewFirstInstallRecord(validFirstInstallRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	record.Confexts[0].Path = filepath.Join(GenerationRecordsDir, record.GenerationID, "confext")
	contract := kernelmodule.Contract{
		Target: kernelmodule.Target{
			Release:       "6.12.1",
			RuntimeSHA256: record.Root.RuntimeArtifactSHA256,
		},
		Modules: []kernelmodule.Module{{
			Name:   "example",
			Path:   "usr/lib/modules/6.12.1/extra/example.ko",
			SHA256: strings.Repeat("d", 64),
		}},
	}
	extension := ExtensionRef{
		Name:            "driver",
		Path:            filepath.Join(GenerationRecordsDir, record.GenerationID, "sysext/driver.raw"),
		ActivationPath:  "/run/extensions/driver.raw",
		SHA256:          strings.Repeat("b", 64),
		ArtifactVersion: "1",
		PayloadVersion:  "1",
		Architecture:    record.Root.Architecture,
		Compatibility: ExtensionCompatibility{
			RuntimeInterfaces: []string{record.Root.RuntimeInterface},
			Kernel:            &contract,
		},
	}
	record.Sysexts = []ExtensionRef{extension}
	spec := SpecFromRecord(record)
	status, err := NewGenerationStatus(spec, CommitStateCandidate, BootStatePending, HealthStateUnknown, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteGeneration(root, spec, status); err == nil || !strings.Contains(err.Error(), "requires dependency indexes") {
		t.Fatalf("published incomplete module selection: %v", err)
	}
	selection, err := kernelmodule.IndexInputs([]kernelmodule.Contract{contract})
	if err != nil {
		t.Fatal(err)
	}
	indexes := extension
	indexes.Name = kernelmodule.IndexExtensionName
	indexes.Path = filepath.Join(GenerationRecordsDir, record.GenerationID, "sysext", kernelmodule.IndexExtensionName+".raw")
	indexes.ActivationPath = "/run/extensions/" + kernelmodule.IndexExtensionName + ".raw"
	indexes.Compatibility.Kernel = nil
	indexes.Compatibility.ModuleIndexes = &selection
	record.Sysexts = append(record.Sysexts, indexes)
	spec = SpecFromRecord(record)
	status, err = NewGenerationStatus(spec, CommitStateCandidate, BootStatePending, HealthStateUnknown, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanActivation(record); err != nil {
		t.Fatal(err)
	}

	contract.Modules[0].SHA256 = strings.Repeat("e", 64)
	if _, err := PlanActivation(record); err == nil || !strings.Contains(err.Error(), "requires dependency indexes") {
		t.Fatalf("activated stale indexes after a same-kernel driver change: %v", err)
	}
}
