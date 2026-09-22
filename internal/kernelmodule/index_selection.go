package kernelmodule

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// IndexSelection binds generated indexes to both the base kernel and the
// complete selected module inventory. Matching only the kernel would allow
// stale indexes to survive adding or removing a driver on the same runtime.
type IndexSelection struct {
	Target        Target `json:"target"`
	ModulesSHA256 string `json:"modulesSHA256"`
}

func (selection IndexSelection) Validate() error {
	if err := selection.Target.Validate(); err != nil {
		return err
	}
	return validateDigest(selection.ModulesSHA256)
}

func IndexInputs(contracts []Contract) (IndexSelection, error) {
	var target Target
	modules := map[string]Module{}
	for _, contract := range contracts {
		if err := contract.Validate(); err != nil {
			return IndexSelection{}, err
		}
		if target.Release != "" && target != contract.Target {
			return IndexSelection{}, fmt.Errorf("selected module bundles target different kernel builds")
		}
		target = contract.Target
		for _, module := range contract.Modules {
			module.Name = moduleName(module.Name)
			if module.Replaces != "" {
				module.Replaces = moduleName(module.Replaces)
			}
			if previous, exists := modules[module.Name]; exists && previous != module {
				return IndexSelection{}, fmt.Errorf("module %q has conflicting inventories", module.Name)
			}
			modules[module.Name] = module
		}
	}
	if len(modules) == 0 {
		return IndexSelection{}, fmt.Errorf("module index selection requires a module inventory")
	}
	ordered := make([]Module, 0, len(modules))
	for _, module := range modules {
		ordered = append(ordered, module)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	data, err := json.Marshal(ordered)
	if err != nil {
		return IndexSelection{}, err
	}
	digest := sha256.Sum256(data)
	return IndexSelection{
		Target:        target,
		ModulesSHA256: hex.EncodeToString(digest[:]),
	}, nil
}
