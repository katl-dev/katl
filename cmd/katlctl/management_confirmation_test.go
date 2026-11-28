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
	"syscall"
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
	const notAnswered = "the node did not answer"
	tests := map[string]struct {
		mutualTLS bool
		timeout   bool
		fault     func(*testing.T, *trialNode)
		want      []string
		// rebindWant replaces want for the rebind to a proposed address.
		rebindWant []string
		refuse     []string
		deadline   bool
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
			// At the saved address the certificate came from the node; at the
			// proposed address it shows only what answered there.
			want:       []string{"the node may still trust a different management authority"},
			rebindWant: []string{"the endpoint at 192.0.2.91:9443 presented a certificate the configured authority did not sign"},
			refuse:     []string{"did not answer"},
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
					want := test.want
					if args[1] == "rebind" && test.rebindWant != nil {
						want = test.rebindWant
					}
					for _, want := range append([]string{"could not confirm"}, want...) {
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

func TestManagementFailureText(t *testing.T) {
	check := managementCheck{claim: "the node answers"}
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
		want := "could not confirm the node answers: the node did not answer: " + unreachable.Error()
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

// A failed check advises from its evidence and from whether its address is
// bound to the node. A node status check uses the saved address, so it can only
// help when the failing check used that address too. Another authority's
// certificate at the saved address came from the node, which needs the
// authority remedy and no address advice; at any other address it may have come
// from anything, so the address is checked before the remedy applies.
func TestManagementFailureAdvice(t *testing.T) {
	const (
		statusAdvice  = "'katlctl node status' shows it up"
		addressAdvice = "is the node's management address"
		remedy        = "katlctl management identity recover"
		enableRemedy  = "rerun with the secrets file of the enable-mtls attempt that left the node on trial, or wait for its trial window to end"
		unsigned      = "presented a certificate the configured authority did not sign"
	)
	stopped := func(_ *testing.T, n *trialNode) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.server.Stop()
	}
	refused := func(_ *testing.T, n *trialNode) {
		dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
			return n.dialWithOptions(ctx, endpoint, grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
			}))
		}
	}
	foreign := func(t *testing.T, n *trialNode) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.staged = foreignNodeCredentials(t, n.name)
		n.rebindLocked()
	}
	save := func(w enableMTLSWorld) []string {
		return []string{"context", "save", "--config", w.configPath, "--timeout", "1s"}
	}
	rebind := func(enableMTLSWorld) []string {
		return []string{"context", "rebind", "--node", "cp-1", "--endpoint", "192.0.2.11:9999", "--timeout", "1s"}
	}
	apply := func(w enableMTLSWorld) []string {
		return []string{"cluster", "apply", "--config", w.configPath}
	}
	applyAt := func(w enableMTLSWorld) []string {
		return []string{"cluster", "apply", "--config", w.configPath, "--node", "cp-1", "--endpoint", "192.0.2.11"}
	}
	enable := func(w enableMTLSWorld) []string {
		return []string{"management", "enable-mtls", "--config", w.configPath, "--timeout", "3s"}
	}
	notBooting := []string{"may still be booting", "increase --timeout"}
	noAddressAdvice := []string{"reachability", "check the address", "check that", "node status"}
	checkFirst := func(endpoint string) string {
		return "check that " + endpoint + " is the node's management address; if it is, "
	}

	tests := map[string]struct {
		mutualTLS bool
		// trial leaves the node on an mTLS trial this workstation cannot
		// reach, so a rerun finds a node that presents another authority.
		trial bool
		// savedFirst saves a context before the trial, binding the node's
		// address.
		savedFirst bool
		fault      func(*testing.T, *trialNode)
		command    func(enableMTLSWorld) []string
		want       []string
		refuse     []string
	}{
		"saved address, node down":        {fault: stopped, command: save, want: []string{statusAdvice}, refuse: []string{"check that"}},
		"saved address, port closed":      {fault: refused, command: save, want: []string{statusAdvice}, refuse: append([]string{"check that"}, notBooting...)},
		"apply at saved address, down":    {fault: stopped, command: apply, want: []string{statusAdvice}, refuse: []string{"check that"}},
		"supplied address, node down":     {fault: stopped, command: rebind, want: []string{addressAdvice}, refuse: []string{"node status"}},
		"supplied address, port closed":   {fault: refused, command: rebind, want: []string{addressAdvice}, refuse: append([]string{"node status"}, notBooting...)},
		"apply at supplied address, down": {fault: stopped, command: applyAt, want: []string{addressAdvice}, refuse: []string{"node status"}},
		"apply at supplied address, closed": {
			fault: refused, command: applyAt, want: []string{addressAdvice}, refuse: append([]string{"node status"}, notBooting...),
		},
		"context save, other authority":  {mutualTLS: true, fault: foreign, command: save, want: []string{remedy}, refuse: append([]string{unsigned}, noAddressAdvice...)},
		"cluster apply, other authority": {mutualTLS: true, fault: foreign, command: apply, want: []string{remedy}, refuse: append([]string{unsigned}, noAddressAdvice...)},
		"context rebind, other authority": {
			mutualTLS: true, fault: foreign, command: rebind,
			want:   []string{"the endpoint at 192.0.2.11:9999 " + unsigned, checkFirst("192.0.2.11:9999") + "after rotation", remedy},
			refuse: []string{"may still trust", "reachability", "node status"},
		},
		"apply at address, other authority": {
			mutualTLS: true, fault: foreign, command: applyAt,
			want:   []string{"the endpoint at 192.0.2.11:9443 " + unsigned, checkFirst("192.0.2.11:9443") + "after rotation", remedy},
			refuse: []string{"may still trust", "reachability", "node status"},
		},
		"enable-mtls preflight, other authority": {
			trial: true, fault: func(*testing.T, *trialNode) {}, command: enable,
			want:   []string{"no node was changed", checkFirst("192.0.2.11:9443") + "rerun with the secrets file", enableRemedy, "use the mTLS ClusterConfig"},
			refuse: []string{"identity recover", "after rotation", "reachability", "node status"},
		},
		"enable-mtls preflight at saved address, other authority": {
			trial: true, savedFirst: true, fault: func(*testing.T, *trialNode) {}, command: enable,
			want:   []string{"no node was changed", enableRemedy, "use the mTLS ClusterConfig"},
			refuse: []string{"identity recover", "after rotation", "reachability", "node status", "check that"},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
				node := world.nodes[0]
				if test.mutualTLS {
					if _, stderr, err := world.enable(); err != nil {
						t.Fatalf("enable-mtls: %v\n%s", err, stderr)
					}
				}
				if test.savedFirst {
					if err := run(t.Context(), []string{"context", "save", "--config", world.configPath}, io.Discard, io.Discard); err != nil {
						t.Fatal(err)
					}
				}
				if test.trial {
					node.unreachableTrial = true
					if _, _, err := world.enable(); err == nil {
						t.Fatal("enable-mtls reached a trial it should not reach")
					}
					node.rebinds.Wait()
				}
				if err := run(t.Context(), []string{"context", "save", "--config", world.configPath}, io.Discard, io.Discard); err != nil && !test.trial {
					t.Fatal(err)
				}
				dialKatlcAgent = node.dial
				test.fault(t, node)

				err := run(t.Context(), test.command(world), io.Discard, io.Discard)

				if err == nil {
					t.Fatal("command succeeded against a failing node")
				}
				for _, want := range test.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error lacks %q: %v", want, err)
					}
				}
				for _, refuse := range test.refuse {
					if strings.Contains(err.Error(), refuse) {
						t.Errorf("error contains %q: %v", refuse, err)
					}
				}
			})
		})
	}
}

