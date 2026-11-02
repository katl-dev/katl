package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/operation"
	"gopkg.in/yaml.v3"
)

func (e *Executor) executeKubeadmUpgradePrepare(ctx context.Context, record operation.OperationRecord) error {
	var err error
	record, err = e.resolveKubernetesUpgradePayload(ctx, record)
	if err != nil {
		return e.failKubeadmUpgrade(record, "resolve-target", err, false)
	}
	request := record.KubernetesSysextUpdate
	if request.UpgradeRole == "worker" {
		return e.failKubeadmUpgrade(record, "etcd-prepare", errors.New("etcd preparation requires a control plane"), false)
	}
	if err := verifyFileDigest(rootedRuntimePath(e.Root, request.TargetSysextPath), request.TargetSysextSHA256, request.TargetSysextSize); err != nil {
		return e.failKubeadmUpgrade(record, "etcd-prepare", err, false)
	}
	toolRoot := rootedRuntimePath(e.Root, filepath.Join("/var/lib/katl/operations", record.OperationID, "tools/kubernetes"))
	if err := os.MkdirAll(toolRoot, 0o700); err != nil {
		return e.failKubeadmUpgrade(record, "etcd-prepare", err, false)
	}
	// Register cleanup before acquisition: a failed mount can still leave a view.
	defer e.toolRunner()(context.Background(), []string{"systemd-dissect", "--umount", toolRoot}, nil)
	result := e.toolRunner()(ctx, []string{"systemd-dissect", "--mount", "--read-only", rootedRuntimePath(e.Root, request.TargetSysextPath), toolRoot}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return e.failKubeadmUpgrade(record, "etcd-prepare", fmt.Errorf("mount target kubeadm: %s", toolFailure(result)), false)
	}
	kubeadm := filepath.Join(toolRoot, "usr/bin/kubeadm")
	version := e.toolRunner()(ctx, []string{kubeadm, "version", "-o", "short"}, nil)
	if version.Err != nil || version.ExitStatus != 0 || strings.TrimSpace(string(version.Stdout)) != request.TargetPayloadVersion {
		return e.failKubeadmUpgrade(record, "etcd-prepare", fmt.Errorf("target kubeadm version does not match %s", request.TargetPayloadVersion), false)
	}
	record, err = e.prepareUpgradeEtcd(ctx, record, kubeadm)
	if err != nil {
		return e.failKubeadmUpgrade(record, "etcd-prepare", err, false)
	}
	now := e.clock()
	_, err = e.Store.Update(record.OperationID, "etcd-prepared", "etcd-prepared", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "etcd-prepared"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "etcd-prepared")
		current.PhaseIndex = len(current.CompletedPhases)
		current.Terminal = true
		current.Result = operation.ResultSucceeded
		current.CompletedAt = &now
		current.UpdatedAt = now
		current.NextAction = "review the etcd image transition; all control planes must finish preparation before the first upgrade request"
		return current, nil
	})
	return err
}

