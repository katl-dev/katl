package generation

import (
	"fmt"

	"github.com/katl-dev/katl/internal/kernelmodule"
)

// A planning spec may be incomplete while artifacts are prepared. Publishing
// or activating it requires one index image for the exact selected module set.
func validateModuleIndexes(refs []ExtensionRef) error {
	var modules []kernelmodule.Contract
	var indexes *kernelmodule.IndexSelection
	for _, ref := range refs {
		if contract := ref.Compatibility.Kernel; contract != nil {
			modules = append(modules, *contract)
		}
		if target := ref.Compatibility.ModuleIndexes; target != nil {
			if indexes != nil {
				return fmt.Errorf("generation contains multiple module index images")
			}
			indexes = target
		}
	}
	if modules == nil && indexes != nil {
		return fmt.Errorf("generation retains module indexes without selected kernel extensions")
	}
	if modules != nil {
		selection, err := kernelmodule.IndexInputs(modules)
		if err != nil {
			return err
		}
		if indexes == nil || selection != *indexes {
			return fmt.Errorf("generation requires dependency indexes for its selected kernel modules")
		}
	}
	return nil
}
