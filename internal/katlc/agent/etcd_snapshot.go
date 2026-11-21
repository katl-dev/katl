package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// The export directory holds at most the snapshot of the one export in
	// progress. It is under /var/lib/etcd because the etcd container writes the
	// file, and it is separate from upgrade snapshots so retention never sees it.
	etcdSnapshotExportDirectory = "/var/lib/etcd/katl-snapshot-export"
	// etcd recommends a backend quota of at most 8 GiB.
	maxEtcdSnapshotSize     = uint64(8 << 30)
	etcdSnapshotSaveTimeout = 10 * time.Minute
	etcdSnapshotChunkSize   = 1 << 20
)

func (s *Server) SaveEtcdSnapshot(_ *agentapi.SaveEtcdSnapshotRequest, stream grpc.ServerStreamingServer[agentapi.EtcdSnapshotChunk]) error {
	if !s.etcdSnapshotMu.TryLock() {
		return status.Error(codes.Aborted, "another etcd snapshot is in progress on this node; retry after it finishes")
	}
	defer s.etcdSnapshotMu.Unlock()
	ctx := stream.Context()
	run := s.etcdRunner()

	report, err := collectEtcdStatus(ctx, run)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "inspect local stacked etcd: %v", err)
	}
	if local := findEtcdMember(report, report.GetLocalMemberId()); local == nil || !local.GetHealthy() {
		return status.Error(codes.FailedPrecondition, "local etcd member is not healthy; take the snapshot on another control plane")
	}
	if report.GetHealthyMembers() < report.GetQuorum() {
		return status.Errorf(codes.FailedPrecondition, "etcd has %d healthy members, below quorum %d; restore quorum before taking a snapshot", report.GetHealthyMembers(), report.GetQuorum())
	}
	containerID, err := runningEtcdContainer(ctx, run)
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	databaseSize, err := localEtcdDatabaseSize(ctx, run, containerID)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read local etcd database size: %v", err)
	}
	if databaseSize > maxEtcdSnapshotSize {
		return status.Errorf(codes.ResourceExhausted, "etcd database is %d bytes, above the %d byte snapshot limit; compact and defragment etcd first", databaseSize, maxEtcdSnapshotSize)
	}

	// The directory is cleared under etcdSnapshotMu, which also removes a file
	// left by an export the agent did not finish.
	directory := rootedRuntimePath(s.Root, etcdSnapshotExportDirectory)
	if err := os.RemoveAll(directory); err != nil {
		return status.Errorf(codes.Internal, "clear etcd snapshot export directory: %v", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return status.Errorf(codes.Internal, "prepare etcd snapshot export directory: %v", err)
	}
	defer os.RemoveAll(directory)
	// The snapshot shares a filesystem with the etcd data directory, which
	// must not run out of space.
	if err := requireHostUpgradeStorage(s.AvailableStorage, directory, databaseSize, "etcd snapshot"); err != nil {
		return status.Error(codes.ResourceExhausted, err.Error())
	}

	name, err := randomID("snapshot")
	if err != nil {
		return status.Errorf(codes.Internal, "name etcd snapshot: %v", err)
	}
	location := path.Join(etcdSnapshotExportDirectory, name+".db")
	file := rootedRuntimePath(s.Root, location)
	saveCtx, cancel := context.WithTimeout(ctx, etcdSnapshotSaveTimeout)
	_, err = runEtcdctl(saveCtx, run, containerID, "https://127.0.0.1:2379", "snapshot", "save", location)
	cancel()
	if err != nil {
		return status.Errorf(codes.Unavailable, "save etcd snapshot: %v", err)
	}
	if err := verifyEtcdSnapshot(file); err != nil {
		return status.Errorf(codes.DataLoss, "verify etcd snapshot: %v", err)
	}
	snapshotStatus, err := readEtcdSnapshotStatus(ctx, run, containerID, location)
	if err != nil {
		return status.Errorf(codes.DataLoss, "%v", err)
	}
	digest, size, err := fileDigest(file)
	if err != nil {
		return status.Errorf(codes.Internal, "digest etcd snapshot: %v", err)
	}
	if uint64(size) > maxEtcdSnapshotSize {
		return status.Errorf(codes.ResourceExhausted, "etcd snapshot is %d bytes, above the %d byte limit", size, maxEtcdSnapshotSize)
	}

	snapshot, err := os.Open(file)
	if err != nil {
		return status.Errorf(codes.Internal, "open etcd snapshot: %v", err)
	}
	defer snapshot.Close()
	if err := stream.Send(&agentapi.EtcdSnapshotChunk{Info: &agentapi.EtcdSnapshotInfo{
		ClusterId: report.GetClusterId(),
		MemberId:  report.GetLocalMemberId(),
		Revision:  snapshotStatus.Revision,
		TotalKeys: snapshotStatus.TotalKeys,
		Hash:      snapshotStatus.Hash,
		Sha256:    digest,
		SizeBytes: uint64(size),
	}}); err != nil {
		return err
	}
	buffer := make([]byte, etcdSnapshotChunkSize)
	for {
		n, err := snapshot.Read(buffer)
		if n > 0 {
			if sendErr := stream.Send(&agentapi.EtcdSnapshotChunk{Data: buffer[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Internal, "read etcd snapshot: %v", err)
		}
	}
}

func localEtcdDatabaseSize(ctx context.Context, run ToolRunner, containerID string) (uint64, error) {
	result, err := runEtcdctl(ctx, run, containerID, "https://127.0.0.1:2379", "endpoint", "status", "--write-out=json")
	if err != nil {
		return 0, err
	}
	var statuses []struct {
		Status struct {
			DBSize *int64 `json:"dbSize"`
		} `json:"Status"`
	}
	if err := json.Unmarshal(result.Stdout, &statuses); err != nil || len(statuses) != 1 || statuses[0].Status.DBSize == nil || *statuses[0].Status.DBSize < 0 {
		return 0, errors.New("decode local endpoint status")
	}
	return uint64(*statuses[0].Status.DBSize), nil
}