func (e *Executor) prepareUpgradeEtcd(ctx context.Context, record operation.OperationRecord, kubeadm string) (operation.OperationRecord, error) {
	evidence, err := e.inspectEtcdUpgrade(ctx, record, kubeadm)
	if err != nil {
		return record, err
	}
	before, inspectErr := e.observeEtcd(ctx, nil)
	evidence.Before = before
	err = errors.Join(inspectErr, validateUpgradeEtcd(before, nil, true))
	if err == nil && (before.LocalImage != evidence.SourceImage || before.LocalContainerID == "") {
		err = errors.New("running etcd image does not match its manifest")
	}
	if err == nil {
		name, nameErr := kubernetesNodeName(e.Root)
		if nameErr != nil {
			err = nameErr
		} else {
			for _, member := range before.Members {
				if member.ID == before.LocalMemberID && member.Name != name {
					err = fmt.Errorf("local etcd member %s does not match node %s", member.Name, name)
				}
			}
		}
	}
	if err == nil {
		preflight, preflightErr := e.kubeadmUpgradePreflightArgs(ctx)
		if preflightErr != nil {
			err = preflightErr
		} else {
			argv := append([]string{kubeadm, "upgrade", "plan", record.KubernetesSysextUpdate.TargetPayloadVersion}, preflight...)
			result := e.toolRunner()(ctx, argv, nil)
			if len(result.Stdout) > 0 {
				_, err = e.Store.AddDiagnosticArtifact(record.OperationID, "kubeadm-upgrade-plan", []byte(inventory.Redact(string(result.Stdout))), e.clock())
			}
			if result.Err != nil || result.ExitStatus != 0 {
				err = errors.Join(err, fmt.Errorf("target kubeadm rejected upgrade plan: %s", toolFailure(result)))
			}
		}
	}
	if err != nil {
		evidence.Failure = inventory.Redact(err.Error())
	}
	record, storeErr := e.Store.Update(record.OperationID, "etcd-inspected", "etcd-inspected", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.KubeadmUpgradeEvidence.Etcd = evidence
		return current, nil
	})
	if err != nil || storeErr != nil {
		return record, errors.Join(err, storeErr)
	}
	if record.KubernetesSysextUpdate.PreviewOnly {
		return record, nil
	}
	record, err = e.prepareKubeadmUpgradeSnapshot(ctx, record)
	if err != nil {
		return record, err
	}
	return record, nil
}

