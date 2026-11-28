package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// foreignNodeCredentials issues serving credentials for name from an authority
// the cluster does not use.
func foreignNodeCredentials(t *testing.T, name string) managementidentity.NodeCredentials {
	t.Helper()
	other, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "other"})
	if err != nil {
		t.Fatal(err)
	}
	credentials, _, err := managementidentity.EnsureNode(&other, name, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

// A context check that changed nothing explains its failure only from what the
// connection observed: a node that took no connection did not answer, and a
// trust clause appears only for typed evidence of another authority.
func TestContextVerificationExplainsEvidence(t *testing.T) {
	const notAnswered = "the node did not answer; retry once 'katlctl node status' shows it up"
	tests := map[string]struct {
		mutualTLS bool
		timeout   bool
		fault     func(*testing.T, *trialNode)
		want      []string
		refuse    []string
		deadline  bool
	}{
		"node stopped": {
			fault: func(_ *testing.T, n *trialNode) {
				n.mu.Lock()
				defer n.mu.Unlock()
				n.server.Stop()
			},
			want:   []string{notAnswered},
			refuse: []string{"may still trust", "management authority"},
		},
		"dial blocks until timeout": {
			timeout: true,
			fault: func(_ *testing.T, n *trialNode) {
				dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
					return n.dialWithOptions(ctx, endpoint, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
						<-ctx.Done()
						return nil, ctx.Err()
					}))
				}
			},
			want:     []string{notAnswered},
			refuse:   []string{"may still trust", "management authority"},
			deadline: true,
		},
		"untrusted certificate": {
			mutualTLS: true,
			fault: func(t *testing.T, n *trialNode) {
				n.mu.Lock()
				defer n.mu.Unlock()
				n.staged = foreignNodeCredentials(t, n.name)
				n.rebindLocked()
			},
			want:   []string{"the node may still trust a different management authority"},
			refuse: []string{"did not answer"},
		},
		"trusted-network configuration at an mTLS node": {
			fault: func(t *testing.T, n *trialNode) {
				n.mu.Lock()
				defer n.mu.Unlock()
				n.mode = managementidentity.MutualTLS
				n.staged = foreignNodeCredentials(t, n.name)
				n.rebindLocked()
			},
			want:   []string{"the node may still trust an mTLS authority"},
			refuse: []string{"did not answer"},
		},
		"listener closes during handshake": {
			mutualTLS: true,
			fault: func(_ *testing.T, n *trialNode) {
				dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
					return n.dialWithOptions(ctx, endpoint, grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
						client, server := net.Pipe()
						go func() {
							defer server.Close()
							var header [5]byte
							if _, err := io.ReadFull(server, header[:]); err == nil {
								_, _ = io.CopyN(io.Discard, server, int64(binary.BigEndian.Uint16(header[3:])))
							}
						}()
						return client, nil
					}))
				}
			},
			want:   []string{"could not confirm"},
			refuse: []string{"did not answer", "may still trust"},
		},
		"reply after deadline": {
			timeout: true,
			fault: func(_ *testing.T, n *trialNode) {
				n.interceptRPC = delayManagementStatus
			},
			want:     []string{"could not confirm"},
			refuse:   []string{"did not answer", "may still trust"},
			deadline: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
				if test.mutualTLS {
					if _, stderr, err := world.enable(); err != nil {
						t.Fatalf("enable-mtls: %v\n%s", err, stderr)
					}
				}
				save := []string{"context", "save", "--config", world.configPath}
				if err := run(t.Context(), save, io.Discard, io.Discard); err != nil {
					t.Fatal(err)
				}
				contextPath, err := workstation.ConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(contextPath)
				if err != nil {
					t.Fatal(err)
				}

				node := world.nodes[0]
				dialKatlcAgent = node.dial
				test.fault(t, node)
				rebind := []string{"context", "rebind", "--node", node.name, "--endpoint", "192.0.2.91"}
				for _, args := range [][]string{save, rebind} {
					if test.timeout {
						args = append(args, "--timeout", "1s")
					}

					err := run(t.Context(), args, io.Discard, io.Discard)

					if err == nil {
						t.Fatalf("%s accepted the endpoint", args[1])
					}
					for _, want := range append([]string{"could not confirm"}, test.want...) {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("%s error = %v, missing %q", args[1], err, want)
						}
					}
					for _, refuse := range test.refuse {
						if strings.Contains(err.Error(), refuse) {
							t.Errorf("%s error = %v, contains %q", args[1], err, refuse)
						}
					}
					if test.deadline && !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("%s error = %v, want a deadline error", args[1], err)
					}
					after, readErr := os.ReadFile(contextPath)
					if readErr != nil || string(before) != string(after) {
						t.Fatalf("failed verification changed the context: %v", readErr)
					}
				}
			})
		})
	}
}

