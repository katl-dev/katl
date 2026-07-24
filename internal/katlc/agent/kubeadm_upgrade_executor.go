package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	"gopkg.in/yaml.v3"
)

const (
	kubeadmUpgradeTimeout = 30 * time.Minute
	// kubeadm keeps the API server running while its local stacked-etcd member
	// restarts. Draining for the upstream-recommended interval closes existing
	// client connections before they can stall against the unavailable member.
	kubeAPIServerDrainDelay = 20 * time.Second
)

func (e *Executor) executeKubeadmUpgrade(ctx context.Context, record operation.OperationRecord) error {
	var err error
	record, err = e.resolveKubernetesUpgradePayload(ctx, record)
	if err != nil {
		return e.failKubeadmUpgrade(record, "resolve-target", err, false)
	}
	if record.KubernetesSysextUpdate != nil {
		defer func(request operation.KubernetesSysextUpdate) {
			_ = cleanupManagedKubernetesUpgradeArtifact(e.Root, request)
		}(*record.KubernetesSysextUpdate)
	}
	record, err = e.prepareKubeadmUpgradeSnapshot(ctx, record)
	if err != nil {
		return e.failKubeadmUpgrade(record, "snapshot", err, false)
	}
	request := record.KubernetesSysextUpdate
	if request == nil || request.UpgradeRole == "" {
		return fmt.Errorf("executable kubeadm upgrade request is required")
	}
	currentID, err := currentGenerationID(e.Root)
	if err != nil {
		return e.failKubeadmUpgrade(record, "staged", err, false)
	}
	currentSpec, _, err := generation.ReadGeneration(e.Root, currentID)
	if err != nil {
		return e.failKubeadmUpgrade(record, "staged", err, false)
	}
	currentRef, ok := kubernetesRef(currentSpec.Sysexts)
	if !ok {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("current generation has no Kubernetes sysext"), false)
	}
	if currentRef.PayloadVersion != request.SourcePayloadVersion {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("current Kubernetes payload %q does not match requested source %q", currentRef.PayloadVersion, request.SourcePayloadVersion), false)
	}
	if err := verifyFileDigest(rootedRuntimePath(e.Root, request.TargetSysextPath), request.TargetSysextSHA256, request.TargetSysextSize); err != nil {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("verify target Kubernetes sysext: %w", err), false)
	}
	if request.UpgradeRole != "worker" {
		if err := verifyFileDigest(rootedRuntimePath(e.Root, request.SnapshotStorageLocation), request.SnapshotDigest, 0); err != nil {
			return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("verify referenced etcd snapshot: %w", err), false)
		}
	}

	candidateRef, err := e.stageKubernetesCandidate(currentSpec, currentRef, *request, record.OperationID)
	if err != nil {
		return e.failKubeadmUpgrade(record, "staged", err, false)
	}
	toolRoot := rootedRuntimePath(e.Root, filepath.ToSlash(filepath.Join("/var/lib/katl/operations", record.OperationID, "tools/kubernetes")))
	if err := os.MkdirAll(toolRoot, 0o700); err != nil {
		return e.failKubeadmUpgrade(record, "staged", err, false)
	}
	if result := e.toolRunner()(ctx, []string{"systemd-dissect", "--mount", "--read-only", rootedRuntimePath(e.Root, candidateRef.Path), toolRoot}, nil); result.Err != nil || result.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("mount operation-private target sysext: %s", toolFailure(result)), false)
	}
	retainToolView := false
	defer func() {
		if !retainToolView {
			_ = e.toolRunner()(context.Background(), []string{"systemd-dissect", "--umount", toolRoot}, nil)
		}
	}()
	targetKubeadm := filepath.Join(toolRoot, "usr/bin/kubeadm")
	versionResult := e.toolRunner()(ctx, []string{targetKubeadm, "version", "-o", "short"}, nil)
	if versionResult.Err != nil || versionResult.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("inspect target kubeadm: %s", toolFailure(versionResult)), false)
	}
	observedVersion := strings.TrimSpace(string(versionResult.Stdout))
	if observedVersion != request.TargetPayloadVersion {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("target kubeadm reported %q, want %q", observedVersion, request.TargetPayloadVersion), false)
	}
	gatePath := filepath.ToSlash(filepath.Join("/run/katl/operation-gates", record.OperationID, "target-kubelet-released"))
	gateUnit := "kubelet.service.d/20-katl-upgrade-gate.conf"
	if err := e.installKubeletGate(gatePath, gateUnit); err != nil {
		return e.failKubeadmUpgrade(record, "staged", err, false)
	}
	if result := e.toolRunner()(ctx, []string{"systemctl", "daemon-reload"}, nil); result.Err != nil || result.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, "staged", fmt.Errorf("load target kubelet activation gate: %s", toolFailure(result)), false)
	}
	record, err = e.Store.Update(record.OperationID, "kubeadm-upgrade-staged", "staged", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "staged"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "accepted", "staged")
		current.PhaseIndex = len(current.CompletedPhases)
		current.PreviousGenerationID = currentID
		current.CandidateGenerationID = request.CandidateGenerationID
		current.KubeadmUpgradeEvidence.TargetKubeadmArtifactPath = filepath.ToSlash(filepath.Join("/var/lib/katl/operations", record.OperationID, "tools/kubernetes"))
		current.KubeadmUpgradeEvidence.TargetKubeadmArtifactDigest = request.TargetSysextSHA256
		current.KubeadmUpgradeEvidence.TargetKubeadmObservedVersion = observedVersion
		current.KubeadmUpgradeEvidence.KubeletGateTokenPath = gatePath
		current.KubeadmUpgradeEvidence.KubeletGateEnforcementUnit = gateUnit
		current.UpdatedAt = e.clock()
		current.NextAction = "run target kubeadm while source kubelet remains active"
		return current, nil
	})
	if err != nil {
		return err
	}

	if request.UpgradeRole == "apply" {
		if err := e.runKubeadmUpgradeCommand(ctx, record, "kubeadm-plan-running", []string{targetKubeadm, "upgrade", "plan", request.TargetPayloadVersion}, false); err != nil {
			return err
		}
		if _, err := e.Store.Update(record.OperationID, "kubeadm-plan-complete", "kubeadm-plan-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.Phase = "kubeadm-plan-complete"
			current.CompletedPhases = appendMissing(current.CompletedPhases, "kubeadm-plan-running", "kubeadm-plan-complete")
			current.PhaseIndex = len(current.CompletedPhases)
			current.UpdatedAt = e.clock()
			return current, nil
		}); err != nil {
			return err
		}
	}

	endpointPaused := false
	kubeadmPeerConfig := ""
	if request.UpgradeRole != "worker" {
		endpointPaused, err = pauseManagedEndpoint(ctx, e.Root, e.toolRunner())
		if err != nil {
			return e.failKubeadmUpgrade(record, "endpoint-withdraw-running", err, false)
		}
		if endpointPaused {
			defer func() {
				if endpointPaused {
					_ = resumeManagedEndpoint(context.Background(), e.Root, e.toolRunner())
				}
			}()
		}
		kubeadmPeerConfig, err = e.drainKubeAPIServerConnections(ctx, record)
		if err != nil {
			return err
		}
		if kubeadmPeerConfig != "" {
			defer os.Remove(rootedRuntimePath(e.Root, kubeadmPeerConfig))
		}
	}

	retainToolView = true
	if request.UpgradeRole == "apply" {
		argv := []string{targetKubeadm, "upgrade", "apply", "--yes", request.TargetPayloadVersion}
		if kubeadmPeerConfig != "" {
			argv = append(argv, "--kubeconfig", kubeadmPeerConfig)
		}
		if err := e.runKubeadmUpgradeCommand(ctx, record, "kubeadm-apply-running", argv, true); err != nil {
			return err
		}
	} else {
		argv := []string{targetKubeadm, "upgrade", "node"}
		if kubeadmPeerConfig != "" {
			argv = append(argv, "--kubeconfig", kubeadmPeerConfig)
		}
		if err := e.runKubeadmUpgradeCommand(ctx, record, "kubeadm-node-running", argv, true); err != nil {
			return err
		}
	}

	if _, err := e.Store.Update(record.OperationID, "stop-source-kubelet", "kubelet-stop-running", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "kubelet-stop-running"
		current.ActivationState = operation.ActivationStateActivating
		current.KubeadmUpgradeEvidence.KubeletGateState = "released"
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(rootedRuntimePath(e.Root, gatePath)), 0o700); err != nil {
		return e.failKubeadmUpgrade(record, "kubelet-stop-running", err, true)
	}
	if err := os.WriteFile(rootedRuntimePath(e.Root, gatePath), []byte(record.OperationID+"\n"), 0o600); err != nil {
		return e.failKubeadmUpgrade(record, "kubelet-stop-running", err, true)
	}
	if result := e.toolRunner()(ctx, []string{"systemctl", "stop", "kubelet.service"}, nil); result.Err != nil || result.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, "kubelet-stop-running", fmt.Errorf("stop source kubelet: %s", toolFailure(result)), true)
	}
	if _, err := e.Store.Update(record.OperationID, "refresh-kubernetes-sysext", "sysext-refresh-running", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "sysext-refresh-running"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "kubelet-stop-running")
		current.PhaseIndex = len(current.CompletedPhases)
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return e.failKubeadmUpgrade(record, "sysext-refresh-running", err, true)
	}
	if err := e.activateKubernetesCandidate(ctx, currentRef, candidateRef); err != nil {
		return e.failKubeadmUpgrade(record, "sysext-refresh-running", err, true)
	}
	if _, err := e.Store.Update(record.OperationID, "restart-target-kubelet", "kubelet-restart-running", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "kubelet-restart-running"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "sysext-refresh-running")
		current.PhaseIndex = len(current.CompletedPhases)
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return e.failKubeadmUpgrade(record, "kubelet-restart-running", err, true)
	}
	if result := e.toolRunner()(ctx, []string{"systemctl", "restart", "kubelet.service"}, nil); result.Err != nil || result.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, "kubelet-restart-running", fmt.Errorf("restart target kubelet: %s", toolFailure(result)), true)
	}
	if err := e.checkKubeadmUpgradeHealth(ctx, *request); err != nil {
		return e.failKubeadmUpgrade(record, "health-check-running", err, true)
	}
	if endpointPaused {
		if err := resumeManagedEndpoint(ctx, e.Root, e.toolRunner()); err != nil {
			return e.failKubeadmUpgrade(record, "endpoint-resume-running", err, true)
		}
		endpointPaused = false
	}
	if err := e.completeKubeadmUpgrade(ctx, record); err != nil {
		return err
	}
	retainToolView = false
	return nil
}

