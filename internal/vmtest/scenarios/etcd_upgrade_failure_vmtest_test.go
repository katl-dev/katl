package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/distribution/reference"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/artifact"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/vmtest"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"
)

func runEtcdUpgradeFailureProof(t *testing.T, ctx context.Context, result vmtest.Result, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string, endpoint, kubeconfig string) error {
	t.Helper()
	dir := filepath.Join(result.RunDir, "etcd-upgrade-failure")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cp1 := nodeByName(nodes, "cp-1")
	repo := katlRepoRoot(t)
	localArtifact := filepath.Join(repo, "_build/mkosi/katl-kubernetes-upgrade.raw")
	metadata, err := artifact.ReadLocal(localArtifact + ".json")
	if err != nil {
		return err
	}
	images, err := kubernetesImageReferences(ctx, repo, metadata.PayloadVersion)
	if err != nil {
		return err
	}
	targetImage := ""
	for _, image := range images {
		if strings.HasPrefix(filepath.Base(image), "etcd:") {
			targetImage = image
		}
	}
	named, err := reference.ParseNormalizedNamed(targetImage)
	if err != nil {
		return err
	}
	tagged, ok := named.(reference.Tagged)
	if !ok {
		return fmt.Errorf("target kubeadm etcd image has no tag for fault injection")
	}
	failureTag := strings.SplitN(tagged.Tag(), "-", 2)[0] + "-9999"
	// Only cp-1 receives a syntactically valid target image without an etcd
	// executable. Kubeadm owns the actual version and image selection.
	failingImage := reference.TrimNamed(named).Name() + ":" + failureTag
	if _, err := buildBootstrapImageFixtures(ctx, repo, dir); err != nil {
		return err
	}
	archive := filepath.Join(dir, "failed-etcd.tar")
	if err := writeDockerArchive(archive, failingImage, filepath.Join(dir, "net-server"), "/net-server"); err != nil {
		return err
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		return err
	}
	guestArchive := "/var/lib/katl/test-artifacts/failed-etcd.tar"
	if err := writeNodeFileChunked(ctx, cp1, guestArchive, data, 0o600); err != nil {
		return err
	}
	loaded, err := runNodeCommand(ctx, cp1, []string{"ctr", "-n", "k8s.io", "images", "import", guestArchive}, 128<<10)
	if err != nil || loaded.ExitStatus != 0 {
		return fmt.Errorf("import failing etcd image: %v", err)
	}
	live, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "get", "configmap", "kubeadm-config", "-o", "jsonpath={.data.ClusterConfiguration}")
	if err != nil {
		return err
	}
	var config map[string]any
	if err := yaml.Unmarshal(live, &config); err != nil {
		return err
	}
	etcd, ok := config["etcd"].(map[string]any)
	if !ok {
		etcd = map[string]any{}
		config["etcd"] = etcd
	}
	local, ok := etcd["local"].(map[string]any)
	if !ok {
		local = map[string]any{}
		etcd["local"] = local
	}
	local["imageRepository"] = strings.TrimSuffix(reference.TrimNamed(named).Name(), "/etcd")
	local["imageTag"] = failureTag
	configured, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{"data": map[string]string{"ClusterConfiguration": string(configured)}})
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "kube-system", "patch", "configmap", "kubeadm-config", "--type=merge", "-p", string(patch)); err != nil {
		return err
	}

	configPath, contextPath, err := writeThreeControlPlaneUpgradeInput(ctx, dir, nodes, addresses, endpoint, metadata.PayloadVersion)
	if err != nil {
		return err
	}
	katlctl := buildKatlctlCommand(t, ctx, repo)
	input, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	mislabeled := strings.Replace(string(input), "    - name: cp-1\n      controlPlane: true", "    - name: cp-1\n      controlPlane: false", 1)
	if mislabeled == string(input) {
		return fmt.Errorf("fixture has no cp-1 control-plane intent")
	}
	mislabeledPath := filepath.Join(dir, "mislabeled-cluster.yaml")
	if err := os.WriteFile(mislabeledPath, []byte(mislabeled), 0o600); err != nil {
		return err
	}
	_, roleError, err := runProofKatlctl(ctx, katlctl, dir, "role-refusal", "kubernetes", "upgrade", "--config", mislabeledPath, "--context-file", contextPath, "--artifact", localArtifact, "--plan", "--output", "json")
	if err == nil || !strings.Contains(roleError, "requested upgrade role does not match local control-plane state") {
		return fmt.Errorf("mislabeled voter did not hit the agent role guard: %v: %s", err, roleError)
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "--server=https://"+addresses["cp-2"]+":6443", "create", "configmap", "katl-upgrade-writes", "--from-literal=value=0"); err != nil {
		return err
	}
	writeCtx, stopWrites := context.WithCancel(ctx)
	defer stopWrites()
	type writesResult struct {
		Count int
		Error error
	}
	writesDone := make(chan writesResult, 1)
	go func() {
		count := 0
		for {
			select {
			case <-writeCtx.Done():
				writesDone <- writesResult{Count: count}
				return
			default:
			}
			value := strconv.Itoa(count + 1)
			peer, other := "cp-2", "cp-3"
			if count%2 == 1 {
				peer, other = other, peer
			}
			_, err := kubectlOutput(ctx, kubeconfig, "--server=https://"+addresses[peer]+":6443", "--request-timeout=10s", "patch", "configmap", "katl-upgrade-writes", "--type=merge", "-p", `{"data":{"value":"`+value+`"}}`)
			if err == nil {
				var read []byte
				read, err = kubectlOutput(ctx, kubeconfig, "--server=https://"+addresses[other]+":6443", "--request-timeout=10s", "get", "configmap", "katl-upgrade-writes", "-o", "jsonpath={.data.value}")
				if err == nil && string(read) != value {
					err = fmt.Errorf("acknowledged value %s read back as %s", value, read)
				}
			}
			if err != nil {
				writesDone <- writesResult{Count: count, Error: err}
				return
			}
			count++
			select {
			case <-writeCtx.Done():
				writesDone <- writesResult{Count: count}
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	stdout, stderr, upgradeErr := runProofKatlctl(ctx, katlctl, dir, "upgrade", "kubernetes", "upgrade", "--config", configPath, "--context-file", contextPath, "--artifact", localArtifact, "--timeout", "10m", "--output", "json")
	stopWrites()
	writes := <-writesDone
	writeEvidence, _ := json.Marshal(map[string]any{"acknowledgedWrites": writes.Count, "error": fmt.Sprint(writes.Error)})
	_ = os.WriteFile(filepath.Join(dir, "writes.json"), writeEvidence, 0o600)
	if upgradeErr == nil {
		return fmt.Errorf("etcd startup failure did not stop rollout")
	}
	if writes.Error != nil || writes.Count < 2 {
		return fmt.Errorf("surviving-voter writes: %d acknowledged: %v", writes.Count, writes.Error)
	}
	var report struct {
		RecoveryRequired bool `json:"recoveryRequired"`
		Nodes            []struct {
			Name             string                        `json:"name"`
			RecoveryRequired bool                          `json:"recoveryRequired"`
			Etcd             *agentapi.EtcdUpgradeEvidence `json:"etcd"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(stdout, &report); err != nil {
		return fmt.Errorf("upgrade failed without recovery report: %w: %s", err, stderr)
	}
	if !report.RecoveryRequired || len(report.Nodes) != 1 || report.Nodes[0].Name != "cp-1" || !report.Nodes[0].RecoveryRequired || report.Nodes[0].Etcd.GetAfter() == nil {
		return fmt.Errorf("unexpected failed rollout report: %s", stdout)
	}
	for _, peer := range []string{"cp-2", "cp-3"} {
		connector, err := vmtest.VMTestAgentConnector(vmtest.VMTestManagementClusterName)
		if err != nil {
			return err
		}
		connection, err := connector.Connect(ctx, inventory.PlannedNode{Name: peer, Address: addresses[peer], Access: inventory.Access{Method: "agent"}})
		if err != nil {
			return err
		}
		client, ok := connection.Client.(interface {
			ListOperations(context.Context, *agentapi.ListOperationsRequest, ...grpc.CallOption) (*agentapi.ListOperationsResponse, error)
		})
		if !ok {
			connection.Close()
			return fmt.Errorf("agent cannot list operations")
		}
		operations, err := client.ListOperations(ctx, &agentapi.ListOperationsRequest{})
		connection.Close()
		if err != nil {
			return err
		}
		for _, op := range operations.Operations {
			if op.OperationKind == "kubeadm-upgrade" {
				return fmt.Errorf("%s received an upgrade operation after cp-1 failed", peer)
			}
		}
		value, err := kubectlOutput(ctx, kubeconfig, "--server=https://"+addresses[peer]+":6443", "get", "configmap", "katl-upgrade-writes", "-o", "jsonpath={.data.value}")
		if err != nil || string(value) != strconv.Itoa(writes.Count) {
			return fmt.Errorf("%s lost acknowledged writes: %s, %v", peer, value, err)
		}
	}
	after, err := readLiveEtcdStatus(ctx, "cp-2", addresses["cp-2"])
	if err != nil || after.GetHealthyMembers() < 2 || len(after.GetMembers()) != 3 {
		return fmt.Errorf("surviving etcd quorum: %v, %v", after, err)
	}
	return nil
}
