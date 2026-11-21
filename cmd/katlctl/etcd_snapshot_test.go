package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type snapshotAgentClient struct {
	*fakeKatlcAgentClient
	chunks   []*agentapi.EtcdSnapshotChunk
	err      error
	requests int
}

func (c *snapshotAgentClient) SaveEtcdSnapshot(context.Context, *agentapi.SaveEtcdSnapshotRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentapi.EtcdSnapshotChunk], error) {
	c.requests++
	return &snapshotClientStream{chunks: c.chunks, err: c.err}, nil
}

type snapshotClientStream struct {
	grpc.ClientStream
	chunks []*agentapi.EtcdSnapshotChunk
	err    error
}

func (s *snapshotClientStream) Recv() (*agentapi.EtcdSnapshotChunk, error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	chunk := s.chunks[0]
	s.chunks = s.chunks[1:]
	return chunk, nil
}

type snapshotConnector map[string]*snapshotAgentClient

func (c snapshotConnector) Connect(_ context.Context, node inventory.PlannedNode) (cluster.AgentConnection, error) {
	client := c[node.Name]
	if client == nil {
		return cluster.AgentConnection{}, errors.New("unreachable")
	}
	client.nodeStatus.EnrollmentId = node.EnrollmentID
	client.nodeStatus.InventoryNodeName = node.Name
	return cluster.AgentConnection{Endpoint: node.Address + ":9443", Client: client, Close: func() error { return nil }}, nil
}

// snapshotChunks describes contents as digest claims and splits it into
// two data messages.
func snapshotChunks(contents []byte, digest string) []*agentapi.EtcdSnapshotChunk {
	info := &agentapi.EtcdSnapshotInfo{ClusterId: "64", MemberId: "2", Revision: 42, TotalKeys: 7, Sha256: digest, SizeBytes: uint64(len(contents))}
	half := len(contents) / 2
	return []*agentapi.EtcdSnapshotChunk{{Info: info}, {Data: contents[:half]}, {Data: contents[half:]}}
}

func writeSnapshotClusterConfig(t *testing.T) string {
	t.Helper()
	config, _ := writeKubernetesUpgradeConfig(t, "v1.36.1",
		workstation.Node{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine-cp-1"},
		workstation.Node{Name: "cp-2", ManagementEndpoint: "192.0.2.2:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-2", MachineID: "machine-cp-2"},
		workstation.Node{Name: "worker-1", ManagementEndpoint: "192.0.2.3:9443", SystemRole: inventory.RoleWorker, EnrollmentID: "enrollment-worker-1", MachineID: "machine-worker-1"},
	)
	return config
}

func useSnapshotConnector(t *testing.T, connector snapshotConnector) {
	t.Helper()
	endpoints := map[string]*snapshotAgentClient{}
	for i, name := range []string{"cp-1", "cp-2"} {
		client := connector[name]
		client.nodeStatus.EnrollmentId = "enrollment-" + name
		client.nodeStatus.InventoryNodeName = name
		endpoints[fmt.Sprintf("192.0.2.%d:9443", i+1)] = client
	}
	oldConnector := newWipeClusterConnector
	oldDial := dialKatlcAgent
	newWipeClusterConnector = func() cluster.AgentConnector { return connector }
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if client := endpoints[endpoint]; client != nil {
			return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
		}
		return katlcAgentConnection{}, errors.New("unreachable")
	}
	t.Cleanup(func() { newWipeClusterConnector = oldConnector; dialKatlcAgent = oldDial })
}