func cleanupManagedKubernetesUpgradeArtifact(root string, request operation.KubernetesSysextUpdate) error {
	digest := strings.TrimSpace(request.TargetSysextSHA256)
	if validateArtifactSHA256(digest) != nil {
		return nil
	}
	logicalPath := filepath.ToSlash(filepath.Clean(request.TargetSysextPath))
	expected := filepath.ToSlash(filepath.Join("/var/lib/katl/artifacts", kubernetesUpgradeUploadDirectory, digest+".raw"))
	if logicalPath != expected {
		return nil
	}
	if err := os.Remove(rootedRuntimePath(root, logicalPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove uploaded Kubernetes artifact: %w", err)
	}
	return nil
}

type kubeAPIServerEndpoint struct {
	Address netip.Addr
	Port    uint16
}

func (e kubeAPIServerEndpoint) String() string {
	return net.JoinHostPort(e.Address.String(), fmt.Sprintf("%d", e.Port))
}

func (e kubeAPIServerEndpoint) URL() string {
	return "https://" + e.String()
}

func (e *Executor) drainKubeAPIServerConnections(ctx context.Context, record operation.OperationRecord) (string, error) {
	if _, err := e.Store.Update(record.OperationID, "apiserver-drain-start", "apiserver-drain-running", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "apiserver-drain-running"
		current.UpdatedAt = e.clock()
		current.NextAction = "inspect API endpoints before restarting stacked etcd"
		return current, nil
	}); err != nil {
		return "", err
	}
	result := e.toolRunner()(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "default", "get", "endpoints", "kubernetes", "-o", "json"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return "", e.failKubeadmUpgrade(record, "apiserver-drain-running", fmt.Errorf("inspect Kubernetes API endpoints: %s", toolFailure(result)), false)
	}
	endpoints, err := parseKubeAPIServerEndpoints(result.Stdout)
	if err != nil {
		return "", e.failKubeadmUpgrade(record, "apiserver-drain-running", fmt.Errorf("inspect Kubernetes API endpoints: %w", err), false)
	}
	if len(endpoints) < 2 {
		return "", e.completeKubeAPIServerDrain(record, "run kubeadm without draining the only API endpoint", "")
	}
	localAddress, err := localKubeAPIServerAddress(e.Root)
	if err != nil {
		return "", e.failKubeadmUpgrade(record, "apiserver-drain-running", fmt.Errorf("identify local Kubernetes API endpoint: %w", err), false)
	}
	var peer *kubeAPIServerEndpoint
	for i := range endpoints {
		if endpoints[i].Address == localAddress {
			continue
		}
		probe := e.toolRunner()(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "--server", endpoints[i].URL(), "--request-timeout=10s", "get", "--raw=/readyz"}, nil)
		if probe.Err == nil && probe.ExitStatus == 0 {
			peer = &endpoints[i]
			break
		}
	}
	if peer == nil {
		return "", e.failKubeadmUpgrade(record, "apiserver-drain-running", fmt.Errorf("no alternate Kubernetes API endpoint passed authenticated readiness; keep the local API server available and retry after restoring another control plane"), false)
	}
	kubeconfigPath, err := writeKubeadmPeerConfig(e.Root, record.OperationID, peer.URL())
	if err != nil {
		return "", e.failKubeadmUpgrade(record, "apiserver-drain-running", fmt.Errorf("prepare alternate Kubernetes API access: %w", err), false)
	}
	keepKubeconfig := false
	defer func() {
		if !keepKubeconfig {
			_ = os.Remove(rootedRuntimePath(e.Root, kubeconfigPath))
		}
	}()
	probe := e.toolRunner()(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", kubeconfigPath, "--request-timeout=10s", "get", "--raw=/readyz"}, nil)
	if probe.Err != nil || probe.ExitStatus != 0 {
		return "", e.failKubeadmUpgrade(record, "apiserver-drain-running", fmt.Errorf("verify alternate Kubernetes API access: %s", toolFailure(probe)), false)
	}
	result = e.toolRunner()(ctx, []string{"/usr/bin/killall", "-s", "SIGTERM", "kube-apiserver"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return "", e.failKubeadmUpgrade(record, "apiserver-drain-running", fmt.Errorf("gracefully terminate kube-apiserver: %s", toolFailure(result)), false)
	}
	wait := e.WaitBeforeKubeadm
	if wait == nil {
		wait = waitForContext
	}
	if err := wait(ctx, kubeAPIServerDrainDelay); err != nil {
		return "", e.failKubeadmUpgrade(record, "apiserver-drain-running", fmt.Errorf("wait for kube-apiserver connections to drain: %w", err), false)
	}
	if err := e.completeKubeAPIServerDrain(record, "run kubeadm through alternate API endpoint "+peer.String()+" after local connections have closed", peer.String()); err != nil {
		return "", err
	}
	keepKubeconfig = true
	return kubeconfigPath, nil
}

func parseKubeAPIServerEndpoints(data []byte) ([]kubeAPIServerEndpoint, error) {
	var object struct {
		Subsets []struct {
			Addresses []struct {
				IP string `json:"ip"`
			} `json:"addresses"`
			Ports []struct {
				Name     string `json:"name"`
				Port     int    `json:"port"`
				Protocol string `json:"protocol"`
			} `json:"ports"`
		} `json:"subsets"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("decode endpoints: %w", err)
	}
	unique := map[string]kubeAPIServerEndpoint{}
	for _, subset := range object.Subsets {
		for _, port := range subset.Ports {
			if port.Port < 1 || port.Port > 65535 || (port.Protocol != "" && port.Protocol != "TCP") || (len(subset.Ports) > 1 && port.Name != "https") {
				continue
			}
			for _, address := range subset.Addresses {
				ip, err := netip.ParseAddr(strings.TrimSpace(address.IP))
				if err != nil {
					return nil, fmt.Errorf("invalid endpoint address %q", address.IP)
				}
				endpoint := kubeAPIServerEndpoint{Address: ip.Unmap(), Port: uint16(port.Port)}
				unique[endpoint.String()] = endpoint
			}
		}
	}
	endpoints := make([]kubeAPIServerEndpoint, 0, len(unique))
	for _, endpoint := range unique {
		endpoints = append(endpoints, endpoint)
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].String() < endpoints[j].String() })
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no ready HTTPS endpoint addresses")
	}
	return endpoints, nil
}

func localKubeAPIServerAddress(root string) (netip.Addr, error) {
	data, err := os.ReadFile(rootedRuntimePath(root, "/etc/kubernetes/manifests/kube-apiserver.yaml"))
	if err != nil {
		return netip.Addr{}, err
	}
	var pod struct {
		Spec struct {
			Containers []struct {
				Name    string   `yaml:"name"`
				Command []string `yaml:"command"`
			} `yaml:"containers"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &pod); err != nil {
		return netip.Addr{}, fmt.Errorf("decode static pod manifest: %w", err)
	}
	for _, container := range pod.Spec.Containers {
		if container.Name != "kube-apiserver" {
			continue
		}
		for _, arg := range container.Command {
			value, found := strings.CutPrefix(arg, "--advertise-address=")
			if !found {
				continue
			}
			address, err := netip.ParseAddr(strings.TrimSpace(value))
			if err != nil {
				return netip.Addr{}, fmt.Errorf("invalid --advertise-address %q", value)
			}
			return address.Unmap(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("kube-apiserver static pod has no --advertise-address")
}

func writeKubeadmPeerConfig(root, operationID, server string) (string, error) {
	data, err := os.ReadFile(rootedRuntimePath(root, "/etc/kubernetes/admin.conf"))
	if err != nil {
		return "", err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("decode admin kubeconfig: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("admin kubeconfig is not a YAML mapping")
	}
	clusters := kubeconfigMappingValue(document.Content[0], "clusters")
	if clusters == nil || clusters.Kind != yaml.SequenceNode || len(clusters.Content) != 1 {
		return "", fmt.Errorf("admin kubeconfig must contain exactly one cluster")
	}
	cluster := kubeconfigMappingValue(clusters.Content[0], "cluster")
	serverNode := kubeconfigMappingValue(cluster, "server")
	if cluster == nil || cluster.Kind != yaml.MappingNode || serverNode == nil || serverNode.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("admin kubeconfig cluster has no server")
	}
	serverNode.Value = server
	serverNode.Tag = "!!str"
	output, err := yaml.Marshal(&document)
	if err != nil {
		return "", fmt.Errorf("encode alternate kubeconfig: %w", err)
	}
	logicalPath := filepath.ToSlash(filepath.Join("/var/lib/katl/operations", operationID, "kubeadm-peer.conf"))
	hostPath := rootedRuntimePath(root, logicalPath)
	if err := os.Remove(hostPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	file, err := os.OpenFile(hostPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(output); err != nil {
		_ = file.Close()
		_ = os.Remove(hostPath)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(hostPath)
		return "", err
	}
	return logicalPath, nil
}

func kubeconfigMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func (e *Executor) completeKubeAPIServerDrain(record operation.OperationRecord, nextAction, alternateEndpoint string) error {
	_, err := e.Store.Update(record.OperationID, "apiserver-drain-complete", "apiserver-drain-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "apiserver-drain-complete"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "apiserver-drain-running", "apiserver-drain-complete")
		current.PhaseIndex = len(current.CompletedPhases)
		if current.KubeadmUpgradeEvidence != nil {
			current.KubeadmUpgradeEvidence.AlternateAPIEndpoint = alternateEndpoint
		}
		current.UpdatedAt = e.clock()
		current.NextAction = nextAction
		return current, nil
	})
	return err
}

func (e *Executor) resolveKubernetesUpgradePayload(ctx context.Context, record operation.OperationRecord) (operation.OperationRecord, error) {
	request := record.KubernetesSysextUpdate
	if request == nil {
		return record, fmt.Errorf("Kubernetes upgrade request is missing")
	}
	if strings.TrimSpace(request.TargetSysextPath) != "" && strings.TrimSpace(request.TargetSysextSHA256) != "" {
		return record, nil
	}
	if strings.TrimSpace(request.KubernetesBundleSource) == "" || strings.TrimSpace(request.KubernetesBundleRef) == "" {
		return record, fmt.Errorf("Kubernetes bundle reference is missing")
	}
	record, err := e.Store.Update(record.OperationID, "kubernetes-upgrade-target-resolving", "target-resolving", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "target-resolving"
		current.UpdatedAt = e.clock()
		current.NextAction = "download and verify the target Kubernetes bundle"
		return current, nil
	})
	if err != nil {
		return record, err
	}
	request = record.KubernetesSysextUpdate
	cacheDir := rootedRuntimePath(e.Root, filepath.ToSlash(filepath.Join("/var/lib/katl/artifacts/kubernetes-upgrades", record.OperationID)))
	staged, err := kubernetesbundle.FetchAndStage(ctx, kubernetesbundle.Request{
		Source:           request.KubernetesBundleSource,
		Ref:              request.KubernetesBundleRef,
		CacheDir:         cacheDir,
		RuntimeInterface: "katl-runtime-1",
		Architecture:     "x86_64",
		Client:           e.BundleClient,
	})
	if err != nil {
		return record, fmt.Errorf("fetch Kubernetes bundle: %w", err)
	}
	if staged.PayloadVersion != request.TargetPayloadVersion {
		return record, fmt.Errorf("fetched Kubernetes payload %s does not match requested target %s", staged.PayloadVersion, request.TargetPayloadVersion)
	}
	logicalPath, err := runtimeLogicalPath(e.Root, staged.SysextPath)
	if err != nil {
		return record, err
	}
	info, err := os.Stat(staged.SysextPath)
	if err != nil {
		return record, fmt.Errorf("stat fetched Kubernetes sysext: %w", err)
	}
	return e.Store.Update(record.OperationID, "kubernetes-upgrade-target-resolved", "target-resolved", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.KubernetesSysextUpdate.TargetSysextPath = logicalPath
		current.KubernetesSysextUpdate.TargetSysextSHA256 = strings.TrimPrefix(staged.SysextPayloadDigest, "sha256:")
		current.KubernetesSysextUpdate.TargetSysextSize = uint64(info.Size())
		current.KubernetesSysextUpdate.BundleManifestDigest = staged.BundleManifestDigest
		current.Phase = "target-resolved"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "accepted", "target-resolved")
		current.PhaseIndex = len(current.CompletedPhases)
		current.UpdatedAt = e.clock()
		current.NextAction = "capture pre-upgrade etcd evidence when required"
		return current, nil
	})
}

func (e *Executor) prepareKubeadmUpgradeSnapshot(ctx context.Context, record operation.OperationRecord) (operation.OperationRecord, error) {
	request := record.KubernetesSysextUpdate
	if request == nil || request.UpgradeRole == "worker" || strings.TrimSpace(request.SnapshotStorageLocation) != "" {
		return record, nil
	}
	containerResult := e.toolRunner()(ctx, []string{"crictl", "ps", "--state", "Running", "--name", "etcd", "-q"}, nil)
	if containerResult.Err != nil || containerResult.ExitStatus != 0 {
		return record, fmt.Errorf("locate running etcd container: %s", toolFailure(containerResult))
	}
	containers := strings.Fields(string(containerResult.Stdout))
	if len(containers) != 1 {
		return record, fmt.Errorf("expected one running etcd container, got %d", len(containers))
	}
	location := filepath.ToSlash(filepath.Join("/var/lib/etcd", "katl-upgrade-"+record.OperationID+".db"))
	etcdctl := []string{"crictl", "exec", containers[0], "etcdctl", "--endpoints=https://127.0.0.1:2379", "--cacert=/etc/kubernetes/pki/etcd/ca.crt", "--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt", "--key=/etc/kubernetes/pki/etcd/healthcheck-client.key"}
	saveResult := e.toolRunner()(ctx, append(append([]string(nil), etcdctl...), "snapshot", "save", location), nil)
	if saveResult.Err != nil || saveResult.ExitStatus != 0 {
		return record, fmt.Errorf("save pre-upgrade etcd snapshot: %s", toolFailure(saveResult))
	}
	digest, _, err := fileDigest(rootedRuntimePath(e.Root, location))
	if err != nil {
		return record, fmt.Errorf("hash pre-upgrade etcd snapshot: %w", err)
	}
	membersResult := e.toolRunner()(ctx, append(append([]string(nil), etcdctl...), "member", "list"), nil)
	if membersResult.Err != nil || membersResult.ExitStatus != 0 {
		return record, fmt.Errorf("capture pre-upgrade etcd member list: %s", toolFailure(membersResult))
	}
	membersHash := sha256.Sum256(membersResult.Stdout)
	now := e.clock()
	return e.Store.Update(record.OperationID, "kubernetes-upgrade-snapshot-prepared", "snapshot-prepared", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		update := current.KubernetesSysextUpdate
		update.SnapshotRef = "pre-" + update.SourcePayloadVersion + "-to-" + update.TargetPayloadVersion
		update.SnapshotDigest = digest
		update.SnapshotCreatedAt = now.Format(time.RFC3339)
		update.CapturedMemberListDigest = hex.EncodeToString(membersHash[:])
		update.SnapshotStorageLocation = location
		update.SnapshotOperatorIdentity = inventory.Redact(current.Actor)
		current.Phase = "snapshot-prepared"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "snapshot-prepared")
		current.PhaseIndex = len(current.CompletedPhases)
		current.UpdatedAt = now
		current.NextAction = "stage the verified target Kubernetes payload"
		return current, nil
	})
}

