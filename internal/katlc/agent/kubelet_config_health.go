package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/kubeletstartup"
)

type kubeletHealthState string

const (
	kubeletHealthReady            kubeletHealthState = "ready"
	kubeletHealthCNIUninitialized kubeletHealthState = "cni-uninitialized"
	kubeletHealthUnhealthy        kubeletHealthState = "unhealthy"
	missingCNIMessage                                = "Network plugin returns error: cni plugin not initialized"
	missingCNINodeMessage                            = "container runtime network not ready: NetworkReady=false reason:NetworkPluginNotReady message:" + missingCNIMessage
)

type kubeletHealthObservation struct {
	state         kubeletHealthState
	nodeReady     bool
	failureReason string
	startup       kubeletstartup.Record
	controlPlane  bool
}

func (e *Executor) captureKubeletHealth(ctx context.Context, record operation.OperationRecord) error {
	stored, err := e.Store.Read(record.OperationID)
	if err != nil {
		return err
	}
	var previous *operation.OperationRecord
	if stored.ExternalMutationStarted {
		previous = &stored
	} else {
		previous, err = e.lastKubeadmConfigMutation(stored)
		if err != nil {
			return err
		}
	}
	repair := previous != nil && previous.Result != operation.ResultSucceeded
	required := string(kubeletHealthReady)
	if repair && previous.KubeadmControlPlaneConfig.ReadinessRequirement == string(kubeletHealthCNIUninitialized) {
		required = string(kubeletHealthCNIUninitialized)
	}
	observed, err := e.observeKubeletHealth(ctx, stored.KubeadmControlPlaneConfig)
	if err != nil && !repair && !observed.nodeReady {
		return fmt.Errorf("observe configuration health before mutation: %w", err)
	}
	observationErr := err
	if observed.nodeReady || stored.KubeadmControlPlaneConfig.ReadinessRequirement == string(kubeletHealthReady) {
		required = string(kubeletHealthReady)
	} else if !repair && observed.state == kubeletHealthCNIUninitialized {
		required = string(kubeletHealthCNIUninitialized)
	}
	// The requirement can tighten to Ready, but interrupted retries cannot
	// manufacture a new CNI exception after external mutation has started.
	updated, err := e.Store.Update(record.OperationID, "configuration-health-requirement", "configuration-health-requirement", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		if current.KubeadmControlPlaneConfig.ReadinessRequirement != string(kubeletHealthReady) {
			current.KubeadmControlPlaneConfig.ReadinessRequirement = required
		}
		current.UpdatedAt = e.clock()
		return current, nil
	})
	if err == nil {
		record.KubeadmControlPlaneConfig.ReadinessRequirement = updated.KubeadmControlPlaneConfig.ReadinessRequirement
	}
	if observationErr != nil && !repair {
		return errors.Join(err, fmt.Errorf("observe configuration health before mutation: %w", observationErr))
	}
	return err
}

func (e *Executor) runKubeletConfigHealth(ctx context.Context, record operation.OperationRecord) ToolResult {
	stored, err := e.Store.Read(record.OperationID)
	if err != nil {
		return ToolResult{Err: fmt.Errorf("read kubelet readiness requirement: %w", err), ExitStatus: 1}
	}
	record.KubeadmControlPlaneConfig.ReadinessRequirement = stored.KubeadmControlPlaneConfig.ReadinessRequirement
	healthCtx, cancel := context.WithTimeout(ctx, postKubeadmHealthTimeout)
	defer cancel()
	for {
		observed, err := e.observeKubeletHealth(healthCtx, record.KubeadmControlPlaneConfig)
		if observed.nodeReady {
			if record.KubeadmControlPlaneConfig.ReadinessRequirement != string(kubeletHealthReady) {
				// A later health check or interrupted retry must not reopen the CNI
				// exception after this operation has observed a Ready node.
				_, err := e.Store.Update(record.OperationID, "configuration-health-ready", "configuration-health-ready", func(current operation.OperationRecord) (operation.OperationRecord, error) {
					current.KubeadmControlPlaneConfig.ReadinessRequirement = string(kubeletHealthReady)
					current.UpdatedAt = e.clock()
					return current, nil
				})
				if err != nil {
					return ToolResult{Err: fmt.Errorf("record Ready configuration health: %w", err), ExitStatus: 1}
				}
				record.KubeadmControlPlaneConfig.ReadinessRequirement = string(kubeletHealthReady)
			}
		}
		if err == nil && (observed.state == kubeletHealthReady || observed.state == kubeletHealthCNIUninitialized && record.KubeadmControlPlaneConfig.ReadinessRequirement == string(kubeletHealthCNIUninitialized)) {
			err = e.verifyKubeletCompletion(healthCtx, record.KubeadmControlPlaneConfig, observed)
			if err == nil {
				return ToolResult{}
			}
		}
		if err == nil {
			err = errors.New(observed.failureReason)
		}
		select {
		case <-healthCtx.Done():
			return ToolResult{Err: errors.Join(err, healthCtx.Err()), ExitStatus: 1}
		case <-time.After(time.Second):
		}
	}
}

