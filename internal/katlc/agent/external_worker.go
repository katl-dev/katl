package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/externalworker"
	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/persistedrecord"
	"gopkg.in/yaml.v3"
)

func generationExternalWorker(root, generationID string) (bool, error) {
	selected, _, err := configapply.ReadEffectiveGenerationManifest(root, generationID)
	if err == nil {
		return selected.Node.Kubernetes.External, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	// Installed intent supplies ownership when a generation has no materialized
	// manifest. Missing or malformed ownership never defaults to managed mode.
	intent, _, err := installer.ReadClusterIntent(root)
	if err != nil {
		return false, err
	}
	return intent.Kubernetes.External, nil
}

func (e *Executor) prepareExternalWorker(record operation.OperationRecord) error {
	fail := func(err error) error {
		_, markErr := e.failRecordPhase(record.OperationID, "external-worker-start-failed", "start-external-kubelet", "start-external-kubelet", "inspect the worker's kubelet journal and preserve its identity before attempting repair", err)
		return errors.Join(err, markErr)
	}
	configuration, err := configapply.ReadGenerationConfiguration(e.Root, record.ExpectedCurrentGenerationID)
	if err != nil {
		return fail(err)
	}
	node := configuration.Manifest.Node
	input := configuration.KubeadmConfigs[node.Kubernetes.Kubeadm.ConfigRef]
	kubelet, err := externalKubeletConfig(input.Config.Content)
	if err != nil {
		return fail(err)
	}
	bootstrap, err := os.ReadFile(rootedRuntimePath(e.Root, record.BootstrapRequest.TemporaryJoinConfigPath))
	if err != nil {
		return fail(fmt.Errorf("read operation-scoped bootstrap kubeconfig: %w", err))
	}
	if sha256Bytes(bootstrap) != record.BootstrapRequest.JoinMaterialDigest {
		return fail(fmt.Errorf("bootstrap kubeconfig no longer matches accepted material"))
	}
	ca, err := externalworker.Bootstrap(bootstrap, record.BootstrapRequest.ControlPlaneEndpoint)
	if err != nil {
		return fail(err)
	}
	args := []string{"--hostname-override=" + node.Identity.Hostname}
	if node.Kubernetes.Address != "" {
		args = append(args, "--node-ip="+node.Kubernetes.Address)
	}
	var labels []string
	for key, value := range node.Bootstrap.Labels {
		labels = append(labels, key+"="+value)
	}
	sort.Strings(labels)
	if len(labels) > 0 {
		args = append(args, "--node-labels="+strings.Join(labels, ","))
	}
	var taints []string
	for _, taint := range node.Bootstrap.Taints {
		value := taint.Key
		if taint.Value != "" {
			value += "=" + taint.Value
		}
		taints = append(taints, value+":"+taint.Effect)
	}
	if len(taints) > 0 {
		args = append(args, "--register-with-taints="+strings.Join(taints, ","))
	}
	// Persist the mutation boundary before installing bootstrap credentials or
	// starting kubelet. A partial enrollment must never be treated as a clean join.
	_, err = e.Store.Update(record.OperationID, "external-worker-mutation", "external-worker-mutation", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.ExternalMutationStarted = true
		current.MutationScopes = appendMissing(current.MutationScopes, "etc-kubernetes", "kubelet-state", "cluster-objects")
		current.Phase = "start-external-kubelet"
		current.UpdatedAt = e.clock()
		return current, nil
	})
	if err != nil {
		return err
	}
	files := []struct {
		path string
		data []byte
	}{
		{"/etc/kubernetes/pki/ca.crt", ca},
		{"/etc/kubernetes/bootstrap-kubelet.conf", bootstrap},
		{"/var/lib/kubelet/kubeadm-flags.env", []byte("KUBELET_KUBEADM_ARGS=\"" + strings.Join(args, " ") + "\"\n")},
		{"/var/lib/kubelet/config.yaml", kubelet},
	}
	for _, file := range files {
		path := rootedRuntimePath(e.Root, file.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fail(err)
		}
		if err := persistedrecord.WriteFileAtomic(path, file.data, 0o600); err != nil {
			return fail(err)
		}
	}
	return nil
}

func (e *Executor) waitExternalWorkerCertificate(ctx context.Context, record operation.OperationRecord) error {
	if _, err := e.Store.Update(record.OperationID, "external-worker-certificate-wait", "external-worker-certificate-wait", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "post-kubeadm-health"
		current.NextAction = "wait for the external cluster to approve the kubelet client certificate; inspect kubectl get csr"
		current.UpdatedAt = e.clock()
		return current, nil
	}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		certificateReady := externalWorkerCertificate(e.Root, record.BootstrapRequest.InventoryNodeName, e.clock()) && externalKubeletAccess(e.Root, record.BootstrapRequest.ControlPlaneEndpoint) == nil
		status, statusErr := nodeKubernetesStatus(ctx, e.Root, e.toolRunner())
		if certificateReady && statusErr == nil && status.GetState() == "ready" {
			// Kubelet now uses its persisted rotating client certificate. Retain bootstrap
			// credentials only while enrollment is pending, including across a reboot.
			if err := os.Remove(rootedRuntimePath(e.Root, "/etc/kubernetes/bootstrap-kubelet.conf")); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		select {
		case <-ctx.Done():
			cause := fmt.Errorf("waiting for worker %s client certificate and Node readiness: inspect kubectl get csr, approve the expected request, and check CNI and kubelet health, then retry node join: %w", record.BootstrapRequest.InventoryNodeName, ctx.Err())
			_, markErr := e.failRecordPhase(record.OperationID, "external-worker-certificate-pending", "post-kubeadm-health", "post-kubeadm-health", "approve the expected kubelet client CSR and retry node join; preserve the node identity", cause)
			return errors.Join(cause, markErr)
		case <-ticker.C:
		}
	}
}