func runtimeLogicalPath(root, hostPath string) (string, error) {
	rel, err := filepath.Rel(runtimeRoot(root), hostPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("resolved Kubernetes sysext is outside the runtime root")
	}
	return "/" + filepath.ToSlash(rel), nil
}

func (e *Executor) runKubeadmUpgradeCommand(ctx context.Context, record operation.OperationRecord, phase string, argv []string, mutating bool) error {
	started := e.clock()
	invocationID := strings.TrimSuffix(phase, "-running")
	if mutating {
		scopes := []string{"kubeadm-state", "kubernetes-api"}
		if record.KubernetesSysextUpdate != nil && record.KubernetesSysextUpdate.UpgradeRole != "worker" {
			scopes = append(scopes, "stacked-etcd")
		}
		marker := operation.PreExecMutationMarker{MarkerID: invocationID, InvocationID: invocationID, Phase: phase, Tool: "kubeadm", ArgvDigest: digestArgv(argv), ExpectedMutationScopes: scopes, MarkedAt: started}
		if _, err := e.Store.Update(record.OperationID, invocationID+"-start", "pre-exec-mutation", func(current operation.OperationRecord) (operation.OperationRecord, error) {
			current.Phase = phase
			current.ExternalMutationStarted = true
			current.PreExecMutationMarkers = append(current.PreExecMutationMarkers, marker)
			current.MutationScopes = appendMissing(current.MutationScopes, marker.ExpectedMutationScopes...)
			current.Invocations = append(current.Invocations, operation.InvocationRecord{InvocationID: invocationID, AgentStartID: e.AgentStartID, ExecutorAttemptID: e.AgentStartID, ChildProcess: redactArgv(argv), BootID: currentBootID(), StartedAt: started, Result: "started"})
			current.UpdatedAt = started
			return current, nil
		}); err != nil {
			return err
		}
	} else if _, err := e.Store.Update(record.OperationID, invocationID+"-start", phase, func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = phase
		current.Invocations = append(current.Invocations, operation.InvocationRecord{InvocationID: invocationID, AgentStartID: e.AgentStartID, ExecutorAttemptID: e.AgentStartID, ChildProcess: redactArgv(argv), BootID: currentBootID(), StartedAt: started, Result: "started"})
		current.UpdatedAt = started
		return current, nil
	}); err != nil {
		return err
	}
	toolCtx, cancel := context.WithTimeout(ctx, kubeadmUpgradeTimeout)
	defer cancel()
	result := e.toolRunner()(toolCtx, argv, nil)
	completed := e.clock()
	var artifactErr error
	if len(result.Stdout) > 0 {
		_, artifactErr = e.Store.AddDiagnosticArtifact(record.OperationID, invocationID+"-stdout", []byte(inventory.Redact(string(result.Stdout))), completed)
	}
	if len(result.Stderr) > 0 {
		_, err := e.Store.AddDiagnosticArtifact(record.OperationID, invocationID+"-stderr", []byte(inventory.Redact(string(result.Stderr))), completed)
		artifactErr = errors.Join(artifactErr, err)
	}
	if result.Err != nil || result.ExitStatus != 0 {
		return e.failKubeadmCommand(record, phase, errors.Join(fmt.Errorf("target kubeadm failed: %s", toolFailure(result)), artifactErr), mutating)
	}
	if artifactErr != nil {
		return e.failKubeadmCommand(record, phase, fmt.Errorf("record redacted kubeadm diagnostics: %w", artifactErr), mutating)
	}
	_, err := e.Store.Update(record.OperationID, invocationID+"-complete", phase+"-complete", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		completeInvocation(current.Invocations, invocationID, completed, operation.ResultSucceeded, result)
		if mutating {
			current.MutatingToolRan = true
			current.MutatingToolInvocations = appendMissing(current.MutatingToolInvocations, inventory.Redact(strings.Join(argv, " ")))
		}
		current.CompletedPhases = appendMissing(current.CompletedPhases, phase)
		current.PhaseIndex = len(current.CompletedPhases)
		current.UpdatedAt = completed
		return current, nil
	})
	return err
}

