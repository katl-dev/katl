package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const (
	workerCurrentEndpoint = "10.0.0.13:9443"
	workerDesiredEndpoint = "10.0.0.23:9443"
)

// writeMovedWorkerConfig writes a trusted-network cluster whose worker-1
// management.address names the worker's new address.
func writeMovedWorkerConfig(t *testing.T, workerKubernetes string) string {
	t.Helper()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(t.TempDir(), "katlctl.yaml"))
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	source := strings.Replace(configBundleSource(), "managementAuthentication: mtls", "managementAuthentication: trusted-network", 1) + `    - name: worker-1
      management:
        address: 10.0.0.23
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-worker-root
` + workerKubernetes
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func movedWorkerClient(agentStart string) *fakeKatlcAgentClient {
	client := healthyHostClient("machine-worker-1", agentStart, "generation-1")
	client.nodeStatus.InventoryNodeName = "worker-1"
	client.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{State: "ready", Role: "worker", NodeName: "worker-1", KubeletActive: true, NodeReady: true}
	return client
}

func TestClusterApplyStagesWorkerAddressChange(t *testing.T) {
	path := writeMovedWorkerConfig(t, "")
	worker := movedWorkerClient("agent-1")
	worker.validateResult = &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: generation.ApplyModeNextBoot, ChangedDomains: []string{configapply.DomainHostConfiguration}}
	worker.submitAccepted = &agentapi.OperationAccepted{OperationId: "stage-worker-1"}
	worker.operationStatus = &agentapi.OperationStatus{OperationId: "stage-worker-1", Terminal: true, Result: operation.ResultSucceeded}
	var dialed []string
	installKatlcDial(t, func(endpoint string) {
		dialed = append(dialed, endpoint)
		if endpoint != workerCurrentEndpoint {
			t.Fatalf("apply dialed %s; the worker answers only at %s", endpoint, workerCurrentEndpoint)
		}
	}, worker)

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"cluster", "apply", "--config", path, "--node", "worker-1", "--endpoint", "10.0.0.13"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, stderr.String())
	}

	if len(dialed) == 0 {
		t.Fatal("apply did not contact the worker")
	}
	if got := worker.validateRequest.GetApplyMode(); got != generation.ApplyModeNextBoot {
		t.Fatalf("validation apply mode = %q, want next-boot", got)
	}
	var staged int
	for _, request := range worker.submitRequests {
		if request.GetConfigApply() != nil {
			staged++
			if request.GetConfigApply().GetApplyMode() != generation.ApplyModeNextBoot {
				t.Fatalf("config apply mode = %q, want next-boot", request.GetConfigApply().GetApplyMode())
			}
		}
		if request.GetKubeadmControlPlaneConfig() != nil {
			t.Fatal("Kubernetes configuration ran before the worker rebooted at its new address")
		}
	}
	if staged != 1 {
		t.Fatalf("config apply operations = %d, want 1", staged)
	}
	want := "katlctl node reboot worker-1 --config " + path + " --endpoint 10.0.0.13', which waits for it at 10.0.0.23"
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("stdout = %q, want reboot guidance containing %q", stdout.String(), want)
	}
}

func TestClusterApplyRefusesAddressChange(t *testing.T) {
	for _, test := range []struct {
		name             string
		args             []string
		workerKubernetes string
		dials            bool
		want             string
	}{
		{name: "control plane", args: []string{"--node", "cp-1", "--endpoint", "10.0.0.99"}, want: "refusing to move control-plane node cp-1 from 10.0.0.99 to 10.0.0.11"},
		{name: "live", args: []string{"--node", "worker-1", "--endpoint", "10.0.0.13", "--mode", "live"}, want: "requires a reboot"},
		{name: "no node", args: []string{"--endpoint", "10.0.0.13"}, want: "exactly one --node"},
		{name: "two nodes", args: []string{"--node", "worker-1", "--node", "cp-1", "--endpoint", "10.0.0.13"}, want: "exactly one --node"},
		{name: "pinned kubelet", args: []string{"--node", "worker-1", "--endpoint", "10.0.0.13"}, workerKubernetes: "      kubernetes:\n        address: 10.0.0.13\n", dials: true, want: "pins its kubelet node IP to 10.0.0.13"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeMovedWorkerConfig(t, test.workerKubernetes)
			worker := movedWorkerClient("agent-1")
			installKatlcDial(t, func(endpoint string) {
				if !test.dials {
					t.Fatalf("refused change dialed %s", endpoint)
				}
			}, worker)

			err := run(context.Background(), append([]string{"cluster", "apply", "--config", path}, test.args...), &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if worker.validateRequest != nil || len(worker.submitRequests) != 0 {
				t.Fatal("refused change reached node validation or mutation")
			}
		})
	}
}

