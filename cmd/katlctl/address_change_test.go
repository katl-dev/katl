package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"google.golang.org/grpc"
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
		{name: "live", args: []string{"--node", "worker-1", "--endpoint", "10.0.0.13", "--mode", "live"}, dials: true, want: "moving worker-1 from 10.0.0.13 to 10.0.0.23 requires a reboot"},
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
		{name: "answers at desired address", answeredAs: "worker-2", want: `the endpoint answered as node "worker-2", not worker-1; check that 10.0.0.23:9443 is worker-1's management address`, notWant: "--endpoint"},
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

// TestClusterApplyChecksEndpointBeforeMove covers cluster apply --endpoint on
// a cluster whose cp-1, worker-1 and worker-2 answer at 192.0.2.11, .12 and
// .13. The address is checked before any move is decided, so a wrong address
// is told to check the address, and only a node that answered as itself on
// another host is refused a move.
func TestClusterApplyChecksEndpointBeforeMove(t *testing.T) {
	const down = ""
	tests := map[string]struct {
		args []string
		// routes sends an extra endpoint to the named node, or to a stopped
		// listener when the name is down.
		routes map[string]string
		// bundle reaches nodes through the saved context: mTLS without a
		// secrets reference.
		bundle bool
		want   []string
		refuse []string
		// moveRefused requires that the refusal followed a status read at
		// this endpoint and nothing else.
		moveRefused string
		// proceeds requires that apply reached the node's config validation.
		proceeds bool
	}{
		"control plane, nothing answers": {
			args:   []string{"--node", "cp-1", "--endpoint", "192.0.2.99", "--timeout", "10s"},
			routes: map[string]string{"192.0.2.99:9443": down},
			want:   []string{"did not answer at 192.0.2.99:9443", "check that 192.0.2.99:9443 is the node's management address, then rerun the same command"},
			refuse: []string{"refusing to move", "node wipe", "restore management.address", "omit --endpoint"},
		},
		"closed port on the configured host": {
			args:   []string{"--node", "cp-1", "--endpoint", "192.0.2.11:9999", "--timeout", "10s"},
			routes: map[string]string{"192.0.2.11:9999": down},
			want:   []string{"did not answer at 192.0.2.11:9999", "check that 192.0.2.11:9999 is the node's management address"},
			refuse: []string{"refusing to move", "node wipe", "from 192.0.2.11 to 192.0.2.11"},
		},
		"control plane answers on another host": {
			args:        []string{"--node", "cp-1", "--endpoint", "192.0.2.99"},
			routes:      map[string]string{"192.0.2.99:9443": "cp-1"},
			want:        []string{"refusing to move control-plane node cp-1 from 192.0.2.99 to 192.0.2.11: cp-1 answers at 192.0.2.99", "plan 'katlctl node wipe cp-1"},
			moveRefused: "192.0.2.99:9443",
		},
		"another node answers": {
			args:   []string{"--node", "cp-1", "--endpoint", "192.0.2.13"},
			want:   []string{`the endpoint answered as node "worker-2", not cp-1; check that 192.0.2.13:9443 is cp-1's management address, then rerun the same command`},
			refuse: []string{"refusing to move", "node wipe"},
		},
		"live mode, nothing answers": {
			args:   []string{"--node", "worker-1", "--endpoint", "192.0.2.99", "--mode", "live", "--timeout", "10s"},
			routes: map[string]string{"192.0.2.99:9443": down},
			want:   []string{"check that 192.0.2.99:9443 is the node's management address"},
			refuse: []string{"requires a reboot"},
		},
		"saved context, nothing answers": {
			args:   []string{"--node", "cp-1", "--endpoint", "192.0.2.99", "--timeout", "10s"},
			routes: map[string]string{"192.0.2.99:9443": down},
			bundle: true,
			want: []string{
				"did not answer at 192.0.2.99:9443",
				"check that 192.0.2.99:9443 is the node's management address, then rerun the same command; the saved context and CONFIG both place cp-1 at 192.0.2.11, so omit --endpoint to use it",
			},
			refuse: []string{"refusing to move", "node wipe"},
		},
		"forwarded port on the configured host": {
			args:     []string{"--node", "cp-1", "--endpoint", "192.0.2.11:9999", "--plan"},
			routes:   map[string]string{"192.0.2.11:9999": "cp-1"},
			refuse:   []string{"refusing to move", "from 192.0.2.11 to 192.0.2.11"},
			proceeds: true,
		},
		"configured address": {
			args:     []string{"--node", "cp-1", "--endpoint", "192.0.2.11:9443", "--plan"},
			refuse:   []string{"refusing to move"},
			proceeds: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			testEnableMTLSWithVirtualTime(t, []string{"cp-1", "worker-1", "worker-2"}, func(t *testing.T, world enableMTLSWorld) {
				if test.bundle {
					if _, stderr, err := world.enable(); err != nil {
						t.Fatalf("enable-mtls: %v\n%s", err, stderr)
					}
					if err := run(t.Context(), []string{"context", "save", "--config", world.configPath}, io.Discard, io.Discard); err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(world.configPath)
					if err != nil {
						t.Fatal(err)
					}
					var kept []string
					for _, line := range strings.Split(string(data), "\n") {
						if !strings.Contains(line, "managementIdentity:") {
							kept = append(kept, line)
						}
					}
					if err := os.WriteFile(world.configPath, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				listeners := map[string]*trialNode{}
				byName := map[string]*trialNode{}
				for i, node := range world.nodes {
					listeners[fmt.Sprintf("192.0.2.%d:9443", i+11)] = node
					byName[node.name] = node
				}
				for endpoint, name := range test.routes {
					if name != down {
						listeners[endpoint] = byName[name]
						continue
					}
					stopped := newTrialNode(t, "stopped")
					stopped.mu.Lock()
					stopped.server.Stop()
					stopped.mu.Unlock()
					listeners[endpoint] = stopped
				}
				var mu sync.Mutex
				var calls []string
				for _, node := range world.nodes {
					node.interceptRPC = func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
						mu.Lock()
						calls = append(calls, cc.Target()+" "+method)
						mu.Unlock()
						return invoker(ctx, method, req, reply, cc, opts...)
					}
				}
				routeManagement(listeners)

				err := run(t.Context(), append([]string{"cluster", "apply", "--config", world.configPath}, test.args...), io.Discard, io.Discard)

				if err == nil && !test.proceeds {
					t.Fatal("apply accepted the endpoint")
				}
				for _, want := range test.want {
					if want = strings.ReplaceAll(want, "CONFIG", world.configPath); err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("error = %v, want %q", err, want)
					}
				}
				for _, refuse := range test.refuse {
					if err != nil && strings.Contains(err.Error(), refuse) {
						t.Errorf("error = %v, contains %q", err, refuse)
					}
				}
				mu.Lock()
				defer mu.Unlock()
				if test.moveRefused != "" {
					if want := []string{"passthrough:///" + test.moveRefused + " " + agentapi.KatlcAgent_GetNodeStatus_FullMethodName}; !slices.Equal(calls, want) {
						t.Errorf("calls = %q, want only the status read %q before the refusal", calls, want)
					}
				}
				if test.proceeds && !slices.ContainsFunc(calls, func(call string) bool { return strings.HasSuffix(call, "/ValidateConfig") }) {
					t.Errorf("calls = %q, error = %v; want apply to reach config validation", calls, err)
				}
				if !test.proceeds && slices.ContainsFunc(calls, func(call string) bool {
					return strings.HasSuffix(call, "/ValidateConfig") || strings.HasSuffix(call, "/SubmitOperation")
				}) {
					t.Errorf("calls = %q; a refused apply reached validation or mutation", calls)
				}
			})
		})
	}
}