// foreignListener serves management TLS as name from an authority the cluster
// does not use. It is not the node: it stands for another cluster's node,
// another TLS service or an intercepting proxy at a mistaken address.
func foreignListener(t *testing.T, name string) *trialNode {
	t.Helper()
	listener := newTrialNode(t, name)
	listener.mu.Lock()
	defer listener.mu.Unlock()
	listener.mode, listener.staged = managementidentity.MutualTLS, foreignNodeCredentials(t, name)
	listener.rebindLocked()
	return listener
}

// routeManagement sends each dialed endpoint to its listener.
func routeManagement(listeners map[string]*trialNode) {
	dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
		listener, ok := listeners[endpoint]
		if !ok {
			return katlcAgentConnection{}, fmt.Errorf("no test listener at %s", endpoint)
		}
		return listener.dial(ctx, endpoint)
	}
}

// A certificate from another authority at a supplied address does not show
// that the node answered, so the reader checks the address before changing
// trust. The node itself is unchanged and still answers at its saved address.
func TestManagementAdviceAtForeignListener(t *testing.T) {
	for name, command := range map[string]func(enableMTLSWorld) []string{
		"context rebind": func(enableMTLSWorld) []string {
			return []string{"context", "rebind", "--node", "worker-1", "--endpoint", "192.0.2.99", "--timeout", "1s"}
		},
		"cluster apply": func(w enableMTLSWorld) []string {
			return []string{"cluster", "apply", "--config", w.configPath, "--node", "worker-1", "--endpoint", "192.0.2.99"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			testEnableMTLSWithVirtualTime(t, []string{"cp-1", "worker-1"}, func(t *testing.T, world enableMTLSWorld) {
				if _, stderr, err := world.enable(); err != nil {
					t.Fatalf("enable-mtls: %v\n%s", err, stderr)
				}
				if err := run(t.Context(), []string{"context", "save", "--config", world.configPath}, io.Discard, io.Discard); err != nil {
					t.Fatal(err)
				}
				routeManagement(map[string]*trialNode{
					"192.0.2.11:9443": world.nodes[0], "192.0.2.12:9443": world.nodes[1],
					"192.0.2.99:9443": foreignListener(t, "worker-1"),
				})

				err := run(t.Context(), command(world), io.Discard, io.Discard)

				if err == nil {
					t.Fatal("command accepted a foreign listener")
				}
				for _, want := range []string{
					"the endpoint at 192.0.2.99:9443 presented a certificate the configured authority did not sign",
					"check that 192.0.2.99:9443 is the node's management address; if it is, ",
				} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error lacks %q: %v", want, err)
					}
				}
				if strings.Contains(err.Error(), "the node may still trust") {
					t.Errorf("error attributes the certificate to the node: %v", err)
				}

				contextPath, err := workstation.ConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				if saved := savedEndpoint(contextPath, "lab", "worker-1"); saved.String() != "192.0.2.12:9443" {
					t.Errorf("saved endpoint = %s, want the node's address 192.0.2.12:9443", saved)
				}
				if err := run(t.Context(), []string{"node", "status", "worker-1"}, io.Discard, io.Discard); err != nil {
					t.Errorf("node status at the saved address: %v", err)
				}
			})
		})
	}
}

