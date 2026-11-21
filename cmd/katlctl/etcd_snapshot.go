package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxEtcdSnapshotBytes matches the agent's limit, which follows etcd's largest
// recommended backend quota.
const maxEtcdSnapshotBytes = uint64(8 << 30)

type etcdSnapshotClient interface {
	SaveEtcdSnapshot(context.Context, *agentapi.SaveEtcdSnapshotRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentapi.EtcdSnapshotChunk], error)
}

type etcdSnapshotOptions struct {
	configPath string
	node       string
	outputPath string
	timeout    time.Duration
}

func newEtcdSnapshotCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := etcdSnapshotOptions{timeout: 15 * time.Minute}
	cmd := &cobra.Command{
		Use:   "snapshot --output PATH",
		Short: "Save a verified etcd snapshot on this workstation",
		Long:  "Take an etcd snapshot on a healthy control plane, verify it there, and download it through the management API. The node-local copy is removed when the transfer ends.",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return runEtcdSnapshot(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.configPath, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().StringVar(&opts.node, "node", "", "control-plane node to snapshot; defaults to the first healthy one")
	cmd.Flags().StringVar(&opts.outputPath, "output", "", "new snapshot file to create (stored with mode 0600)")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "snapshot and transfer timeout")
	return cmd
}

func runEtcdSnapshot(ctx context.Context, opts etcdSnapshotOptions, stdout, stderr io.Writer) error {
	output := filepath.Clean(strings.TrimSpace(opts.outputPath))
	if strings.TrimSpace(opts.outputPath) == "" {
		return errors.New("--output is required; name a new file for the snapshot")
	}
	if opts.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if err := requireNewFile(output); err != nil {
		return err
	}
	var err error
	ctx, err = managementContextForCluster(ctx, opts.configPath, "", "")
	if err != nil {
		return err
	}
	inv, err := loadWipeInventory(ctx, opts.configPath, "", stderr)
	if err != nil {
		return err
	}
	candidates, err := etcdSnapshotCandidates(inv, opts.node)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	var skipped []error
	for _, candidate := range candidates {
		node, err := refreshEtcdSnapshotNode(ctx, opts.configPath, candidate.Name, stderr)
		var info *agentapi.EtcdSnapshotInfo
		if err == nil {
			info, err = saveEtcdSnapshot(node.ctx, node.PlannedNode, output, stderr)
		}
		var unavailable errSnapshotUnavailable
		if errors.As(err, &unavailable) && opts.node == "" && ctx.Err() == nil {
			fmt.Fprintf(stderr, "etcd snapshot node=%s status=skipped reason=%q\n", candidate.Name, unavailable.Error())
			skipped = append(skipped, err)
			continue
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "etcd snapshot saved path=%s node=%s cluster=%s member=%s revision=%d keys=%d size=%d sha256=%s\n", output, candidate.Name, info.GetClusterId(), info.GetMemberId(), info.GetRevision(), info.GetTotalKeys(), info.GetSizeBytes(), info.GetSha256())
		return err
	}
	return fmt.Errorf("no control plane could take an etcd snapshot; restore etcd health or choose a node with --node: %w", errors.Join(skipped...))
}

type etcdSnapshotNode struct {
	inventory.PlannedNode
	ctx context.Context
}

// refreshEtcdSnapshotNode observes one candidate's current installation, so
// an unreachable control plane only skips that candidate.
func refreshEtcdSnapshotNode(ctx context.Context, configPath, name string, stderr io.Writer) (etcdSnapshotNode, error) {
	refreshed, err := refreshConfiguredManagement(ctx, configPath, "", "", stderr, name)
	if err != nil {
		return etcdSnapshotNode{}, errSnapshotUnavailable{err}
	}
	inv, err := loadWipeInventory(refreshed, configPath, "", stderr)
	if err != nil {
		return etcdSnapshotNode{}, err
	}
	candidates, err := etcdSnapshotCandidates(inv, name)
	if err != nil {
		return etcdSnapshotNode{}, err
	}
	return etcdSnapshotNode{PlannedNode: candidates[0], ctx: refreshed}, nil
}