func (e *Executor) failKubeadmCommand(record operation.OperationRecord, phase string, cause error, postMutation bool) error {
	if record.KubeadmControlPlaneConfig != nil {
		return e.failControlPlaneConfig(record, phase, cause)
	}
	return e.failKubeadmUpgrade(record, phase, cause, postMutation)
}

func (e *Executor) stageKubernetesCandidate(previous generation.GenerationSpec, current generation.ExtensionRef, request operation.KubernetesSysextUpdate, operationID string) (generation.ExtensionRef, error) {
	dir, err := generation.GenerationDir(e.Root, request.CandidateGenerationID)
	if err != nil {
		return generation.ExtensionRef{}, err
	}
	targetLogical := filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, request.CandidateGenerationID, "sysext", "kubernetes.raw"))
	targetHost := rootedRuntimePath(e.Root, targetLogical)
	if err := os.MkdirAll(filepath.Dir(targetHost), 0o700); err != nil {
		return generation.ExtensionRef{}, err
	}
	if err := copyVerifiedFile(rootedRuntimePath(e.Root, request.TargetSysextPath), targetHost, request.TargetSysextSHA256); err != nil {
		return generation.ExtensionRef{}, err
	}
	ref := current
	ref.Path = targetLogical
	ref.SHA256 = request.TargetSysextSHA256
	ref.PayloadVersion = request.TargetPayloadVersion
	ref.ArtifactVersion = request.TargetPayloadVersion
	spec := previous
	spec.GenerationID = request.CandidateGenerationID
	spec.PreviousGenerationID = previous.GenerationID
	spec.Boot.LoaderEntryPath = "loader/entries/katl-" + request.CandidateGenerationID + ".conf"
	spec.Sysexts = replaceKubernetesRef(spec.Sysexts, ref)
	spec.KubernetesUpgrade = &generation.KubernetesUpgrade{
		OperationID:             operationID,
		TargetKubeadmAccessMode: kubeadmAccessOperationPrivate,
		KubeletActivationGate:   kubeletGateOperationReleased,
	}
	if err := e.cloneCandidateConfext(previous.GenerationID, request.CandidateGenerationID, spec.Confexts); err != nil {
		return generation.ExtensionRef{}, err
	}
	for i := range spec.Confexts {
		spec.Confexts[i].Path = filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, request.CandidateGenerationID, "confext"))
	}
	spec.CreatedAt = e.clock()
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCandidate, generation.BootStatePending, generation.HealthStateUnknown, e.clock())
	if err != nil {
		return generation.ExtensionRef{}, err
	}
	if err := generation.WriteGeneration(e.Root, spec, status); err != nil {
		return generation.ExtensionRef{}, fmt.Errorf("write candidate generation %s (%s): %w", request.CandidateGenerationID, dir, err)
	}
	return ref, nil
}

