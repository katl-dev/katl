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

	"github.com/katl-dev/katl/internal/deadline"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/kubeletstartup"
	"github.com/katl-dev/katl/internal/retry"
)

type kubeletHealthState string

const (
	kubeletHealthReady            kubeletHealthState = "ready"
	kubeletHealthCNIUninitialized kubeletHealthState = "cni-uninitialized"
	kubeletHealthUnhealthy        kubeletHealthState = "unhealthy"
	missingCNIMessage                                = "Network plugin returns error: cni plugin not initialized"
	missingCNINodeMessage                            = "container runtime network not ready: NetworkReady=false reason:NetworkPluginNotReady message:" + missingCNIMessage
)

// settledReportAge is how long a NotReady report other than sole-CNI must
// stand before it counts as kubelet's settled state rather than a startup
// transient. Kubelet posts its Node status as soon as a condition changes, so
// a report that has stood for three 10s status update periods has been
// re-evaluated by kubelet without changing.
//
// A report has stood that long when either of two observations shows it. Its
// heartbeat is at least this old, which classifies a long-settled report at
// once; this assumes kubelet reposts an unchanged status no more often than
// every settledReportAge, as with a nodeStatusReportFrequency of at least 30s.
// The heartbeat rather than kubelet's start is used because kubelet polls the
// API for CSINode initialization indefinitely, so after a power cut the API
// can return long after kubelet started. Otherwise the baseline wait has
// itself observed the same Ready status, reason and message for this long on
// the agent clock. That second arm covers a nodeStatusReportFrequency under
// 30s, including the one kubelet derives from an explicit
// nodeStatusUpdateFrequency, where every read carries a fresh heartbeat. The
// heartbeat and the agent clock are both the node's own clock.
const settledReportAge = 30 * time.Second

// pendingObservation marks an observation that a kubelet starting in this
// boot or invocation is expected to resolve without intervention: the service
// is still starting, the Kubernetes API is not answering yet, the Node report
// predates the current invocation, or kubelet's NotReady report is too recent
// to be its settled state.
type pendingObservation struct{ error }

func (p pendingObservation) Unwrap() error { return p.error }

// readyCondition is the content of a Node Ready condition, without its heartbeat.
type readyCondition struct{ status, reason, message string }

// reportSettling is one baseline wait's record of the Ready condition it has
// observed unchanged, and since when on the agent clock.
type reportSettling struct {
	condition readyCondition
	since     time.Time
}

// settled reports whether a NotReady report counts as kubelet's settled state;
// see settledReportAge. A nil record has no observation history, so only the
// heartbeat arm applies.
func (s *reportSettling) settled(condition readyCondition, heartbeat, now time.Time) bool {
	if now.Sub(heartbeat) >= settledReportAge {
		return true
	}
	if s == nil {
		return false
	}
	if s.since.IsZero() || s.condition != condition {
		s.condition, s.since = condition, now
	}
	return now.Sub(s.since) >= settledReportAge
}

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
	var observed kubeletHealthObservation
	if repair {
		// Repair tolerates an unobservable baseline and restarts kubelet anyway.
		observed, err = e.observeKubeletHealth(ctx, stored.KubeadmControlPlaneConfig, nil)
	} else {
		observed, err = e.awaitKubeletObservation(ctx, stored.KubeadmControlPlaneConfig)
	}
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

// awaitKubeletObservation establishes the pre-mutation baseline from the
// current kubelet invocation. A request admitted right after boot or a kubelet
// restart would otherwise observe the previous invocation's Node report, or a
// report the starting kubelet has not finished, and record a requirement that
// the settled node cannot meet.
func (e *Executor) awaitKubeletObservation(ctx context.Context, request *operation.KubeadmControlPlaneConfig) (kubeletHealthObservation, error) {
	waitCtx, cancel := context.WithTimeout(ctx, postKubeadmHealthTimeout)
	defer cancel()
	// The caller also reads an observation that ended in error: a valid Ready
	// report tightens the requirement even when a later probe fails.
	var observed kubeletHealthObservation
	var pending error
	var settling reportSettling
	backoff := retry.NewBackoff(time.Second, time.Second)
	_, err := retry.Await(waitCtx, &backoff, func(ctx context.Context) (struct{}, bool, error) {
		current, err := e.observeKubeletHealth(ctx, request, &settling)
		if retry.ContextError(ctx) != nil {
			// An observation cut short by the deadline carries no new evidence; keep the last complete one.
			return struct{}{}, false, err
		}
		observed = current
		if isPendingObservation(err) {
			pending = err
		}
		return struct{}{}, true, err
	}, isPendingObservation)
	switch {
	case err == nil:
		return observed, nil
	case errors.Is(err, context.Canceled):
		return observed, errors.Join(pending, err)
	case errors.Is(err, context.DeadlineExceeded) && pending != nil:
		return observed, fmt.Errorf("kubelet did not report current health for node %s in time: %w; inspect kubelet with katlctl node logs %s --unit kubelet", request.NodeName, pending, request.NodeName)
	}
	return observed, err
}

func isPendingObservation(err error) bool { return errors.As(err, new(pendingObservation)) }

// armDeadline starts a systemd deadline timer. Unit tests replace it so they
// do not start timers on the test host.
var armDeadline = deadline.Arm

