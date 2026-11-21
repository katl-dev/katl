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

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
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