func TestClusterApplyGuidesUnansweredWorkerAddress(t *testing.T) {
	for _, test := range []struct {
		name       string
		saved      string
		desiredErr error
		answeredAs string
		want       string
		notWant    string
	}{
		{name: "saved previous address", saved: workerCurrentEndpoint, desiredErr: context.DeadlineExceeded, want: "worker-1 is not answering at 10.0.0.23:9443, and the saved context last reached it at 10.0.0.13:9443; if you are changing its address, run 'katlctl cluster apply --config CONFIG --node worker-1 --endpoint 10.0.0.13'"},
		{name: "refused at desired address", saved: workerCurrentEndpoint, desiredErr: grpcstatus.Error(codes.Unavailable, "connection refused"), want: "--node worker-1 --endpoint 10.0.0.13'"},
		{name: "no saved context", desiredErr: context.DeadlineExceeded, want: "run 'katlctl cluster apply --config CONFIG --node worker-1 --endpoint CURRENT_ADDRESS' with the address it answers at now"},
		{name: "saved address is configured", saved: workerDesiredEndpoint, desiredErr: context.DeadlineExceeded, want: "--endpoint CURRENT_ADDRESS", notWant: "saved context last reached"},
		{name: "answers at desired address", answeredAs: "worker-2", want: `answered as enrolled node "worker-2"`, notWant: "--endpoint"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeMovedWorkerConfig(t, "")
			if test.saved != "" {
				saved := workstation.Config{}.UpsertCluster("lab", workstation.Cluster{Name: "lab", Nodes: []workstation.Node{
					{Name: "cp-1", ManagementEndpoint: "10.0.0.11:9443", SystemRole: inventory.RoleControlPlane},
					{Name: "worker-1", ManagementEndpoint: test.saved, SystemRole: inventory.RoleWorker},
				}})
				if err := workstation.Save(os.Getenv("KATLCTL_CONFIG"), saved); err != nil {
					t.Fatal(err)
				}
			}
			worker := movedWorkerClient("agent-1")
			if test.answeredAs != "" {
				worker.nodeStatus.InventoryNodeName = test.answeredAs
			}
			oldDial := dialKatlcAgent
			t.Cleanup(func() { dialKatlcAgent = oldDial })
			dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
				if endpoint == workerDesiredEndpoint && test.desiredErr != nil {
					return katlcAgentConnection{}, test.desiredErr
				}
				return katlcAgentConnection{Client: worker, Close: func() error { return nil }}, nil
			}

			err := run(context.Background(), []string{"cluster", "apply", "--config", path, "--node", "worker-1"}, &bytes.Buffer{}, &bytes.Buffer{})

			want := strings.ReplaceAll(test.want, "CONFIG", path)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want %q", err, want)
			}
			if test.notWant != "" && strings.Contains(err.Error(), test.notWant) {
				t.Fatalf("error = %v, want no %q", err, test.notWant)
			}
			if worker.validateRequest != nil || len(worker.submitRequests) != 0 {
				t.Fatal("unverified worker reached node validation or mutation")
			}
		})
	}
}

