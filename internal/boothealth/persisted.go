package boothealth

import (
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/kernelcmdline"
)

const (
	Healthy   = "healthy"
	Pending   = "pending"
	Failed    = "failed"
	Unhealthy = "unhealthy"
	Unknown   = "unknown"
)

type Status struct {
	SelectedGenerationID string
	State                string
	Diagnostic           string
}

// ReadPersisted describes the selected generation's durable state.
func ReadPersisted(root string) Status {
	commandLine, err := kernelcmdline.ReadCurrent(root)
	if err != nil {
		return Status{State: Unknown, Diagnostic: "cannot read the running kernel command line"}
	}
	selected, err := generation.SelectedGenerationFromCommandLine(strings.Join(commandLine, " "))
	if err != nil {
		return Status{State: Unknown, Diagnostic: err.Error()}
	}
	result := Status{SelectedGenerationID: selected}
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		result.State = Unknown
		result.Diagnostic = "durable boot selection is unavailable"
		return result
	}

	booted := strings.TrimSpace(selection.BootedGenerationID)
	if selection.IsPendingBootTarget(selected) {
		result.State = Pending
		result.Diagnostic = fmt.Sprintf("generation %s is awaiting boot-health validation", selected)
		return result
	}
	if booted != "" && booted != selected {
		result.State = Failed
		result.Diagnostic = fmt.Sprintf("running generation %s does not match durable boot evidence %s; boot recovery must reconcile the selected generation", selected, booted)
		return result
	}

	_, generationStatus, err := generation.ReadGeneration(root, selected)
	if err != nil {
		result.State = Unknown
		result.Diagnostic = fmt.Sprintf("selected generation %s has no readable status", selected)
		return result
	}
	switch {
	case generationStatus.UnavailableReason != "":
		result.State = Failed
		result.Diagnostic = generationStatus.UnavailableReason
	case generation.IsKnownGood(generationStatus):
		result.State = Healthy
	case generationStatus.BootState == generation.BootStateFailed || generationStatus.HealthState == generation.HealthStateUnhealthy:
		result.State = Failed
		result.Diagnostic = fmt.Sprintf("selected generation %s is recorded as boot=%s health=%s", selected, generationStatus.BootState, generationStatus.HealthState)
	default:
		result.State = Pending
		result.Diagnostic = fmt.Sprintf("selected generation %s is awaiting boot-health validation", selected)
	}
	return result
}
