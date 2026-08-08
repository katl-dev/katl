package agent

import (
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/installer/generation"
)

const (
	nodeBootHealthHealthy = "healthy"
	nodeBootHealthPending = "pending"
	nodeBootHealthFailed  = "failed"
	nodeBootHealthUnknown = "unknown"
)

type nodeBootHealth struct {
	SelectedGenerationID string
	State                string
	Diagnostic           string
}

func readNodeBootHealth(root string) nodeBootHealth {
	commandLine, err := readCurrentKernelCommandLine(root)
	if err != nil {
		return nodeBootHealth{}
	}
	selected, err := generation.SelectedGenerationFromCommandLine(strings.Join(commandLine, " "))
	if err != nil {
		return nodeBootHealth{State: nodeBootHealthUnknown, Diagnostic: err.Error()}
	}
	result := nodeBootHealth{SelectedGenerationID: selected}
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		result.State = nodeBootHealthUnknown
		result.Diagnostic = "durable boot selection is unavailable"
		return result
	}

	booted := strings.TrimSpace(selection.BootedGenerationID)
	if selection.PendingHealthValidation {
		target := strings.TrimSpace(selection.TargetBootGenerationID)
		trial := strings.TrimSpace(selection.TrialGenerationID)
		if selected == target || selected == trial {
			result.State = nodeBootHealthPending
			result.Diagnostic = fmt.Sprintf("generation %s is awaiting boot-health validation", selected)
			return result
		}
	}
	if booted != "" && booted != selected {
		result.State = nodeBootHealthFailed
		result.Diagnostic = fmt.Sprintf("running generation %s does not match durable boot evidence %s; boot recovery must reconcile the selected generation", selected, booted)
		return result
	}

	_, generationStatus, err := generation.ReadGeneration(root, selected)
	if err != nil {
		result.State = nodeBootHealthUnknown
		result.Diagnostic = fmt.Sprintf("selected generation %s has no readable status", selected)
		return result
	}
	switch {
	case generationStatus.CommitState == generation.CommitStateCommitted &&
		generationStatus.BootState == generation.BootStateGood &&
		generationStatus.HealthState == generation.HealthStateHealthy:
		result.State = nodeBootHealthHealthy
	case generationStatus.BootState == generation.BootStateFailed || generationStatus.HealthState == generation.HealthStateUnhealthy:
		result.State = nodeBootHealthFailed
		result.Diagnostic = fmt.Sprintf("selected generation %s is recorded as boot=%s health=%s", selected, generationStatus.BootState, generationStatus.HealthState)
	default:
		result.State = nodeBootHealthPending
		result.Diagnostic = fmt.Sprintf("selected generation %s is awaiting boot-health validation", selected)
	}
	return result
}
