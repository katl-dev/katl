package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/apiproxy"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func nodeAPIProxyStatus(root string) (*agentapi.APIProxyStatus, error) {
	configPath := rootedPath(root, apiproxy.ConfigPath)
	configData, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var config apiproxy.Config
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	config, err = apiproxy.Normalize(config)
	if err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	report := &agentapi.APIProxyStatus{
		State:             "unavailable",
		CanonicalEndpoint: config.CanonicalEndpoint,
		CanonicalState:    "not-checked",
		FailureReason:     "API proxy has not reported status",
	}
	for _, listener := range config.Listeners {
		report.Listeners = append(report.Listeners, &agentapi.APIProxyListenerStatus{
			Address: listener.Address, Exposure: listener.Exposure,
		})
	}

	statusData, err := os.ReadFile(rootedPath(root, apiproxy.StatusPath))
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runtime status: %w", err)
	}
	var status apiproxy.Status
	if err := json.Unmarshal(statusData, &status); err != nil {
		return nil, fmt.Errorf("decode runtime status: %w", err)
	}
	if status.APIVersion != apiproxy.APIVersion || status.Kind != apiproxy.StatusKind {
		return nil, fmt.Errorf("runtime status identity is %s %s", status.APIVersion, status.Kind)
	}
	report.Listeners = nil
	for _, listener := range status.Listeners {
		report.Listeners = append(report.Listeners, &agentapi.APIProxyListenerStatus{
			Address: listener.Address, Exposure: listener.Exposure,
		})
	}
	eligible := 0
	for _, backend := range status.Backends {
		report.Backends = append(report.Backends, &agentapi.APIProxyBackendStatus{
			Name: backend.Name, Address: backend.Address, Local: backend.Local,
			Eligible: backend.Eligible, Reason: backend.Reason,
			LastChecked: formatTime(backend.LastChecked),
		})
		if backend.Eligible {
			eligible++
			if backend.Local {
				report.LocalApiEligible = true
			}
		}
	}
	report.CanonicalEndpoint = status.Canonical.Endpoint
	report.CanonicalState = status.Canonical.State
	report.CanonicalFailureReason = status.Canonical.Reason
	report.UpdatedAt = formatTime(status.UpdatedAt)
	report.FailureReason = ""
	if eligible == 0 {
		report.State = "no-eligible-backends"
		report.FailureReason = "no eligible Kubernetes API backend"
	} else {
		report.State = "ready"
	}
	return report, nil
}

func rootedPath(root, path string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		root = string(filepath.Separator)
	}
	return filepath.Join(filepath.Clean(root), strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)))
}
