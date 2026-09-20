package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/yaml.v3"
)

func TestManagementJourney(t *testing.T) {
	for _, mode := range []string{"trusted-network", "mtls"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			contextPath := filepath.Join(dir, "workstation-a", "context.yaml")
			t.Setenv("KATLCTL_CONFIG", contextPath)
			key := filepath.Join(dir, "ssh.pub")
			if err := os.WriteFile(key, []byte(uxTestSSHKey), 0o600); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(dir, "cluster.yaml")
			args := []string{"config", "init", config, "--name", "lab", "--ssh-authorized-key", key, "--node", "cp-1=control-plane,127.0.0.1,/dev/disk/by-id/test"}
			if mode == "mtls" {
				args = append(args, "--management-authentication", mode)
			}
			for _, invocation := range [][]string{args, append(append([]string{}, args...), "--force")} {
				if err := run(context.Background(), invocation, io.Discard, io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			source, err := configbundle.DecodeSource(bytes.NewReader(data))
			if err != nil || string(source.ManagementAuthentication()) != mode {
				t.Fatalf("source mode: %v, %v", source.Spec.ManagementAuthentication, err)
			}
			source.Spec.Nodes[0].Kubernetes.Address = "192.0.2.1"
			data, err = yaml.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "trusted-network" {
				if source.Spec.ManagementIdentity != "" {
					t.Fatal("default config references secrets")
				}
				if _, err := os.Stat(filepath.Join(dir, defaultManagementSecrets)); !os.IsNotExist(err) {
					t.Fatalf("default created secrets: %v", err)
				}
			}
			var serverOptions []grpc.ServerOption
			if mode == "mtls" {
				identity, err := managementIdentityForSource(config, source)
				if err != nil {
					t.Fatal(err)
				}
				node, err := managementidentity.IssueNode(identity, "cp-1", time.Now(), nil)
				if err != nil {
					t.Fatal(err)
				}
				tlsConfig, err := transport.ServerTLSConfigForNode(node)
				if err != nil {
					t.Fatal(err)
				}
				serverOptions = append(serverOptions, grpc.Creds(credentials.NewTLS(tlsConfig)))
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			fake := readyWipeClusterClient("machine-1")
			fake.nodeStatus.InventoryNodeName = "cp-1"
			fake.nodeStatus.EnrollmentId = "install-1"
			server := grpc.NewServer(serverOptions...)
			agentapi.RegisterKatlcAgentServer(server, &managementJourneyServer{clusterApplyIdentityServer{client: fake}})
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { server.Stop(); listener.Close() })
			oldDial, oldConnector := dialKatlcAgent, newWipeClusterConnector
			t.Cleanup(func() { dialKatlcAgent = oldDial; newWipeClusterConnector = oldConnector })
			dialKatlcAgent = func(ctx context.Context, _ string) (katlcAgentConnection, error) {
				return dialKatlcAgentTCP(ctx, listener.Addr().String())
			}
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			newWipeClusterConnector = func() cluster.AgentConnector {
				connector := managementAgentConnector("")
				connector.DefaultPort = port
				return connector
			}
			bundle := filepath.Join(dir, "cluster.katlcfg")
			if err := run(context.Background(), []string{"config", "bundle", config, "--output", bundle}, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			for _, workstationName := range []string{"workstation-a", "workstation-b"} {
				contextPath = filepath.Join(dir, workstationName, "context.yaml")
				t.Setenv("KATLCTL_CONFIG", contextPath)
				for _, input := range []string{config, bundle} {
					// mTLS bundles deliberately do not carry the operator's private key.
					if mode == "mtls" && input == bundle {
						continue
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					err := run(ctx, []string{"cluster", "wipe", "--config", input, "--all", "--plan"}, io.Discard, io.Discard)
					cancel()
					if err != nil {
						t.Fatalf("%s using %s: %v", workstationName, input, err)
					}
					if _, err := os.Stat(contextPath); !os.IsNotExist(err) {
						t.Fatalf("planning created workstation state: %v", err)
					}
				}
				kubePath := filepath.Join(dir, workstationName, "kubeconfig")
				for i := 0; i < 2; i++ {
					if err := run(context.Background(), []string{"cluster", "kubeconfig", kubePath, "--config", config}, io.Discard, io.Discard); err != nil {
						t.Fatal(err)
					}
				}
				kubeData, err := os.ReadFile(kubePath)
				if err != nil || !bytes.Contains(kubeData, []byte("server: https://127.0.0.1:6443")) {
					t.Fatalf("retrieved kubeconfig endpoint: %s, %v", kubeData, err)
				}
				info, err := os.Stat(kubePath)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("private kubeconfig mode: %v", err)
				}
				if err := os.WriteFile(kubePath, []byte("other cluster"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := run(context.Background(), []string{"cluster", "kubeconfig", kubePath, "--config", config}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--force") {
					t.Fatalf("overwrite error: %v", err)
				}
				var logs bytes.Buffer
				if err := run(context.Background(), []string{"node", "logs", "cp-1", "--config", config, "--unit", "kubelet"}, &logs, io.Discard); err != nil {
					t.Fatal(err)
				}
				if logs.String() != "kubelet ready\n" {
					t.Fatalf("logs %q", &logs)
				}
				if _, err := os.Stat(contextPath); !os.IsNotExist(err) {
					t.Fatalf("read commands created context: %v", err)
				}
			}
			if err := run(context.Background(), []string{"context", "save", "--config", config}, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			saved, err := workstation.Load(contextPath)
			if err != nil {
				t.Fatal(err)
			}
			// Reused addresses in an unrelated current context must not choose trust.
			wrong := saved.Clusters[0]
			wrong.Name = "old-lab"
			other, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "old-lab"})
			if err != nil {
				t.Fatal(err)
			}
			otherClient, err := managementidentity.Client(other, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			wrong.Management = &otherClient
			saved = saved.UpsertCluster("old", wrong)
			if err := workstation.Save(contextPath, saved); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(contextPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := run(context.Background(), []string{"cluster", "wipe", "--config", config, "--all", "--plan"}, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(contextPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("config-based planning modified saved contexts: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := run(ctx, []string{"cluster", "wipe", "--context", "lab", "--all", "--plan"}, io.Discard, io.Discard); err != nil {
				t.Fatalf("selected context wipe: %v", err)
			}
			var members bytes.Buffer
			if err := run(ctx, []string{"cluster", "etcd", "members", "--config", config}, &members, io.Discard); err != nil {
				t.Fatalf("selected cluster etcd inspection: %v", err)
			}
			if !strings.Contains(members.String(), "etcd cluster=123") {
				t.Fatalf("etcd membership: %s", members.String())
			}
			fake.nodeStatus.MachineId = "machine-2"
			fake.nodeStatus.EnrollmentId = "install-2"
			if err := run(ctx, []string{"cluster", "wipe", "--config", config, "--all", "--plan"}, io.Discard, io.Discard); err != nil {
				t.Fatalf("reinstall: %v", err)
			}
			fake.nodeStatus.InventoryNodeName = "wrong-node"
			err = run(ctx, []string{"cluster", "wipe", "--config", config, "--all", "--plan"}, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "wrong-node") {
				t.Fatalf("wrong target accepted: %v", err)
			}
			if len(fake.submitRequests) != 0 {
				t.Fatal("plan submitted destructive operation")
			}
		})
	}
}

type managementJourneyServer struct {
	clusterApplyIdentityServer
}

func (*managementJourneyServer) GetKubeconfig(context.Context, *agentapi.GetKubeconfigRequest) (*agentapi.KubeconfigResponse, error) {
	return &agentapi.KubeconfigResponse{Kubeconfig: []byte("clusters: [{cluster: {certificate-authority-data: Q0E=}}]\nusers: [{user: {client-certificate-data: Q0VSVA==, client-key-data: S0VZ}}]\n")}, nil
}

func (*managementJourneyServer) ReadJournal(request *agentapi.JournalRequest, stream grpc.ServerStreamingServer[agentapi.JournalEntry]) error {
	return stream.Send(&agentapi.JournalEntry{Line: "kubelet ready"})
}

func (*managementJourneyServer) GetEtcdStatus(context.Context, *agentapi.GetEtcdStatusRequest) (*agentapi.EtcdStatus, error) {
	return &agentapi.EtcdStatus{ClusterId: "123", Members: []*agentapi.EtcdMember{{Id: "1", Name: "cp-1"}}}, nil
}
