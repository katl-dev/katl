package operatorconsole

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var controlPlanePodNames = [controlPlanePodCount]string{
	"kube-apiserver",
	"kube-controller-manager",
	"kube-scheduler",
	"etcd",
}

func initialControlPlanePods(state string) ControlPlanePodStatuses {
	var pods ControlPlanePodStatuses
	for index, name := range controlPlanePodNames {
		pods[index] = KubernetesPodStatus{Name: name, State: state}
	}
	return pods
}

func probeControlPlanePods(ctx context.Context) (ControlPlanePodStatuses, error) {
	command := exec.CommandContext(
		ctx,
		"/usr/bin/crictl",
		"ps",
		"--all",
		"--namespace", "^kube-system$",
		"--output", "json",
	)
	output, err := command.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return initialControlPlanePods(KubernetesPodUnknown), fmt.Errorf("query control-plane containers: %w", ctxErr)
		}
		detail := ""
		if exitError, ok := err.(*exec.ExitError); ok {
			detail = strings.Join(strings.Fields(string(exitError.Stderr)), " ")
		}
		if detail != "" {
			return initialControlPlanePods(KubernetesPodUnknown), fmt.Errorf("query control-plane containers: %w: %s", err, detail)
		}
		return initialControlPlanePods(KubernetesPodUnknown), fmt.Errorf("query control-plane containers: %w", err)
	}
	return decodeControlPlanePods(output)
}

func decodeControlPlanePods(data []byte) (ControlPlanePodStatuses, error) {
	var response runtimeapi.ListContainersResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, &response); err != nil {
		return initialControlPlanePods(KubernetesPodUnknown), fmt.Errorf("decode control-plane containers: %w", err)
	}
	pods := initialControlPlanePods(KubernetesPodNotStarted)
	var newest [controlPlanePodCount]int64
	for _, candidate := range response.GetContainers() {
		index := controlPlanePodIndex(candidate.GetMetadata().GetName())
		if index < 0 {
			continue
		}
		state := criContainerState(candidate.GetState())
		createdAt := candidate.GetCreatedAt()
		if state == KubernetesPodRunning || (pods[index].State != KubernetesPodRunning && createdAt >= newest[index]) {
			pods[index].State = state
			newest[index] = createdAt
		}
	}
	return pods, nil
}

func controlPlanePodIndex(name string) int {
	name = strings.TrimSpace(name)
	for index, candidate := range controlPlanePodNames {
		if name == candidate {
			return index
		}
	}
	return -1
}

func criContainerState(state runtimeapi.ContainerState) string {
	switch state {
	case runtimeapi.ContainerState_CONTAINER_RUNNING:
		return KubernetesPodRunning
	case runtimeapi.ContainerState_CONTAINER_CREATED:
		return KubernetesPodStarting
	case runtimeapi.ContainerState_CONTAINER_EXITED:
		return KubernetesPodNotRunning
	default:
		return KubernetesPodUnknown
	}
}