func (e *Executor) cloneCandidateConfext(previousID, candidateID string, refs []generation.GeneratedConfext) error {
	if len(refs) == 0 {
		return nil
	}
	previousLogical := filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, previousID, "confext"))
	for _, ref := range refs {
		if filepath.ToSlash(filepath.Clean(ref.Path)) != previousLogical {
			return fmt.Errorf("confext %s path %q does not belong to previous generation %s", ref.Name, ref.Path, previousID)
		}
	}
	candidateLogical := filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, candidateID, "confext"))
	if err := os.CopyFS(rootedRuntimePath(e.Root, candidateLogical), os.DirFS(rootedRuntimePath(e.Root, previousLogical))); err != nil {
		return fmt.Errorf("inherit confext for candidate generation %s: %w", candidateID, err)
	}
	return nil
}

func (e *Executor) installKubeletGate(gatePath, unit string) error {
	path := rootedRuntimePath(e.Root, "/run/systemd/system/"+unit)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := "[Unit]\nConditionPathExists=" + gatePath + "\n"
	return os.WriteFile(path, []byte(content), 0o644)
}

func (e *Executor) activateKubernetesCandidate(ctx context.Context, current, candidate generation.ExtensionRef) error {
	activation := rootedRuntimePath(e.Root, candidate.ActivationPath)
	if err := os.MkdirAll(filepath.Dir(activation), 0o755); err != nil {
		return err
	}
	_ = os.Remove(activation)
	if err := os.Symlink(candidate.Path, activation); err != nil {
		return err
	}
	for _, argv := range [][]string{{"systemd-sysext", "refresh"}, {"systemctl", "daemon-reload"}} {
		result := e.toolRunner()(ctx, argv, nil)
		if result.Err != nil || result.ExitStatus != 0 {
			_ = os.Remove(activation)
			_ = os.Symlink(current.Path, activation)
			_ = e.toolRunner()(ctx, []string{"systemd-sysext", "refresh"}, nil)
			return fmt.Errorf("%s: %s", argv[0], toolFailure(result))
		}
	}
	return nil
}