// armKubeletHealth bounds post-apply kubelet health with the operation's
// kubelet-config-health timer, which owns the deadline's duration.
func armKubeletHealth(ctx context.Context, record operation.OperationRecord) (context.Context, context.CancelFunc, error) {
	return armDeadline(ctx, deadline.KubeletConfigHealth,
		fmt.Sprintf("operation=%s rollout=%s", record.OperationID, record.KubeadmControlPlaneConfig.RolloutID))
}

func (e *Executor) runKubeletConfigHealth(ctx context.Context, record operation.OperationRecord) ToolResult {
	stored, err := e.Store.Read(record.OperationID)
	if err != nil {
		return ToolResult{Err: fmt.Errorf("read kubelet readiness requirement: %w", err), ExitStatus: 1}
	}
	record.KubeadmControlPlaneConfig.ReadinessRequirement = stored.KubeadmControlPlaneConfig.ReadinessRequirement
	healthCtx, cancel, err := armKubeletHealth(ctx, stored)
	if err != nil {
		return ToolResult{Err: err, ExitStatus: 1}
	}
	defer cancel()
	var lastErr error
	for {
		observed, err := e.observeKubeletHealth(healthCtx, record.KubeadmControlPlaneConfig, nil)
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
		if healthCtx.Err() != nil && lastErr != nil {
			// An attempt cut short by the deadline carries no new evidence; report the last complete one.
			err = lastErr
		}
		lastErr = err
		select {
		case <-healthCtx.Done():
			return ToolResult{Err: errors.Join(err, context.Cause(healthCtx)), ExitStatus: 1}
		case <-time.After(time.Second):
		}
	}
}

// observeKubeletHealth classifies the current kubelet invocation's Node
// report. The baseline wait passes its settling record; other callers pass nil.
func (e *Executor) observeKubeletHealth(ctx context.Context, request *operation.KubeadmControlPlaneConfig, settling *reportSettling) (kubeletHealthObservation, error) {
	observed := kubeletHealthObservation{state: kubeletHealthUnhealthy}
	invocation, starting, err := e.kubeletService(ctx)
	if err != nil {
		return observed, err
	}
	if starting {
		observed.failureReason = "kubelet is starting"
		return observed, pendingObservation{errors.New(observed.failureReason)}
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
		return observed, pendingObservation{fmt.Errorf("read Kubernetes node %s: %s", request.NodeName, toolFailure(result))}
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
	cniUninitialized, notReady := false, false
	var heartbeat time.Time
	var ready readyCondition
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
		ready = readyCondition{condition.Status, condition.Reason, condition.Message}
		notReady = condition.Status == "False" && condition.Reason == "KubeletNotReady"
		cniUninitialized = notReady && condition.Message == missingCNINodeMessage
		observed.failureReason = fmt.Sprintf("Kubernetes node %s Ready=%s: %s: %s", request.NodeName, condition.Status, condition.Reason, condition.Message)
	}
	if readyConditions != 1 {
		observed.nodeReady = false
		return observed, fmt.Errorf("expected one Kubernetes Ready condition, observed %d", readyConditions)
	}
	now := e.clock()
	if err := startup.Verify(e.Root, invocation, node.Status.NodeInfo.BootID, heartbeat, now); errors.Is(err, kubeletstartup.ErrReportPending) {
		return observed, pendingObservation{err}
	} else if err != nil {
		return observed, err
	}
	// Sole-CNI is classifiable at once; any other fresh NotReady may still be a
	// startup transient that kubelet replaces within the window.
	if notReady && !cniUninitialized && !settling.settled(ready, heartbeat, now) {
		return observed, pendingObservation{errors.New(observed.failureReason)}
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
	invocation, _, err := e.kubeletService(ctx)
	return invocation, err
}

// kubeletService returns the active kubelet invocation, or whether systemd is
// still starting kubelet. A pending automatic restart is not a start: it is
// how a failing kubelet waits between attempts.
func (e *Executor) kubeletService(ctx context.Context) (invocation string, starting bool, err error) {
	result := boundedHealthRunner(e.toolRunner())(ctx, []string{"/usr/bin/systemctl", "show", "kubelet.service", "--property=ActiveState", "--property=SubState", "--property=Job", "--property=MainPID", "--property=InvocationID"}, nil)
	if err := kubeletProbeExecutionError(ctx, result); err != nil {
		return "", false, fmt.Errorf("observe kubelet service: %w", err)
	}
	if result.Err != nil || result.ExitStatus != 0 {
		return "", false, fmt.Errorf("read kubelet service properties: %s", toolFailure(result))
	}
	properties := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(result.Stdout)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return "", false, fmt.Errorf("malformed kubelet service property %q", line)
		}
		if _, exists := properties[key]; exists {
			return "", false, fmt.Errorf("repeated kubelet service property %q", key)
		}
		properties[key] = value
	}
	pid, err := strconv.ParseUint(properties["MainPID"], 10, 64)
	if err != nil || properties["ActiveState"] == "" {
		return "", false, fmt.Errorf("incomplete kubelet service properties")
	}
	if properties["ActiveState"] != "active" || pid == 0 {
		starting = properties["ActiveState"] == "activating" && !strings.HasPrefix(properties["SubState"], "auto-restart") || properties["ActiveState"] == "inactive" && properties["Job"] != ""
		return "", starting, nil
	}
	invocation = properties["InvocationID"]
	if len(invocation) != 32 || strings.Trim(invocation, "0123456789abcdef") != "" {
		return "", false, fmt.Errorf("active kubelet has no valid service invocation identity")
	}
	return invocation, false, nil
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
