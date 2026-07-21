package operatorconsole

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
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
	output, err := exec.CommandContext(
		ctx,
		"/usr/bin/crictl",
		"ps",
		"--all",
		"--namespace", "^kube-system$",
		"--output", "json",
	).Output()
	if err != nil {
		return initialControlPlanePods(KubernetesPodUnknown), fmt.Errorf("query control-plane containers: %w", err)
	}
	return decodeControlPlanePods(output)
}

func decodeControlPlanePods(data []byte) (ControlPlanePodStatuses, error) {
	type container struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		State     string `json:"state"`
		CreatedAt int64  `json:"createdAt"`
	}
	var response struct {
		Containers []container `json:"containers"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return initialControlPlanePods(KubernetesPodUnknown), fmt.Errorf("decode control-plane containers: %w", err)
	}
	pods := initialControlPlanePods(KubernetesPodNotStarted)
	var newest [controlPlanePodCount]int64
	for _, candidate := range response.Containers {
		index := controlPlanePodIndex(candidate.Metadata.Name)
		if index < 0 {
			continue
		}
		state := criContainerState(candidate.State)
		if state == KubernetesPodRunning || (pods[index].State != KubernetesPodRunning && candidate.CreatedAt >= newest[index]) {
			pods[index].State = state
			newest[index] = candidate.CreatedAt
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

func criContainerState(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "CONTAINER_RUNNING", "RUNNING":
		return KubernetesPodRunning
	case "CONTAINER_CREATED", "CREATED":
		return KubernetesPodStarting
	case "CONTAINER_EXITED", "EXITED":
		return KubernetesPodNotRunning
	default:
		return KubernetesPodUnknown
	}
}