func TestHostRebootFollowsAddressChange(t *testing.T) {
	for _, test := range []struct {
		name string
		// answer returns the client answering at endpoint after the reboot.
		answer     func(endpoint string) *fakeKatlcAgentClient
		wantStdout string
		wantError  string
	}{
		{
			name: "new address",
			answer: func(endpoint string) *fakeKatlcAgentClient {
				if endpoint == workerDesiredEndpoint {
					return movedWorkerClient("agent-2")
				}
				return nil
			},
			wantStdout: "worker-1 rebooted successfully at 10.0.0.23; health OK\n",
		},
		{
			name: "previous address",
			answer: func(endpoint string) *fakeKatlcAgentClient {
				if endpoint == workerCurrentEndpoint {
					return movedWorkerClient("agent-2")
				}
				return nil
			},
			wantError: "still answers at its previous address 10.0.0.13",
		},
		{
			name: "other installation",
			answer: func(endpoint string) *fakeKatlcAgentClient {
				if endpoint == workerDesiredEndpoint {
					other := movedWorkerClient("agent-9")
					other.nodeStatus.MachineId = "machine-other"
					return other
				}
				return nil
			},
			wantError: "address 10.0.0.23:9443 answered as a different KatlOS installation",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeMovedWorkerConfig(t, "")
			before := movedWorkerClient("agent-1")
			rebooted := false
			before.onReboot = func(*agentapi.RebootRequest) { rebooted = true }
			oldDial := dialKatlcAgent
			t.Cleanup(func() { dialKatlcAgent = oldDial })
			dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
				client := before
				if rebooted {
					client = test.answer(endpoint)
				} else if endpoint != workerCurrentEndpoint {
					return katlcAgentConnection{}, errors.New("no route to host")
				}
				if client == nil {
					return katlcAgentConnection{}, errors.New("no route to host")
				}
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}
			previousInterval := upgradeRebootPollInterval
			upgradeRebootPollInterval = time.Millisecond
			t.Cleanup(func() { upgradeRebootPollInterval = previousInterval })

			var stdout, stderr bytes.Buffer
			err := run(context.Background(), []string{"node", "reboot", "worker-1", "--config", path, "--endpoint", "10.0.0.13", "--timeout", "1s"}, &stdout, &stderr)

			if len(before.rebootRequests) != 1 {
				t.Fatalf("reboot requests at the current address = %d, want 1", len(before.rebootRequests))
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("reboot: %v\n%s", err, stderr.String())
			}
			if got := stdout.String(); got != test.wantStdout {
				t.Fatalf("stdout = %q, want %q", got, test.wantStdout)
			}
		})
	}
}

// TestAddressChangeRetryAfterMove covers rerunning a re-address step at the
// previous address once the worker answers only at its management.address.
func TestAddressChangeRetryAfterMove(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		// mtls selects a configuration whose nodes are reached through the
		// saved context instead of being observed at management.address.
		mtls bool
	}{
		{name: "apply", args: []string{"cluster", "apply", "--node", "worker-1", "--endpoint", "10.0.0.13"}},
		{name: "apply with saved context", args: []string{"cluster", "apply", "--node", "worker-1", "--endpoint", "10.0.0.13"}, mtls: true},
		{name: "reboot", args: []string{"node", "reboot", "worker-1", "--endpoint", "10.0.0.13", "--timeout", "1s"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeMovedWorkerConfig(t, "")
			if test.mtls {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(data), "managementAuthentication: trusted-network", "managementAuthentication: mtls", 1)), 0o600); err != nil {
					t.Fatal(err)
				}
				saved := workstation.Config{}.UpsertCluster("lab", workstation.Cluster{Name: "lab", Nodes: []workstation.Node{
					{Name: "cp-1", ManagementEndpoint: "10.0.0.11:9443", SystemRole: inventory.RoleControlPlane},
					{Name: "worker-1", ManagementEndpoint: workerCurrentEndpoint, SystemRole: inventory.RoleWorker, EnrollmentID: "enrollment-worker-1", MachineID: "machine-worker-1"},
				}})
				if err := workstation.Save(os.Getenv("KATLCTL_CONFIG"), saved); err != nil {
					t.Fatal(err)
				}
			}
			moved := movedWorkerClient("agent-2")
			gone := movedWorkerClient("agent-1")
			gone.nodeStatusErr = grpcstatus.Error(codes.Unavailable, "dial tcp 10.0.0.13:9443: connect: no route to host")
			oldDial := dialKatlcAgent
			t.Cleanup(func() { dialKatlcAgent = oldDial })
			// Like gRPC, the dial succeeds and the first request fails.
			dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
				client := moved
				if endpoint == workerCurrentEndpoint {
					client = gone
				}
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}

			err := run(context.Background(), append(test.args, "--config", path), &bytes.Buffer{}, &bytes.Buffer{})

			want := "if worker-1 already moved to its management.address 10.0.0.23, finish with 'katlctl cluster apply --config " + path + " --node worker-1' without --endpoint"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want %q", err, want)
			}
			if len(moved.submitRequests) != 0 || len(moved.rebootRequests) != 0 || len(gone.rebootRequests) != 0 {
				t.Fatal("a retry at the previous address changed a node")
			}
		})
	}
}
