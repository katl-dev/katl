package manifest

import (
	"slices"

	"github.com/katl-dev/katl/internal/systemdunit"
)

func (node NodeConfig) UnitActivation() systemdunit.Activation {
	activation := systemdunit.Activation{
		Enabled: slices.Clone(node.HostConfiguration.EnabledUnits),
	}
	for _, extension := range node.SystemExtensions {
		if extension.State == SystemExtensionAbsent {
			continue
		}
		for _, unit := range extension.Units {
			if slices.Contains(node.HostConfiguration.MaskedUnits, unit.Name) {
				continue
			}
			if unit.Enable {
				activation.Enabled = append(activation.Enabled, unit.Name)
			}
			if unit.RequiredForBootHealth {
				activation.Required = append(activation.Required, unit.Name)
			}
		}
	}
	slices.Sort(activation.Enabled)
	activation.Enabled = slices.Compact(activation.Enabled)
	slices.Sort(activation.Required)
	activation.Required = slices.Compact(activation.Required)
	return activation
}