// A ClusterConfig address is bound to the node only when the saved context
// records it. At a bound address another authority's certificate came from the
// node, so no stale-address hint follows; at an unbound address the hint and
// the address check stay, and a node that did not answer is not sent to
// 'katlctl node status', which uses the saved address.
func TestContextSaveAdviceAtConfigAddress(t *testing.T) {
	const readdress = "if you are changing its address"
	tests := map[string]struct {
		// moves rewrites a node's ClusterConfig address after the context
		// is saved; the new address reaches listener.
		moves    map[string]string
		listener func(*testing.T) *trialNode
		// foreignNode restages the node itself with another authority.
		foreignNode string
		want        []string
		refuse      []string
	}{
		"worker at saved address, other authority": {
			foreignNode: "worker-1",
			want:        []string{"the node may still trust a different management authority", "katlctl management identity recover"},
			refuse:      []string{"check that", "if you are changing", "presented a certificate"},
		},
		"worker at unsaved address, other authority": {
			moves:    map[string]string{"worker-1": "192.0.2.50"},
			listener: func(t *testing.T) *trialNode { return foreignListener(t, "worker-1") },
			want: []string{
				"the endpoint at 192.0.2.50:9443 presented a certificate the configured authority did not sign",
				"check that 192.0.2.50:9443 is the node's management address; if it is, ",
				"the saved context last reached it at 192.0.2.12:9443; " + readdress,
			},
			refuse: []string{"the node may still trust"},
		},
		"control plane at unsaved address, down": {
			moves: map[string]string{"cp-1": "192.0.2.51"},
			listener: func(t *testing.T) *trialNode {
				listener := newTrialNode(t, "cp-1")
				listener.mu.Lock()
				defer listener.mu.Unlock()
				listener.server.Stop()
				return listener
			},
			want:   []string{"the node did not answer at 192.0.2.51:9443", "check that 192.0.2.51:9443 is the node's management address"},
			refuse: []string{"node status"},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			testEnableMTLSWithVirtualTime(t, []string{"cp-1", "worker-1"}, func(t *testing.T, world enableMTLSWorld) {
				if _, stderr, err := world.enable(); err != nil {
					t.Fatalf("enable-mtls: %v\n%s", err, stderr)
				}
				if err := run(t.Context(), []string{"context", "save", "--config", world.configPath}, io.Discard, io.Discard); err != nil {
					t.Fatal(err)
				}
				listeners := map[string]*trialNode{"192.0.2.11:9443": world.nodes[0], "192.0.2.12:9443": world.nodes[1]}
				config, err := os.ReadFile(world.configPath)
				if err != nil {
					t.Fatal(err)
				}
				for node, address := range test.moves {
					previous := map[string]string{"cp-1": "192.0.2.11", "worker-1": "192.0.2.12"}[node]
					config = []byte(strings.Replace(string(config), "address: "+previous+"\n", "address: "+address+"\n", 1))
					listeners[address+":9443"] = test.listener(t)
				}
				if err := os.WriteFile(world.configPath, config, 0o600); err != nil {
					t.Fatal(err)
				}
				for _, node := range world.nodes {
					if node.name == test.foreignNode {
						node.mu.Lock()
						node.staged = foreignNodeCredentials(t, node.name)
						node.rebindLocked()
						node.mu.Unlock()
					}
				}
				routeManagement(listeners)

				err = run(t.Context(), []string{"context", "save", "--config", world.configPath, "--timeout", "1s"}, io.Discard, io.Discard)

				if err == nil {
					t.Fatal("context save succeeded against a failing endpoint")
				}
				for _, want := range test.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error lacks %q: %v", want, err)
					}
				}
				for _, refuse := range test.refuse {
					if strings.Contains(err.Error(), refuse) {
						t.Errorf("error contains %q: %v", refuse, err)
					}
				}
			})
		})
	}
}