// cluster apply refreshes its view of the nodes through the same context check.
func TestClusterApplyExplainsStoppedNode(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
		node := world.nodes[0]
		node.mu.Lock()
		node.server.Stop()
		node.mu.Unlock()

		err := run(t.Context(), []string{"cluster", "apply", "--config", world.configPath}, io.Discard, io.Discard)

		if err == nil || !strings.Contains(err.Error(), "the node did not answer; retry once 'katlctl node status' shows it up") {
			t.Fatalf("cluster apply error = %v, want it to say the node did not answer", err)
		}
		for _, refuse := range []string{"may still trust", "management authority"} {
			if strings.Contains(err.Error(), refuse) {
				t.Errorf("cluster apply error = %v, contains %q", err, refuse)
			}
		}
	})
}

func TestManagementFailureText(t *testing.T) {
	const check = "the node answers"
	failures := map[string]error{
		"nil":          nil,
		"status":       grpcstatus.Error(codes.Unavailable, "connection error"),
		"deadline":     fmt.Errorf("timed out: %w", context.DeadlineExceeded),
		"unclassified": errors.New("management status did not report a valid node time"),
	}

	t.Run("read", func(t *testing.T) {
		for name, err := range failures {
			got := managementReadError(check, err).Error()
			want := "could not confirm the node answers"
			if err != nil {
				want += ": " + err.Error()
			}
			if got != want {
				t.Errorf("%s: error = %q, want %q", name, got, want)
			}
			if err != nil && !errors.Is(managementReadError(check, err), err) {
				t.Errorf("%s: cause is not wrapped", name)
			}
		}
	})

	// An error that carries connection evidence: the node took no connection.
	stopped := newTrialNode(t, "cp-1")
	stopped.mu.Lock()
	stopped.server.Stop()
	stopped.mu.Unlock()
	ctx := transport.WithClientCredentials(transport.WithoutManagementWait(t.Context()), managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork})
	conn, err := stopped.dial(ctx, "192.0.2.11:9443")
	if err != nil {
		t.Fatal(err)
	}
	_, unreachable := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	_ = conn.Close()
	if got := transport.ClassifyManagementEvidence(unreachable, nil, nil); got != transport.ManagementUnreachable {
		t.Fatalf("stopped node evidence = %v, want unreachable: %v", got, unreachable)
	}

	t.Run("read of an unreachable node", func(t *testing.T) {
		got := managementReadError(check, unreachable).Error()
		want := "could not confirm the node answers: the node did not answer; retry once 'katlctl node status' shows it up: " + unreachable.Error()
		if got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
	})

	failures["unreachable"] = unreachable
	t.Run("trust change", func(t *testing.T) {
		priors := map[managementTrust]string{
			trustOldAuthority:          "the old authority",
			trustPreviousAuthority:     "the previous authority",
			trustNodePreviousAuthority: "its previous management authority",
			trustTrustedNetwork:        "the trusted network",
			trustReplacementAuthority:  "the replacement authority",
		}
		for prior, noun := range priors {
			for name, err := range failures {
				got := managementTrustChangeError(check, prior, err).Error()
				want := "could not confirm the node answers; the node may still trust " + noun
				if err != nil {
					want += ": " + err.Error()
				}
				if got != want {
					t.Errorf("%s/%s: error = %q, want %q", noun, name, got, want)
				}
			}
		}
	})
}