func (e *Executor) inspectEtcdUpgrade(ctx context.Context, record operation.OperationRecord, kubeadm string) (*operation.EtcdUpgradeEvidence, error) {
	if e.inspectUpgradeEtcd != nil {
		return e.inspectUpgradeEtcd(ctx, record, kubeadm)
	}
	source, err := readEtcdImage(rootedRuntimePath(e.Root, "/etc/kubernetes/manifests/etcd.yaml"))
	if err != nil {
		return nil, err
	}
	patchesDir, err := e.prepareKubeadmPatches(record.OperationID, record.PreviousGenerationID)
	if err != nil {
		return nil, err
	}
	result := e.toolRunner()(ctx, []string{"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "--request-timeout=30s", "-n", "kube-system", "get", "configmap", "kubeadm-config", "-o", "jsonpath={.data.ClusterConfiguration}"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return nil, fmt.Errorf("inspect kubeadm etcd selection: %s", toolFailure(result))
	}
	var config map[string]any
	if err := yaml.Unmarshal(result.Stdout, &config); err != nil {
		return nil, err
	}
	if config["kind"] != "ClusterConfiguration" || config["apiVersion"] != "kubeadm.k8s.io/v1beta4" {
		return nil, errors.New("unrecognized live kubeadm ClusterConfiguration")
	}
	if etcd, ok := config["etcd"].(map[string]any); ok && etcd["external"] != nil {
		return nil, errors.New("external etcd upgrades are unsupported")
	}
	config["kubernetesVersion"] = record.KubernetesSysextUpdate.TargetPayloadVersion
	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, err
	}
	name, err := kubernetesNodeName(e.Root)
	if err != nil {
		return nil, err
	}
	endpoint, err := localKubeAPIServerEndpoint(e.Root)
	if err != nil {
		return nil, err
	}
	initInput := map[string]any{
		"apiVersion": "kubeadm.k8s.io/v1beta4", "kind": "InitConfiguration",
		"nodeRegistration": map[string]any{"name": name, "criSocket": "unix:///run/containerd/containerd.sock"},
		"localAPIEndpoint": map[string]any{"advertiseAddress": endpoint.Address.String(), "bindPort": endpoint.Port},
	}
	if patchesDir != "" {
		initInput["patches"] = map[string]any{"directory": rootedRuntimePath(e.Root, patchesDir)}
	}
	initConfig, err := yaml.Marshal(initInput)
	if err != nil {
		return nil, err
	}
	path := rootedRuntimePath(e.Root, filepath.Join("/var/lib/katl/operations", record.OperationID, "etcd-preview-config.yaml"))
	if err := os.WriteFile(path, append(append(initConfig, []byte("---\n")...), data...), 0o600); err != nil {
		return nil, err
	}
	previewDir := rootedRuntimePath(e.Root, filepath.Join("/var/lib/katl/operations", record.OperationID, "tools/etcd-preview"))
	// Never accept a manifest left by an earlier attempt. Kubeadm uses its native
	// dry-run directory, so it applies patches without touching the serving Pod.
	if err := os.RemoveAll(previewDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(previewDir, 0o700); err != nil {
		return nil, err
	}
	argv := []string{"env", "KUBEADM_INIT_DRYRUN_DIR=" + previewDir, kubeadm, "init", "phase", "etcd", "local", "--dry-run", "--config", path}
	result = e.toolRunner()(ctx, argv, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return nil, fmt.Errorf("render target kubeadm etcd manifest: %s", toolFailure(result))
	}
	target, err := readEtcdImage(filepath.Join(previewDir, "etcd.yaml"))
	if err != nil {
		return nil, fmt.Errorf("inspect rendered kubeadm etcd manifest: %w", err)
	}
	return &operation.EtcdUpgradeEvidence{SourceImage: source, TargetImage: target, ImageChanges: source != target}, nil
}

func readEtcdImage(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var pod struct {
		Spec struct {
			Containers []struct {
				Name  string `yaml:"name"`
				Image string `yaml:"image"`
			} `yaml:"containers"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &pod); err != nil {
		return "", err
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == "etcd" && container.Image != "" {
			return container.Image, nil
		}
	}
	return "", errors.New("local etcd manifest has no etcd image")
}

// Verify the embedded snapshot checksum, independently of the file digest used
// to carry the artifact. A digest alone would happily identify a truncated DB.
func verifyEtcdSnapshot(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() <= sha256.Size || info.Size()%512 != sha256.Size {
		return errors.New("etcd snapshot has no embedded SHA-256 checksum")
	}
	hash := sha256.New()
	if _, err := io.CopyN(hash, file, info.Size()-sha256.Size); err != nil {
		return err
	}
	checksum := make([]byte, sha256.Size)
	if _, err := io.ReadFull(file, checksum); err != nil {
		return err
	}
	if !bytes.Equal(hash.Sum(nil), checksum) {
		return errors.New("etcd snapshot embedded checksum mismatch")
	}
	return nil
}

func (e *Executor) recordSnapshotStatus(ctx context.Context, record operation.OperationRecord, containerID string) (operation.OperationRecord, error) {
	request := record.KubernetesSysextUpdate
	path := rootedRuntimePath(e.Root, request.SnapshotStorageLocation)
	if err := verifyEtcdSnapshot(path); err != nil {
		return record, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return record, err
	}
	result := e.toolRunner()(ctx, []string{"crictl", "exec", containerID, "etcdutl", "snapshot", "status", request.SnapshotStorageLocation, "--write-out=json"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return record, fmt.Errorf("verify etcd snapshot database: %s", toolFailure(result))
	}
	var status struct {
		Hash      *uint64 `json:"hash"`
		Revision  uint64  `json:"revision"`
		TotalKeys uint64  `json:"totalKey"`
	}
	if err := json.Unmarshal(result.Stdout, &status); err != nil {
		return record, err
	}
	if status.Hash == nil || status.Revision == 0 || status.TotalKeys == 0 {
		return record, errors.New("etcd snapshot database status is incomplete")
	}
	if record.KubeadmUpgradeEvidence.Etcd != nil {
		if record.KubeadmUpgradeEvidence.Etcd.Before == nil {
			return record, errors.New("snapshot requires observed etcd membership")
		}
		for _, member := range record.KubeadmUpgradeEvidence.Etcd.Before.Members {
			if member.Revision > int64(status.Revision) {
				return record, errors.New("snapshot is older than pre-upgrade observed revision")
			}
		}
	}
	digest, _, err := fileDigest(path)
	if err != nil {
		return record, err
	}
	return e.Store.Update(record.OperationID, "etcd-snapshot-verified", "snapshot-verified", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		if current.KubeadmUpgradeEvidence.Etcd == nil {
			current.KubeadmUpgradeEvidence.Etcd = &operation.EtcdUpgradeEvidence{}
		}
		current.KubeadmUpgradeEvidence.Etcd.Snapshot = &operation.EtcdSnapshot{Path: request.SnapshotStorageLocation, SHA256: digest, Hash: *status.Hash, Revision: status.Revision, TotalKeys: status.TotalKeys, CreatedAt: e.clock().Format(time.RFC3339)}
		current.KubernetesSysextUpdate.SnapshotRevision = strconv.FormatUint(status.Revision, 10)
		return current, nil
	})
}

func (e *Executor) usePreparedUpgrade(record operation.OperationRecord) (operation.OperationRecord, error) {
	request := record.KubernetesSysextUpdate
	prepared, err := e.Store.Read(request.PreparedOperationID)
	if err != nil {
		return record, err
	}
	preparedUpdate := prepared.KubernetesSysextUpdate
	if prepared.OperationKind != OperationKindKubeadmUpgradePrepare || !prepared.Terminal || prepared.Result != operation.ResultSucceeded || preparedUpdate == nil || preparedUpdate.PreviewOnly {
		return record, errors.New("etcd preparation did not complete with a snapshot; prepare the complete rollout again")
	}
	if prepared.PreviousGenerationID != record.PreviousGenerationID || prepared.ExpectedMachineID != record.ExpectedMachineID ||
		preparedUpdate.UpgradeRole != request.UpgradeRole || preparedUpdate.TargetPayloadVersion != request.TargetPayloadVersion ||
		preparedUpdate.SourcePayloadVersion != request.SourcePayloadVersion || preparedUpdate.KubernetesBundleRef != request.KubernetesBundleRef {
		return record, errors.New("prepared etcd evidence does not match this upgrade; prepare the complete rollout again")
	}
	if preparedUpdate.TargetSysextSHA256 != "" && request.TargetSysextSHA256 != "" && preparedUpdate.TargetSysextSHA256 != request.TargetSysextSHA256 {
		return record, errors.New("target Kubernetes artifact changed after preparation")
	}
	if prepared.KubeadmUpgradeEvidence == nil || prepared.KubeadmUpgradeEvidence.Etcd == nil || prepared.KubeadmUpgradeEvidence.Etcd.Snapshot == nil {
		return record, errors.New("prepared etcd snapshot evidence is missing")
	}

	return e.Store.Update(record.OperationID, "etcd-preparation-referenced", "etcd-preparation-referenced", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		preparedRequest := *preparedUpdate
		preparedRequest.CandidateGenerationID = request.CandidateGenerationID
		preparedRequest.UpgradeRole = request.UpgradeRole
		preparedRequest.PreparedOperationID = request.PreparedOperationID
		// Bundle resolution belongs to this operation; preparation's artifact
		// has a different cache path even when the verified bytes are identical.
		preparedRequest.TargetSysextPath = request.TargetSysextPath
		preparedRequest.TargetSysextSHA256 = request.TargetSysextSHA256
		preparedRequest.TargetSysextSize = request.TargetSysextSize
		preparedRequest.BundleManifestDigest = request.BundleManifestDigest
		current.KubernetesSysextUpdate = &preparedRequest
		current.KubeadmUpgradeEvidence.Etcd = prepared.KubeadmUpgradeEvidence.Etcd
		return current, nil
	})
}

func snapshotMembershipDigest(observed *operation.EtcdObservation) string {
	type identity struct {
		ID, Name string
		PeerURLs []string
		Endpoint string
		Learner  bool
	}
	members := make([]identity, 0, len(observed.Members))
	for _, m := range observed.Members {
		members = append(members, identity{m.ID, m.Name, m.PeerURLs, m.Endpoint, m.Learner})
	}
	data, _ := json.Marshal(members)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