func etcdSnapshotCandidates(inv inventory.Inventory, requested string) ([]inventory.PlannedNode, error) {
	plan, err := planWipeInventory(inv)
	if err != nil {
		return nil, err
	}
	requested = strings.TrimSpace(requested)
	var candidates []inventory.PlannedNode
	for _, node := range plan.Nodes {
		if node.SystemRole != inventory.RoleControlPlane {
			continue
		}
		if requested == "" || node.Name == requested {
			candidates = append(candidates, node)
		}
	}
	if len(candidates) == 0 && requested != "" {
		return nil, fmt.Errorf("node %q is not a control-plane node in the cluster config", requested)
	}
	if len(candidates) == 0 {
		return nil, errors.New("the cluster config has no control-plane node with stacked etcd")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	return candidates, nil
}

// errSnapshotUnavailable reports a node that could not start a snapshot, so
// another control plane can be tried without leaving a partial file.
type errSnapshotUnavailable struct{ err error }

func (e errSnapshotUnavailable) Error() string { return e.err.Error() }
func (e errSnapshotUnavailable) Unwrap() error { return e.err }

func saveEtcdSnapshot(ctx context.Context, node inventory.PlannedNode, output string, progress io.Writer) (*agentapi.EtcdSnapshotInfo, error) {
	conn, err := newWipeClusterConnector().Connect(ctx, node)
	if err != nil {
		return nil, errSnapshotUnavailable{fmt.Errorf("connect %s: %w", node.Name, err)}
	}
	defer closeAgentConnection(conn)
	nodeStatus, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return nil, errSnapshotUnavailable{fmt.Errorf("inspect node identity through %s: %w", node.Name, err)}
	}
	if err := verifyPlannedStatus(managementTarget{nodeName: node.Name, endpoint: conn.Endpoint, enrollmentID: node.EnrollmentID, machineID: node.MachineID}, nodeStatus); err != nil {
		return nil, err
	}
	client, ok := conn.Client.(etcdSnapshotClient)
	if !ok {
		return nil, errSnapshotUnavailable{fmt.Errorf("node %s agent does not support etcd snapshots", node.Name)}
	}
	if progress != nil {
		fmt.Fprintf(progress, "etcd snapshot node=%s status=saving\n", node.Name)
	}
	stream, err := client.SaveEtcdSnapshot(ctx, &agentapi.SaveEtcdSnapshotRequest{})
	if err != nil {
		return nil, etcdSnapshotStartError(node.Name, err)
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, etcdSnapshotStartError(node.Name, err)
	}
	info := first.GetInfo()
	if info == nil || len(first.GetData()) != 0 {
		return nil, fmt.Errorf("node %s sent snapshot data before its description", node.Name)
	}
	if info.GetSizeBytes() == 0 || info.GetSizeBytes() > maxEtcdSnapshotBytes {
		return nil, fmt.Errorf("node %s described a %d byte snapshot; the limit is %d bytes", node.Name, info.GetSizeBytes(), maxEtcdSnapshotBytes)
	}
	if digest, err := hex.DecodeString(info.GetSha256()); err != nil || len(digest) != sha256.Size {
		return nil, fmt.Errorf("node %s described the snapshot with an invalid SHA-256 digest", node.Name)
	}
	if progress != nil {
		fmt.Fprintf(progress, "etcd snapshot node=%s status=downloading size=%d\n", node.Name, info.GetSizeBytes())
	}
	if err := receiveEtcdSnapshot(stream, info, output); err != nil {
		return nil, fmt.Errorf("download etcd snapshot from %s: %w", node.Name, err)
	}
	return info, nil
}

func etcdSnapshotStartError(node string, err error) error {
	switch status.Code(err) {
	case codes.Unimplemented:
		return errSnapshotUnavailable{fmt.Errorf("node %s agent does not support etcd snapshots; upgrade KatlOS on that node", node)}
	case codes.Canceled, codes.DeadlineExceeded:
		return fmt.Errorf("etcd snapshot on %s: %w", node, err)
	}
	return errSnapshotUnavailable{fmt.Errorf("etcd snapshot on %s: %s", node, status.Convert(err).Message())}
}

// receiveEtcdSnapshot publishes the file only after its size and SHA-256
// match the description the node computed from its verified snapshot.
func receiveEtcdSnapshot(stream grpc.ServerStreamingClient[agentapi.EtcdSnapshotChunk], info *agentapi.EtcdSnapshotInfo, output string) error {
	dir := filepath.Dir(output)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create snapshot directory %s: %w", dir, err)
	}
	temporary, err := os.CreateTemp(dir, ".katl-etcd-snapshot-*")
	if err != nil {
		return fmt.Errorf("create snapshot file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	defer temporary.Close()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure snapshot file: %w", err)
	}

	hash := sha256.New()
	var received uint64
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if chunk.GetInfo() != nil {
			return errors.New("node repeated the snapshot description")
		}
		data := chunk.GetData()
		if uint64(len(data)) > info.GetSizeBytes()-received {
			return fmt.Errorf("node sent more than the described %d bytes", info.GetSizeBytes())
		}
		if _, err := temporary.Write(data); err != nil {
			return fmt.Errorf("write snapshot file: %w", err)
		}
		hash.Write(data)
		received += uint64(len(data))
	}
	if received != info.GetSizeBytes() {
		return fmt.Errorf("received %d of %d snapshot bytes", received, info.GetSizeBytes())
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != info.GetSha256() {
		return fmt.Errorf("received snapshot SHA-256 %s does not match the node's %s", got, info.GetSha256())
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync snapshot file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close snapshot file: %w", err)
	}
	if err := os.Link(temporaryPath, output); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing file %s", output)
		}
		return fmt.Errorf("publish snapshot %s: %w", output, err)
	}
	return nil
}

func requireNewFile(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("refusing to overwrite existing file %s; choose a new --output path", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect --output %s: %w", path, err)
	}
	return nil
}