func externalKubeletConfig(data []byte) ([]byte, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err == io.EOF {
			return nil, fmt.Errorf("external worker has no KubeletConfiguration")
		} else if err != nil {
			return nil, err
		}
		if document["kind"] == "KubeletConfiguration" {
			content, err := yaml.Marshal(document)
			if err != nil {
				return nil, err
			}
			return externalworker.KubeletConfig(content)
		}
	}
}

func externalWorkerCertificate(root, node string, now time.Time) bool {
	path := rootedRuntimePath(root, "/var/lib/kubelet/pki/kubelet-client-current.pem")
	pair, err := tls.LoadX509KeyPair(path, path)
	if err != nil || len(pair.Certificate) == 0 {
		return false
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	return err == nil && certificate.Subject.CommonName == "system:node:"+node && slices.Contains(certificate.Subject.Organization, "system:nodes") && now.After(certificate.NotBefore) && now.Before(certificate.NotAfter)
}

func externalBootstrapCA(root string, bootstrap []byte, endpoint string) error {
	ca, err := externalworker.Bootstrap(bootstrap, endpoint)
	if err != nil {
		return err
	}
	installed, err := os.ReadFile(rootedRuntimePath(root, "/etc/kubernetes/pki/ca.crt"))
	if err != nil || !bytes.Equal(ca, installed) {
		return fmt.Errorf("replacement bootstrap credentials must retain the installed external cluster CA")
	}
	return nil
}

func externalAPIServerVersion(ctx context.Context, root string, run ToolRunner, target string) (string, error) {
	if run == nil {
		run = runChildProcess
	}
	result := run(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", rootedRuntimePath(root, "/etc/kubernetes/kubelet.conf"), "--request-timeout=10s", "get", "--raw=/version"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return "", fmt.Errorf("read API server version using the enrolled worker identity: %s", toolFailure(result))
	}
	var observed struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := json.Unmarshal(result.Stdout, &observed); err != nil {
		return "", fmt.Errorf("decode API server version: %w", err)
	}
	version := strings.SplitN(strings.SplitN(observed.GitVersion, "+", 2)[0], "-", 2)[0]
	serverMajor, serverMinor, _, err := parseKubernetesVersion(version)
	if err != nil {
		return "", fmt.Errorf("invalid API server version %q", observed.GitVersion)
	}
	major, minor, _, err := parseKubernetesVersion(target)
	if err != nil {
		return "", err
	}
	if major != serverMajor || minor > serverMinor || serverMinor-minor > 3 {
		return "", fmt.Errorf("worker target %s is incompatible with observed API server %s; upgrade the external control plane first and keep kubelet within its supported version skew", target, observed.GitVersion)
	}
	return observed.GitVersion, nil
}

func (e *Executor) refreshExternalBootstrap(ctx context.Context, record operation.OperationRecord) error {
	request := record.BootstrapRequest
	if externalWorkerCertificate(e.Root, request.InventoryNodeName, e.clock()) {
		return nil
	}
	data, err := os.ReadFile(rootedRuntimePath(e.Root, request.TemporaryJoinConfigPath))
	if err != nil {
		return err
	}
	if sha256Bytes(data) != request.JoinMaterialDigest {
		return fmt.Errorf("replacement bootstrap credentials no longer match the accepted request")
	}
	if err := externalBootstrapCA(e.Root, data, request.ControlPlaneEndpoint); err != nil {
		return err
	}
	// The continuation receipt precedes this credential refresh. Kubelet's key,
	// configuration, and certificate request remain owned by the existing join.
	if err := persistedrecord.WriteFileAtomic(rootedRuntimePath(e.Root, "/etc/kubernetes/bootstrap-kubelet.conf"), data, 0o600); err != nil {
		return err
	}
	result := e.toolRunner()(ctx, []string{"/usr/bin/systemctl", "restart", "--no-block", "kubelet.service"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("restart kubelet with refreshed bootstrap credentials: %s", toolFailure(result))
	}
	return nil
}

func externalKubeletAccess(root, endpoint string) error {
	data, err := os.ReadFile(rootedRuntimePath(root, "/etc/kubernetes/kubelet.conf"))
	if err != nil {
		return err
	}
	var config struct {
		Clusters []struct {
			Cluster struct {
				Server   string `yaml:"server"`
				CA       string `yaml:"certificate-authority-data"`
				Insecure bool   `yaml:"insecure-skip-tls-verify"`
				Proxy    string `yaml:"proxy-url"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("external kubelet kubeconfig is invalid")
	}
	if len(config.Clusters) != 1 || config.Clusters[0].Cluster.Server != "https://"+endpoint || config.Clusters[0].Cluster.Insecure || config.Clusters[0].Cluster.Proxy != "" {
		return fmt.Errorf("external kubelet endpoint changed; preserve node state for repair")
	}
	ca, err := base64.StdEncoding.DecodeString(config.Clusters[0].Cluster.CA)
	if err != nil {
		return fmt.Errorf("external kubelet CA is invalid")
	}
	installed, err := os.ReadFile(rootedRuntimePath(root, "/etc/kubernetes/pki/ca.crt"))
	if err != nil || len(ca) == 0 || !bytes.Equal(ca, installed) {
		return fmt.Errorf("external kubelet CA changed; preserve node state for repair")
	}
	return nil
}