func (e *Executor) checkKubeadmUpgradeHealth(ctx context.Context, request operation.KubernetesSysextUpdate) error {
	commands := [][]string{{"systemctl", "is-active", "--quiet", "containerd.service"}, {"systemctl", "is-active", "--quiet", "kubelet.service"}, {"kubelet", "--version"}}
	if request.UpgradeRole != "worker" {
		commands = append(commands, []string{"kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "--raw=/readyz"})
	}
	for _, argv := range commands {
		result := e.toolRunner()(ctx, argv, nil)
		if result.Err != nil || result.ExitStatus != 0 {
			return fmt.Errorf("health command %s: %s", argv[0], toolFailure(result))
		}
		if argv[0] == "kubelet" && !strings.Contains(string(result.Stdout), strings.TrimPrefix(request.TargetPayloadVersion, "v")) {
			return fmt.Errorf("target kubelet version not observed: %q", strings.TrimSpace(string(result.Stdout)))
		}
	}
	return nil
}

func (e *Executor) completeKubeadmUpgrade(ctx context.Context, record operation.OperationRecord) error {
	now := e.clock()
	if err := e.removeKubeletGate(ctx, record); err != nil {
		return e.failKubeadmUpgrade(record, "health-check-running", err, true)
	}
	if err := e.promoteCandidateGenerationLive(ctx, record, now, "Kubernetes sysext activated live and passed local health checks"); err != nil {
		return e.failKubeadmUpgrade(record, "health-check-running", err, true)
	}
	_, err := e.Store.Update(record.OperationID, "kubeadm-upgrade-healthy", "healthy", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "healthy"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "kubelet-stop-running", "sysext-refresh-running", "kubelet-restart-running", "health-check-running", "healthy")
		current.PhaseIndex = len(current.CompletedPhases)
		current.KubeadmUpgradeEvidence.KubeletGateState = "target-observed"
		current.ActivationState = operation.ActivationStateActiveLive
		current.GenerationCommitState = operation.GenerationCommitCommitted
		current.PostKubeadmHealthState = operation.PostKubeadmHealthPassed
		current.BootHealthPending = false
		current.Terminal = true
		current.Result = operation.ResultSucceeded
		current.CompletedAt = &now
		current.UpdatedAt = now
		current.NextAction = "continue the serialized online rollout"
		return current, nil
	})
	return err
}