func (e *Executor) observeKubeletHealth(ctx context.Context, request *operation.KubeadmControlPlaneConfig) (kubeletHealthObservation, error) {
	observed := kubeletHealthObservation{state: kubeletHealthUnhealthy}
	invocation, err := e.kubeletInvocation(ctx)
	if err != nil {
		return observed, err
	}
	if invocation == "" {
		observed.failureReason = "kubelet is not active"
		return observed, nil
	}
	startup, err := kubeletstartup.Read(e.Root)
	if errors.Is(err, os.ErrNotExist) {
		observed.failureReason = "kubelet has no startup evidence; restart is required to verify its runtime"
		return observed, nil
	}
	if err != nil {
		return observed, err
	}
	adminConfig := rootedRuntimePath(e.Root, "/etc/kubernetes/admin.conf")
	controlPlane := false
	if info, err := os.Stat(adminConfig); err == nil {
		controlPlane = !info.IsDir()
	} else if !errors.Is(err, os.ErrNotExist) {
		return observed, fmt.Errorf("inspect control-plane configuration: %w", err)
	}
	kubeconfig := rootedRuntimePath(e.Root, "/etc/kubernetes/kubelet.conf")
	if controlPlane {
		kubeconfig = adminConfig
	}
	run := boundedHealthRunner(e.toolRunner())
	result := run(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", kubeconfig, "get", "node", request.NodeName, "-o", "json"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return observed, fmt.Errorf("read Kubernetes node %s: %s", request.NodeName, toolFailure(result))
	}
	var node struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			NodeInfo struct {
				BootID string `json:"bootID"`
			} `json:"nodeInfo"`
			Conditions []struct {
				Type, Status, Reason, Message string
				LastHeartbeatTime             time.Time `json:"lastHeartbeatTime"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(result.Stdout, &node); err != nil {
		return observed, fmt.Errorf("decode Kubernetes node health: %w", err)
	}
	if node.Metadata.Name != request.NodeName {
		return observed, fmt.Errorf("expected Kubernetes node %q, observed %q", request.NodeName, node.Metadata.Name)
	}
	readyConditions := 0
	cniUninitialized := false
	var heartbeat time.Time
	nodeReady := false
	for _, condition := range node.Status.Conditions {
		if condition.Type != "Ready" {
			continue
		}
		readyConditions++
		if condition.Status != "True" && condition.Status != "False" && condition.Status != "Unknown" {
			return observed, fmt.Errorf("invalid Kubernetes Ready status %q", condition.Status)
		}
		nodeReady = condition.Status == "True"
		heartbeat = condition.LastHeartbeatTime
		cniUninitialized = condition.Status == "False" && condition.Reason == "KubeletNotReady" && condition.Message == missingCNINodeMessage
		observed.failureReason = fmt.Sprintf("Kubernetes node %s Ready=%s: %s: %s", request.NodeName, condition.Status, condition.Reason, condition.Message)
	}
	if readyConditions != 1 {
		observed.nodeReady = false
		return observed, fmt.Errorf("expected one Kubernetes Ready condition, observed %d", readyConditions)
	}
	if err := startup.Verify(e.Root, invocation, node.Status.NodeInfo.BootID, heartbeat, e.clock()); err != nil {
		return observed, err
	}
	observed.startup, observed.controlPlane = startup, controlPlane

	runtime, runtimeErr := e.observeContainerRuntime(ctx)
	// A valid Ready report tightens the requirement even if CRI is unhealthy,
	// but only while the invocation that published it still owns the service.
	current, err := e.kubeletInvocation(ctx)
	if err != nil {
		return observed, errors.Join(runtimeErr, err)
	}
	if current != invocation {
		observed.failureReason = "kubelet service invocation changed during health observation"
		return observed, runtimeErr
	}
	observed.nodeReady = nodeReady
	if runtimeErr != nil {
		return observed, runtimeErr
	}
	if !runtime.ready || !runtime.networkReady && !(cniUninitialized && runtime.missingCNI) {
		observed.failureReason = "container runtime is not ready or its network failure is not solely uninitialized CNI"
		return observed, nil
	}
	if nodeReady {
		observed.state = kubeletHealthReady
	} else if cniUninitialized && runtime.missingCNI {
		observed.state = kubeletHealthCNIUninitialized
	}
	return observed, nil
}

type containerRuntimeHealth struct {
	ready, networkReady, missingCNI bool
}

func (e *Executor) observeContainerRuntime(ctx context.Context) (containerRuntimeHealth, error) {
	var observed containerRuntimeHealth
	result := boundedHealthRunner(e.toolRunner())(ctx, []string{"/usr/bin/crictl", "info", "-o", "json"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return observed, fmt.Errorf("read container runtime health: %s", toolFailure(result))
	}
	var runtime struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  *bool  `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(result.Stdout, &runtime); err != nil {
		return observed, fmt.Errorf("decode container runtime health: %w", err)
	}
	runtimeConditions, networkConditions := 0, 0
	for _, condition := range runtime.Status.Conditions {
		if condition.Type != "RuntimeReady" && condition.Type != "NetworkReady" {
			continue
		}
		if condition.Status == nil {
			return observed, fmt.Errorf("container runtime condition %s has no status", condition.Type)
		}
		if condition.Type == "RuntimeReady" {
			runtimeConditions++
			observed.ready = *condition.Status
		} else {
			networkConditions++
			observed.networkReady = *condition.Status
			observed.missingCNI = !observed.networkReady && condition.Reason == "NetworkPluginNotReady" && condition.Message == missingCNIMessage
		}
	}
	if runtimeConditions != 1 || networkConditions != 1 {
		return observed, fmt.Errorf("expected one RuntimeReady and NetworkReady condition, observed %d and %d", runtimeConditions, networkConditions)
	}
	return observed, nil
}

// Node readiness defines the permitted baseline independently of dependent
// control-plane convergence. Completion requires the selected configuration and
// component health while the invocation whose Node report was observed remains active.
func (e *Executor) verifyKubeletCompletion(ctx context.Context, request *operation.KubeadmControlPlaneConfig, observed kubeletHealthObservation) error {
	if observed.controlPlane {
		endpoint, err := localKubeAPIServerEndpoint(e.Root)
		if err != nil {
			return err
		}
		result := boundedHealthRunner(e.toolRunner())(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", rootedRuntimePath(e.Root, "/etc/kubernetes/admin.conf"), "--server", endpoint.URL(), "--request-timeout", kubernetesStatusProbeTimeout.String(), "get", "--raw=/readyz"}, nil)
		if result.Err != nil || result.ExitStatus != 0 {
			return fmt.Errorf("local Kubernetes API is not ready: %s", toolFailure(result))
		}
		mirrors, err := e.controlPlaneMirrors(ctx, request.NodeName, "etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler")
		if err != nil {
			return err
		}
		for name, mirror := range mirrors {
			if !mirror.containersReady {
				return fmt.Errorf("control-plane containers are not ready: %s", name)
			}
		}
	}
	changed, err := observed.startup.ConfextChanged(e.Root)
	if err != nil {
		return fmt.Errorf("observe active kubelet configuration: %w", err)
	}
	if changed {
		return fmt.Errorf("kubelet has not restarted into the active node configuration")
	}
	current, err := e.kubeletInvocation(ctx)
	if err != nil {
		return err
	}
	if current != observed.startup.InvocationID {
		return fmt.Errorf("kubelet service invocation changed during configuration health checks")
	}
	return nil
}

func (e *Executor) kubeletInvocation(ctx context.Context) (string, error) {
	result := boundedHealthRunner(e.toolRunner())(ctx, []string{"/usr/bin/systemctl", "show", "kubelet.service", "--property=ActiveState", "--property=MainPID", "--property=InvocationID"}, nil)
	if err := kubeletProbeExecutionError(ctx, result); err != nil {
		return "", fmt.Errorf("observe kubelet service: %w", err)
	}
	if result.Err != nil || result.ExitStatus != 0 {
		return "", fmt.Errorf("read kubelet service properties: %s", toolFailure(result))
	}
	properties := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(result.Stdout)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return "", fmt.Errorf("malformed kubelet service property %q", line)
		}
		if _, exists := properties[key]; exists {
			return "", fmt.Errorf("repeated kubelet service property %q", key)
		}
		properties[key] = value
	}
	pid, err := strconv.ParseUint(properties["MainPID"], 10, 64)
	if err != nil || properties["ActiveState"] == "" {
		return "", fmt.Errorf("incomplete kubelet service properties")
	}
	if properties["ActiveState"] != "active" || pid == 0 {
		return "", nil
	}
	invocation := properties["InvocationID"]
	if len(invocation) != 32 || strings.Trim(invocation, "0123456789abcdef") != "" {
		return "", fmt.Errorf("active kubelet has no valid service invocation identity")
	}
	return invocation, nil
}

func (e *Executor) kubeletNeedsRestart(ctx context.Context) (bool, error) {
	invocation, err := e.kubeletInvocation(ctx)
	if err != nil {
		return false, err
	}
	if invocation == "" {
		return true, nil
	}
	startup, err := kubeletstartup.Read(e.Root)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if startup.InvocationID != invocation {
		return true, nil
	}
	return startup.ConfextChanged(e.Root)
}

func kubeletProbeExecutionError(ctx context.Context, result ToolResult) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, result.Err)
	}
	if errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, context.DeadlineExceeded) {
		return result.Err
	}
	if result.ExitStatus < 0 {
		return fmt.Errorf("health probe did not complete: %s", toolFailure(result))
	}
	if result.Err != nil {
		var exitErr *exec.ExitError
		if !errors.As(result.Err, &exitErr) || exitErr.ExitCode() < 0 {
			return result.Err
		}
	}
	return nil
}
