package operatorconsole

import (
	"strings"

	"github.com/katl-dev/katl/internal/installer/generation"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

// NewDashboardModel translates collected node facts into explicit operator
// presentation states before layout begins.
func NewDashboardModel(snapshot *Snapshot) DashboardModel {
	return DashboardModel{
		Host:       presentHost(snapshot),
		Kubernetes: presentKubernetes(snapshot),
	}
}

func presentInstaller(snapshot *Snapshot) Presentation {
	if snapshot.StatusStale {
		return Presentation{State: PresentationUnknown, Label: "Unknown (stale status)"}
	}
	if snapshot.StatusError != "" {
		return Presentation{State: PresentationUnknown, Label: "Unknown"}
	}
	switch snapshot.State {
	case installstatus.StateFailedBeforeMutation, installstatus.StateFailedAfterMutation, installstatus.StateInstallRefused:
		return Presentation{State: PresentationFailed, Label: stateLabel(snapshot.State)}
	case installstatus.StateRebootRequested:
		return Presentation{State: PresentationHealthy, Label: stateLabel(snapshot.State)}
	case "starting-installer", installstatus.StateRunning, installstatus.StateWaitingForConfig, installstatus.StateDebugHold:
		return Presentation{State: PresentationProgressing, Label: stateLabel(snapshot.State)}
	default:
		return Presentation{State: PresentationUnknown, Label: stateLabel(snapshot.State)}
	}
}

func presentHost(snapshot *Snapshot) Presentation {
	if snapshot.StatusStale {
		return Presentation{State: PresentationUnknown, Label: "Unknown (stale status)"}
	}
	if snapshot.StatusError != "" || snapshot.GenerationError != "" {
		return Presentation{State: PresentationUnknown, Label: "Unknown"}
	}
	if snapshot.State == installstatus.StateRuntimeFailedNeedsRepair {
		return Presentation{State: PresentationFailed, Label: "Needs repair"}
	}
	if snapshot.LastError != "" {
		return Presentation{State: PresentationDegraded, Label: "Degraded"}
	}
	switch strings.ToLower(strings.TrimSpace(snapshot.GenerationHealth)) {
	case generation.HealthStateUnhealthy:
		return Presentation{State: PresentationFailed, Label: "Generation failed"}
	case generation.HealthStateDeferred:
		return Presentation{State: PresentationProgressing, Label: "Health deferred"}
	case generation.HealthStateUnknown:
		return Presentation{State: PresentationUnknown, Label: "Health unknown"}
	case generation.HealthStateHealthy:
		switch snapshot.State {
		case installstatus.StateKubeadmReady, installstatus.StateWaitingForClusterBootstrap:
			return Presentation{State: PresentationHealthy, Label: "Healthy"}
		case "starting-runtime", installstatus.StateRuntimeBootedNotReady:
			return Presentation{State: PresentationProgressing, Label: stateLabel(snapshot.State)}
		default:
			return Presentation{State: PresentationUnknown, Label: "Unknown (" + fallback(snapshot.State, "state") + ")"}
		}
	case "":
		switch snapshot.State {
		case "starting-runtime", installstatus.StateRuntimeBootedNotReady:
			return Presentation{State: PresentationProgressing, Label: stateLabel(snapshot.State)}
		default:
			return Presentation{State: PresentationUnknown, Label: "Unknown"}
		}
	default:
		return Presentation{State: PresentationUnknown, Label: "Unknown health"}
	}
}

func presentKubernetes(snapshot *Snapshot) Presentation {
	if snapshot.StatusStale || snapshot.StatusError != "" || snapshot.GenerationError != "" {
		return Presentation{State: PresentationUnknown, Label: "Unknown"}
	}
	if snapshot.State == installstatus.StateRuntimeFailedNeedsRepair {
		return Presentation{State: PresentationFailed, Label: "Unavailable"}
	}
	if snapshot.LiveSoftware.KubernetesVersion == "" {
		return Presentation{State: PresentationUnknown, Label: "Not installed"}
	}
	if snapshot.KubernetesConfigured {
		if snapshot.ControlPlane {
			return presentControlPlane(snapshot.ControlPlanePods)
		}
		return Presentation{State: PresentationProgressing, Label: "Configured"}
	}
	switch snapshot.State {
	case installstatus.StateKubeadmReady, installstatus.StateWaitingForClusterBootstrap:
		return Presentation{State: PresentationProgressing, Label: "Ready for bootstrap"}
	case "starting-runtime", installstatus.StateRuntimeBootedNotReady:
		return Presentation{State: PresentationProgressing, Label: "Waiting for KatlOS"}
	default:
		return Presentation{State: PresentationUnknown, Label: "Unknown"}
	}
}

func presentControlPlane(pods ControlPlanePodStatuses) Presentation {
	running := 0
	starting := false
	failed := false
	for _, pod := range pods {
		switch pod.State {
		case KubernetesPodRunning:
			running++
		case KubernetesPodNotRunning:
			failed = true
		case KubernetesPodStarting, KubernetesPodNotStarted:
			starting = true
		}
	}
	if running == len(pods) {
		return Presentation{State: PresentationHealthy, Label: "Control plane healthy"}
	}
	if failed {
		return Presentation{State: PresentationDegraded, Label: "Control plane degraded"}
	}
	if starting || running > 0 {
		return Presentation{State: PresentationProgressing, Label: "Control plane starting"}
	}
	return Presentation{State: PresentationUnknown, Label: "Control plane unknown"}
}

func presentationStyle(state PresentationState) Style {
	switch state {
	case PresentationHealthy:
		return styleGood
	case PresentationProgressing, PresentationDegraded:
		return styleWarn
	case PresentationFailed:
		return styleBad
	default:
		return styleDim
	}
}

func stateLabel(state string) string {
	switch state {
	case "starting-installer":
		return "Starting installer"
	case "starting-runtime":
		return "Starting KatlOS"
	case "running":
		return "Installing"
	case "debug-hold":
		return "Debug hold; installation disabled"
	case "waiting-for-config":
		return "Waiting for configuration"
	case "install-refused":
		return "Installation refused"
	case "failed-before-mutation":
		return "Installation failed; disk unchanged"
	case "failed-after-mutation":
		return "Installation failed; repair required"
	case "reboot-requested":
		return "Installation complete; rebooting"
	case "kubeadm-ready":
		return "Ready for Kubernetes bootstrap"
	case "waiting-for-cluster-bootstrap":
		return "Waiting for Kubernetes bootstrap"
	case "runtime-booted-not-ready":
		return "KatlOS booted; not ready"
	case "runtime-failed-needs-repair":
		return "KatlOS needs repair"
	default:
		return fallback(state, "Unknown")
	}
}

func healthLabel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ""
	case "healthy", "good", "ok", "success":
		return "OK"
	case "unhealthy", "failed", "failure":
		return "FAILED"
	default:
		return strings.ToUpper(strings.TrimSpace(value))
	}
}

func healthStyle(value string) Style {
	if value == "" {
		return ""
	}
	if value == "OK" {
		return styleGood
	}
	if value == "FAILED" {
		return styleBad
	}
	return styleWarn
}
