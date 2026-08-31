package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/katl-dev/katl/internal/installer/apivip"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func controlPlaneEndpointStatus(root string) (*agentapi.ControlPlaneEndpointStatus, error) {
	configFile, err := os.Open(rootedRuntimePath(root, apivip.ConfigPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open managed endpoint configuration: %w", err)
	}
	object, decodeErr := apivip.Decode(configFile)
	closeErr := configFile.Close()
	if decodeErr != nil || closeErr != nil {
		return nil, errors.Join(decodeErr, closeErr)
	}
	config, err := apivip.Normalize(object.Spec)
	if err != nil {
		return nil, err
	}

	report := endpointStatusFrom(config, nil)
	statusFile, err := os.Open(rootedRuntimePath(root, apivip.LiveStatusPath))
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		report.State = "failed"
		report.FailureReason = "endpoint status unavailable"
		return report, nil
	}
	live, decodeErr := apivip.DecodeStatus(statusFile)
	closeErr = statusFile.Close()
	if decodeErr != nil || closeErr != nil {
		report.State = "failed"
		report.FailureReason = "endpoint status unavailable"
		return report, nil
	}
	return endpointStatusFrom(config, &live), nil
}

func endpointStatusFrom(config apivip.Config, live *apivip.Status) *agentapi.ControlPlaneEndpointStatus {
	report := &agentapi.ControlPlaneEndpointStatus{
		Endpoint: net.JoinHostPort(config.Endpoint.Host, fmt.Sprint(config.Endpoint.Port)),
		Vip:      config.Endpoint.VIP,
		State:    "starting",
	}
	if config.Ownership.Enabled == nil || !*config.Ownership.Enabled {
		report.State = "disabled"
	}
	if live == nil {
		return report
	}
	report.LocalApiReady = live.HealthState == apivip.HealthHealthy
	report.LocalVipOwned = live.LocalVIPOwned
	report.LastTransitionTime = firstNonEmpty(live.LastOwnershipTransition, live.LastHealthTransition, live.UpdatedAt)
	report.FailureReason = strings.TrimSpace(live.FailureReason)
	report.State = endpointProductState(config, *live)
	return report
}

func endpointProductState(config apivip.Config, live apivip.Status) string {
	if config.Ownership.Enabled == nil || !*config.Ownership.Enabled {
		return "disabled"
	}
	if live.RecoveryRequired || strings.TrimSpace(live.FailureReason) != "" {
		return "failed"
	}
	if !live.VIPInterfaceReady {
		return "waiting-for-network"
	}
	if strings.Contains(strings.ToLower(live.HealthFailure), "kubeadm api ca") {
		return "waiting-for-kubeadm-ca"
	}
	if live.HealthState != apivip.HealthHealthy {
		return "waiting-for-apiserver"
	}
	if live.OwnershipState == apivip.OwnershipOwned && live.LocalVIPOwnedReported && !live.LocalVIPOwned {
		return "failed"
	}
	if live.LocalVIPOwned {
		return "active"
	}
	return "released"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
