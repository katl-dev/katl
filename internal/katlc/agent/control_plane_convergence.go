package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
	"gopkg.in/yaml.v3"
)

type controlPlaneMirror struct {
	hash            string
	containersReady bool
}

func (e *Executor) runControlPlaneConfigHealth(ctx context.Context, node string) ToolResult {
	healthCtx, cancel := context.WithTimeout(ctx, postKubeadmHealthTimeout)
	defer cancel()
	// Static pods can serve a healthy control plane while the subsequent
	// kubelet component operation repairs an inactive kubelet.
	endpoint, err := localKubeAPIServerEndpoint(e.Root)
	if err != nil {
		return ToolResult{Err: fmt.Errorf("identify local API endpoint: %w", err), ExitStatus: 1}
	}
	probe := func() ToolResult {
		run := boundedHealthRunner(e.toolRunner())
		result := run(healthCtx, []string{"/usr/bin/kubectl", "--kubeconfig", rootedRuntimePath(e.Root, "/etc/kubernetes/admin.conf"), "--server", endpoint.URL(), "--request-timeout", kubernetesStatusProbeTimeout.String(), "get", "--raw=/readyz"}, nil)
		if result.Err != nil || result.ExitStatus != 0 {
			result.Err = fmt.Errorf("local Kubernetes API is not ready: %s", toolFailure(result))
			return result
		}
		etcdContainer := ""
		for _, component := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "etcd"} {
			result = run(healthCtx, []string{"crictl", "ps", "--state", "Running", "--name", "^" + component + "$", "--label", "io.kubernetes.pod.namespace=kube-system", "--label", "io.kubernetes.pod.name=" + component + "-" + node, "-q"}, nil)
			if result.Err != nil || result.ExitStatus != 0 {
				result.Err = fmt.Errorf("observe running %s container: %s", component, toolFailure(result))
				return result
			}
			containers := strings.Fields(string(result.Stdout))
			if len(containers) != 1 {
				return ToolResult{Err: fmt.Errorf("expected one running %s container, observed %d", component, len(containers)), ExitStatus: 1}
			}
			if component == "etcd" {
				etcdContainer = containers[0]
			}
		}
		// CRI exec reaches etcd without kubelet's exec endpoint. Cached mirror
		// readiness can be changed by NodeLifecycle while kubelet is stopped.
		result, err = runEtcdctl(healthCtx, run, etcdContainer, "https://127.0.0.1:2379", "endpoint", "health", "--cluster")
		if err != nil {
			result.Err = fmt.Errorf("etcd endpoint health failed: %w", err)
			result.ExitStatus = 1
		}
		return result
	}
	for {
		result := probe()
		if result.Err == nil && result.ExitStatus == 0 {
			return result
		}
		select {
		case <-healthCtx.Done():
			result.Err = errors.Join(result.Err, healthCtx.Err())
			result.ExitStatus = 1
			return result
		case <-time.After(time.Second):
		}
	}
}

func (e *Executor) controlPlaneMirrors(ctx context.Context, node string, components ...string) (map[string]controlPlaneMirror, error) {
	argv := []string{"/usr/bin/kubectl", "--kubeconfig", rootedRuntimePath(e.Root, "/etc/kubernetes/admin.conf"), "-n", "kube-system", "get", "pods"}
	if len(components) == 0 {
		components = []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler"}
	}
	expected := make(map[string]string, len(components))
	for _, component := range components {
		manifest := component + ".yaml"
		name := component + "-" + node
		argv = append(argv, name)
		expected[name] = manifest
	}
	argv = append(argv, "-o", "json")
	result := boundedHealthRunner(e.toolRunner())(ctx, argv, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return nil, fmt.Errorf("read control-plane mirror pods: %s", toolFailure(result))
	}
	var pods struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Annotations       map[string]string `json:"annotations"`
				DeletionTimestamp *string           `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type, Status string
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(result.Stdout, &pods); err != nil {
		return nil, fmt.Errorf("decode control-plane mirror pods: %w", err)
	}
	mirrors := make(map[string]controlPlaneMirror, len(expected))
	for _, pod := range pods.Items {
		manifest, ok := expected[pod.Metadata.Name]
		if !ok || pod.Spec.NodeName != node {
			return nil, fmt.Errorf("unexpected control-plane mirror pod %q on node %q", pod.Metadata.Name, pod.Spec.NodeName)
		}
		if _, exists := mirrors[manifest]; exists {
			return nil, fmt.Errorf("repeated control-plane mirror pod %q", pod.Metadata.Name)
		}
		hash := pod.Metadata.Annotations["kubernetes.io/config.hash"]
		if hash == "" || pod.Metadata.Annotations["kubernetes.io/config.mirror"] != hash {
			return nil, fmt.Errorf("control-plane pod %q has no matching native mirror hash", pod.Metadata.Name)
		}
		ready, readyConditions := false, 0
		// NodeLifecycle can set PodReady=false solely because CNI is absent.
		// ContainersReady retains kubelet's native container probe result.
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "ContainersReady" {
				readyConditions++
				if readyConditions > 1 {
					return nil, fmt.Errorf("control-plane pod %q has repeated ContainersReady conditions", pod.Metadata.Name)
				}
				if condition.Status != "True" && condition.Status != "False" && condition.Status != "Unknown" {
					return nil, fmt.Errorf("control-plane pod %q has invalid ContainersReady status %q", pod.Metadata.Name, condition.Status)
				}
				ready = condition.Status == "True"
			}
		}
		mirrors[manifest] = controlPlaneMirror{hash: hash, containersReady: ready && readyConditions == 1 && pod.Metadata.DeletionTimestamp == nil}
	}
	if len(mirrors) != len(expected) {
		return nil, fmt.Errorf("expected %d control-plane mirror pods, observed %d", len(expected), len(mirrors))
	}
	return mirrors, nil
}

func (e *Executor) waitControlPlaneManifests(ctx context.Context, record operation.OperationRecord, before map[string]controlPlaneMirror) error {
	changed := make(map[string]string)
	for _, name := range controlPlaneManifestNames {
		var previous, desired any
		for _, manifest := range []struct {
			path  string
			value *any
		}{
			{filepath.Join(e.Store.Root, record.OperationID, "manifest-backup", name), &previous},
			{rootedRuntimePath(e.Root, "/etc/kubernetes/manifests/"+name), &desired},
		} {
			data, err := os.ReadFile(manifest.path)
			if err != nil {
				return err
			}
			if err := yaml.Unmarshal(data, manifest.value); err != nil {
				return fmt.Errorf("decode native manifest %s: %w", name, err)
			}
		}
		// Formatting alone does not change kubelet's native configuration hash.
		if !reflect.DeepEqual(previous, desired) {
			changed[name] = before[name].hash
		}
	}
	if len(changed) == 0 {
		return nil
	}
	healthCtx, cancel := context.WithTimeout(ctx, postKubeadmHealthTimeout)
	defer cancel()
	for {
		mirrors, err := e.controlPlaneMirrors(healthCtx, record.KubeadmControlPlaneConfig.NodeName)
		if err == nil {
			for _, name := range controlPlaneManifestNames {
				oldHash, requiresChange := changed[name]
				if requiresChange && (mirrors[name].hash == oldHash || !mirrors[name].containersReady) {
					err = fmt.Errorf("control-plane pod for %s containers have not become ready with the changed native manifest", name)
					break
				}
			}
		}
		if err == nil {
			return nil
		}
		select {
		case <-healthCtx.Done():
			return errors.Join(err, healthCtx.Err())
		case <-time.After(time.Second):
		}
	}
}
