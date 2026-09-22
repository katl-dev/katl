package kernelmodule

import (
	"strings"
	"testing"
)

func TestRuntimeBinding(t *testing.T) {
	contract := Contract{
		Target: Target{
			Release:       "6.12.1-katl",
			RuntimeSHA256: strings.Repeat("a", 64),
		},
		Modules: []Module{{
			Name:     "drbd",
			Path:     "usr/lib/modules/6.12.1-katl/extra/drbd.ko.zst",
			SHA256:   strings.Repeat("b", 64),
			Replaces: "drbd",
		}},
	}
	if err := contract.ValidateRuntime(strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}

	// A rebuild with the same kernel release is still a different target.
	if err := contract.ValidateRuntime(strings.Repeat("c", 64)); err == nil {
		t.Fatal("accepted another runtime build")
	}
	if err := contract.ValidateRuntime(""); err == nil {
		t.Fatal("accepted an unknown runtime build")
	}
}

func TestModuleInventory(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Contract)
	}{
		{"missing inventory", func(c *Contract) { c.Modules = nil }},
		{"other kernel", func(c *Contract) { c.Modules[0].Path = "usr/lib/modules/other/extra/drbd.ko" }},
		{"traversal", func(c *Contract) { c.Modules[0].Path = "usr/lib/modules/6.12/../extra/drbd.ko" }},
		{"global index", func(c *Contract) { c.Modules[0].Path = "usr/lib/modules/6.12/modules.dep" }},
		{"duplicate provider", func(c *Contract) { c.Modules = append(c.Modules, c.Modules[0]) }},
		{"other replacement", func(c *Contract) { c.Modules[0].Replaces = "nvme" }},
		{"invalid digest", func(c *Contract) { c.Modules[0].SHA256 = "unknown" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract := Contract{
				Target: Target{
					Release:       "6.12",
					RuntimeSHA256: strings.Repeat("a", 64),
				},
				Modules: []Module{{
					Name:   "drbd",
					Path:   "usr/lib/modules/6.12/extra/drbd.ko",
					SHA256: strings.Repeat("b", 64),
				}},
			}
			test.mutate(&contract)
			if err := contract.Validate(); err == nil {
				t.Fatal("accepted invalid module contract")
			}
		})
	}
}
