package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type snapshotStream struct {
	grpc.ServerStream
	chunks []*agentapi.EtcdSnapshotChunk
}

func (s *snapshotStream) Context() context.Context { return context.Background() }

func (s *snapshotStream) Send(chunk *agentapi.EtcdSnapshotChunk) error {
	s.chunks = append(s.chunks, &agentapi.EtcdSnapshotChunk{Info: chunk.Info, Data: bytes.Clone(chunk.Data)})
	return nil
}

// snapshotEtcdRunner extends the healthy three-member fake with a local
// database size, a snapshot save that writes contents beneath root, and
// etcdutl status. It records whether a snapshot was saved.
func snapshotEtcdRunner(root string, contents []byte, localHealthy bool, saved *bool) ToolRunner {
	members := fakeEtcdRunner(false)
	return func(ctx context.Context, argv []string, progress func(int)) ToolResult {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "127.0.0.1:2379") && strings.HasSuffix(joined, "endpoint status --write-out=json"):
			return ToolResult{Stdout: []byte(`[{"Endpoint":"https://127.0.0.1:2379","Status":{"header":{"cluster_id":100,"member_id":1},"leader":1,"dbSize":4096}}]`)}
		case strings.Contains(joined, "10.0.0.1:2379") && strings.Contains(joined, "endpoint health") && !localHealthy:
			return ToolResult{Stdout: []byte(`[{"health":false,"error":"stopped"}]`)}
		case strings.Contains(joined, "snapshot save "):
			*saved = true
			path := argv[len(argv)-1]
			if !strings.HasPrefix(path, "/var/lib/etcd/") {
				return ToolResult{Err: os.ErrPermission, ExitStatus: 1, Stderr: []byte("not visible to the etcd container")}
			}
			if err := os.WriteFile(filepath.Join(root, path), contents, 0o644); err != nil {
				return ToolResult{Err: err, ExitStatus: 1}
			}
			return ToolResult{}
		case strings.Contains(joined, "etcdutl snapshot status"):
			return ToolResult{Stdout: []byte(`{"hash":3735928559,"revision":42,"totalKey":7,"totalSize":4096}`)}
		default:
			return members(ctx, argv, progress)
		}
	}
}

func etcdFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(root, "var/lib/etcd"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestSaveEtcdSnapshot(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "var/lib/etcd/katl-snapshots/kubeadm-upgrade-1.db"), "upgrade snapshot")
	writeTestFile(t, filepath.Join(root, "var/lib/etcd/katl-snapshot-export/snapshot-stale.db"), "interrupted export")
	// Larger than one chunk, so the stream must carry several data messages.
	contents := make([]byte, 512*4096)
	for i := range contents {
		contents[i] = byte(i % 251)
	}
	sum := sha256.Sum256(contents)
	contents = append(contents, sum[:]...)
	var saved bool
	server := &Server{Root: root, RunEtcd: snapshotEtcdRunner(root, contents, true, &saved), AvailableStorage: func(string) (uint64, error) { return 1 << 40, nil }}

	stream := &snapshotStream{}
	if err := server.SaveEtcdSnapshot(&agentapi.SaveEtcdSnapshotRequest{}, stream); err != nil {
		t.Fatal(err)
	}

	if len(stream.chunks) < 2 || stream.chunks[0].GetInfo() == nil || len(stream.chunks[0].GetData()) != 0 {
		t.Fatalf("first message must carry only snapshot info: %d chunks", len(stream.chunks))
	}
	var received []byte
	for _, chunk := range stream.chunks[1:] {
		if chunk.GetInfo() != nil {
			t.Fatal("snapshot info repeated after the first message")
		}
		received = append(received, chunk.GetData()...)
	}
	if !bytes.Equal(received, contents) {
		t.Fatalf("received %d bytes, want the %d saved bytes", len(received), len(contents))
	}
	info := stream.chunks[0].GetInfo()
	digest := sha256.Sum256(contents)
	if info.GetSha256() != hex.EncodeToString(digest[:]) || info.GetSizeBytes() != uint64(len(contents)) || info.GetRevision() != 42 || info.GetTotalKeys() != 7 || info.GetClusterId() != "64" || info.GetMemberId() != "1" {
		t.Fatalf("snapshot info = %+v", info)
	}
	if files := etcdFiles(t, root); !slices.Equal(files, []string{"var/lib/etcd/katl-snapshots/kubeadm-upgrade-1.db"}) {
		t.Fatalf("node-local etcd files after export = %v", files)
	}
}

func TestSaveEtcdSnapshotRefuses(t *testing.T) {
	valid := testEtcdSnapshot()
	corrupt := bytes.Clone(valid)
	corrupt[0] ^= 0xff
	for _, tc := range []struct {
		name         string
		contents     []byte
		localHealthy bool
		available    uint64
		code         codes.Code
		wantSaved    bool
	}{
		{name: "unhealthy local member", contents: valid, available: 1 << 40, code: codes.FailedPrecondition},
		{name: "insufficient storage", contents: valid, localHealthy: true, available: 4096, code: codes.ResourceExhausted},
		{name: "corrupt snapshot", contents: corrupt, localHealthy: true, available: 1 << 40, code: codes.DataLoss, wantSaved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "var/lib/etcd"), 0o700); err != nil {
				t.Fatal(err)
			}
			var saved bool
			server := &Server{Root: root, RunEtcd: snapshotEtcdRunner(root, tc.contents, tc.localHealthy, &saved), AvailableStorage: func(string) (uint64, error) { return tc.available, nil }}

			stream := &snapshotStream{}
			err := server.SaveEtcdSnapshot(&agentapi.SaveEtcdSnapshotRequest{}, stream)

			if status.Code(err) != tc.code {
				t.Fatalf("SaveEtcdSnapshot() error = %v, want code %s", err, tc.code)
			}
			if saved != tc.wantSaved {
				t.Fatalf("snapshot saved = %t, want %t", saved, tc.wantSaved)
			}
			if len(stream.chunks) != 0 {
				t.Fatalf("refused snapshot streamed %d messages", len(stream.chunks))
			}
			if files := etcdFiles(t, root); len(files) != 0 {
				t.Fatalf("refused snapshot left node-local files %v", files)
			}
		})
	}
}