func (e *Executor) removeKubeletGate(ctx context.Context, record operation.OperationRecord) error {
	if record.KubeadmUpgradeEvidence == nil || strings.TrimSpace(record.KubeadmUpgradeEvidence.KubeletGateEnforcementUnit) == "" {
		return fmt.Errorf("kubelet activation gate enforcement unit is not recorded")
	}
	path := rootedRuntimePath(e.Root, "/run/systemd/system/"+record.KubeadmUpgradeEvidence.KubeletGateEnforcementUnit)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	result := e.toolRunner()(ctx, []string{"systemctl", "daemon-reload"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("remove completed kubelet activation gate: %s", toolFailure(result))
	}
	return nil
}

func (e *Executor) failKubeadmUpgrade(record operation.OperationRecord, phase string, cause error, postMutation bool) error {
	now := e.clock()
	latest, readErr := e.Store.Read(record.OperationID)
	mutated := postMutation || (readErr == nil && latest.ExternalMutationStarted)
	cleanupErr := e.restoreSourceKubernetesAfterFailure(latest)
	cause = errors.Join(cause, cleanupErr)
	var abandonErr error
	if !mutated {
		abandonErr = e.abandonKubeadmCandidate(record.CandidateGenerationID, record.OperationID, now)
	}
	_, err := e.Store.Update(record.OperationID, "kubeadm-upgrade-failed-"+strings.ReplaceAll(phase, "-", "_"), "kubeadm-upgrade-failed", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = phase
		current.Terminal = true
		current.CompletedAt = &now
		current.UpdatedAt = now
		current.FailureReason = inventory.Redact(cause.Error())
		current.Result = "failed"
		current.GenerationCommitState = operation.GenerationCommitAbandoned
		current.NextAction = "fix the pre-mutation failure and submit a new explicit upgrade operation"
		if mutated {
			current.RecoveryRequired = true
			current.Result = operation.ResultFailedNeedsRepair
			current.GenerationCommitState = operation.GenerationCommitCandidate
			current.ActivationState = operation.ActivationStateFailed
			current.NextAction = "inspect kubeadm diagnostics and use explicit kubeadm-aware repair; host rollback does not repair Kubernetes or etcd state"
			current.PostMutationRollbackAllowed = true
			current.HostRollback = current.PreviousGenerationID
		}
		return current, nil
	})
	return errors.Join(cause, readErr, abandonErr, err)
}