func TestEtcdSnapshotSavesFromFirstHealthyControlPlane(t *testing.T) {
	contents := []byte("etcd snapshot database contents")
	sum := sha256.Sum256(contents)
	digest := hex.EncodeToString(sum[:])
	cp1 := &snapshotAgentClient{fakeKatlcAgentClient: readyWipeClusterClient("machine-cp-1"), err: status.Error(codes.FailedPrecondition, "local etcd member is not healthy")}
	cp2 := &snapshotAgentClient{fakeKatlcAgentClient: readyWipeClusterClient("machine-cp-2"), chunks: snapshotChunks(contents, digest)}
	useSnapshotConnector(t, snapshotConnector{"cp-1": cp1, "cp-2": cp2})
	output := filepath.Join(t.TempDir(), "backups", "etcd.db")

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "etcd", "snapshot", "--config", writeSnapshotClusterConfig(t), "--output", output}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}

	saved, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(saved, contents) {
		t.Fatalf("saved snapshot = %q, %v", saved, err)
	}
	if info, err := os.Stat(output); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %v, %v", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(output)); len(entries) != 1 {
		t.Fatalf("snapshot directory holds %d entries, want only the snapshot", len(entries))
	}
	if want := "etcd snapshot saved path=" + output + " node=cp-2 cluster=64 member=2 revision=42 keys=7 size=31 sha256=" + digest + "\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "node=cp-1 status=skipped") || !strings.Contains(stderr.String(), "local etcd member is not healthy") {
		t.Fatalf("stderr does not explain the skipped node:\n%s", stderr.String())
	}
}

func TestEtcdSnapshotRejects(t *testing.T) {
	contents := []byte("etcd snapshot database contents")
	sum := sha256.Sum256(contents)
	digest := hex.EncodeToString(sum[:])
	for _, tc := range []struct {
		name     string
		args     []string
		existing bool
		cp1      *snapshotAgentClient
		want     string
	}{
		{
			name: "digest mismatch",
			cp1:  &snapshotAgentClient{chunks: snapshotChunks(append([]byte("x"), contents[1:]...), digest)},
			want: "does not match",
		},
		{
			name: "truncated transfer",
			cp1:  &snapshotAgentClient{chunks: snapshotChunks(contents, digest)[:2]},
			want: "received 15 of 31 snapshot bytes",
		},
		{
			name: "selected node unhealthy",
			args: []string{"--node", "cp-1"},
			cp1:  &snapshotAgentClient{err: status.Error(codes.FailedPrecondition, "local etcd member is not healthy")},
			want: "local etcd member is not healthy",
		},
		{
			name: "old agent",
			args: []string{"--node", "cp-1"},
			cp1:  &snapshotAgentClient{err: status.Error(codes.Unimplemented, "unknown method")},
			want: "upgrade KatlOS on that node",
		},
		{
			name:     "existing output",
			existing: true,
			cp1:      &snapshotAgentClient{chunks: snapshotChunks(contents, digest)},
			want:     "refusing to overwrite",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cp1.fakeKatlcAgentClient = readyWipeClusterClient("machine-cp-1")
			cp2 := &snapshotAgentClient{fakeKatlcAgentClient: readyWipeClusterClient("machine-cp-2"), chunks: snapshotChunks(contents, digest)}
			useSnapshotConnector(t, snapshotConnector{"cp-1": tc.cp1, "cp-2": cp2})
			dir := t.TempDir()
			output := filepath.Join(dir, "etcd.db")
			if tc.existing {
				if err := os.WriteFile(output, []byte("earlier backup"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			args := append([]string{"cluster", "etcd", "snapshot", "--config", writeSnapshotClusterConfig(t), "--output", output}, tc.args...)
			err := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{})

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run() error = %v, want %q", err, tc.want)
			}
			if cp2.requests != 0 {
				t.Fatal("snapshot fell through to another control plane")
			}
			entries, _ := os.ReadDir(dir)
			if tc.existing {
				if data, _ := os.ReadFile(output); string(data) != "earlier backup" || len(entries) != 1 {
					t.Fatalf("existing output changed: %q, %d entries", data, len(entries))
				}
				return
			}
			if len(entries) != 0 {
				t.Fatalf("rejected snapshot left %d files", len(entries))
			}
		})
	}
}
