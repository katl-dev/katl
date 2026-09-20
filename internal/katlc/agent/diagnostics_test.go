package agent

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func diagnosticClient(t *testing.T, root string) agentapi.KatlcAgentClient {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentapi.RegisterKatlcAgentServer(server, &Server{Root: root})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentapi.NewKatlcAgentClient(conn)
}

func TestKubeconfigRead(t *testing.T) {
	root := t.TempDir()
	client := diagnosticClient(t, root)
	if _, err := client.GetKubeconfig(context.Background(), &agentapi.GetKubeconfigRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing config: %v", err)
	}
	path := filepath.Join(root, "etc/kubernetes/admin.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, data string
		code       codes.Code
	}{
		{"available", "admin credentials", codes.OK},
		{"empty", " \n", codes.FailedPrecondition},
		{"oversize", strings.Repeat("x", (1<<20)+1), codes.FailedPrecondition},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := client.GetKubeconfig(context.Background(), &agentapi.GetKubeconfigRequest{})
			if status.Code(err) != test.code {
				t.Fatalf("%v", err)
			}
			if err == nil && string(got.Kubeconfig) != test.data {
				t.Fatal("incorrect content")
			}
		})
	}
}

func TestJournalStream(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	t.Setenv("JOURNAL_TEST_ARGS", capture)
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$JOURNAL_TEST_ARGS\"\nprintf '%s\\n' '{\"MESSAGE\":\"one\"}' '{\"MESSAGE\":\"two\"}'\n"
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	client := diagnosticClient(t, t.TempDir())
	stream, err := client.ReadJournal(context.Background(), &agentapi.JournalRequest{Units: []string{"kubelet", "katl-*"}, Lines: 17, Boot: "-1", Since: "1 hour ago", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`{"MESSAGE":"one"}`, `{"MESSAGE":"two"}`} {
		got, err := stream.Recv()
		if err != nil || got.GetLine() != want {
			t.Fatalf("got %v, %v", got, err)
		}
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := "--no-pager\n--quiet\n--output=json\n--lines=17\n--boot=-1\n--unit=kubelet\n--unit=katl-*\n--since=1 hour ago\n"
	if string(args) != want {
		t.Fatalf("journal filters: %s", args)
	}
}

func TestJournalFailures(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "journalctl")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	client := diagnosticClient(t, t.TempDir())
	for _, request := range []*agentapi.JournalRequest{{Lines: -1}, {Lines: 10001}, {Format: "invalid"}, {Boot: "bad"}, {Units: []string{"x\ny"}}} {
		stream, err := client.ReadJournal(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("validation: %v", err)
		}
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'invalid time filter' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	stream, err := client.ReadJournal(context.Background(), &agentapi.JournalRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "invalid time filter") {
		t.Fatalf("native diagnostic: %v", err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	stream, err = client.ReadJournal(ctx, &agentapi.JournalRequest{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("follow deadline: %v", err)
	}
}