func (e *Executor) restoreSourceKubernetesAfterFailure(record operation.OperationRecord) error {
	var restoreErr error
	restartKubelet := record.ActivationState == operation.ActivationStateActivating || record.ActivationState == operation.ActivationStateFailed
	previous := strings.TrimSpace(record.PreviousGenerationID)
	if previous != "" {
		spec, _, err := generation.ReadGeneration(e.Root, previous)
		if err == nil {
			if ref, ok := kubernetesRef(spec.Sysexts); ok {
				activation := rootedRuntimePath(e.Root, ref.ActivationPath)
				_ = os.Remove(activation)
				if err := os.MkdirAll(filepath.Dir(activation), 0o755); err != nil {
					restoreErr = errors.Join(restoreErr, err)
				} else if err := os.Symlink(ref.Path, activation); err != nil {
					restoreErr = errors.Join(restoreErr, err)
				}
				result := e.toolRunner()(context.Background(), []string{"systemd-sysext", "refresh"}, nil)
				if result.Err != nil || result.ExitStatus != 0 {
					restoreErr = errors.Join(restoreErr, fmt.Errorf("restore source Kubernetes sysext: %s", toolFailure(result)))
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			restoreErr = errors.Join(restoreErr, err)
		}
	}
	unit := "kubelet.service.d/20-katl-upgrade-gate.conf"
	if record.KubeadmUpgradeEvidence != nil && strings.TrimSpace(record.KubeadmUpgradeEvidence.KubeletGateEnforcementUnit) != "" {
		unit = record.KubeadmUpgradeEvidence.KubeletGateEnforcementUnit
	}
	gatePath := rootedRuntimePath(e.Root, "/run/systemd/system/"+unit)
	removed := false
	if err := os.Remove(gatePath); err == nil {
		removed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		restoreErr = errors.Join(restoreErr, err)
	}
	if removed {
		result := e.toolRunner()(context.Background(), []string{"systemctl", "daemon-reload"}, nil)
		if result.Err != nil || result.ExitStatus != 0 {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("remove failed kubelet activation gate: %s", toolFailure(result)))
		}
	}
	if record.KubeadmUpgradeEvidence != nil && strings.TrimSpace(record.KubeadmUpgradeEvidence.KubeletGateTokenPath) != "" {
		_ = os.Remove(rootedRuntimePath(e.Root, record.KubeadmUpgradeEvidence.KubeletGateTokenPath))
	}
	if restartKubelet {
		result := e.toolRunner()(context.Background(), []string{"systemctl", "restart", "kubelet.service"}, nil)
		if result.Err != nil || result.ExitStatus != 0 {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restart source kubelet after failed live activation: %s", toolFailure(result)))
		}
	}
	return restoreErr
}

func (e *Executor) abandonKubeadmCandidate(candidate, operationID string, now time.Time) error {
	if strings.TrimSpace(candidate) == "" {
		return nil
	}
	spec, status, err := generation.ReadGeneration(e.Root, candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if status.CommitState != generation.CommitStateCandidate {
		return nil
	}
	status.CommitState = generation.CommitStateAbandoned
	status.BootState = generation.BootStateFailed
	status.UpdatedAt = now
	status.StatusTransitions = append(status.StatusTransitions, generation.StatusTransition{At: now, OperationID: operationID, Reason: "Kubernetes upgrade failed before kubeadm mutation", CommitState: status.CommitState, BootState: status.BootState, HealthState: status.HealthState})
	return generation.WriteGenerationStatus(e.Root, spec, status)
}

func kubernetesRef(refs []generation.ExtensionRef) (generation.ExtensionRef, bool) {
	for _, ref := range refs {
		if ref.Name == "kubernetes" {
			return ref, true
		}
	}
	return generation.ExtensionRef{}, false
}
func replaceKubernetesRef(refs []generation.ExtensionRef, next generation.ExtensionRef) []generation.ExtensionRef {
	out := append([]generation.ExtensionRef(nil), refs...)
	for i := range out {
		if out[i].Name == "kubernetes" {
			out[i] = next
			return out
		}
	}
	return append(out, next)
}
func rootedRuntimePath(root, path string) string {
	return filepath.Join(runtimeRoot(root), strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)))
}
func verifyFileDigest(path, want string, size uint64) error {
	got, n, err := fileDigest(path)
	if err != nil {
		return err
	}
	if size > 0 && uint64(n) != size {
		return fmt.Errorf("size %d, want %d", n, size)
	}
	if got != want {
		return fmt.Errorf("sha256 %s, want %s", got, want)
	}
	return nil
}
func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}
func copyVerifiedFile(source, target, want string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, hash), in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		return errors.Join(copyErr, closeErr)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		_ = os.Remove(target)
		return fmt.Errorf("copied sysext sha256 %s, want %s", got, want)
	}
	return nil
}
