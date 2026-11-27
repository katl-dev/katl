package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nettest"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"github.com/katl-dev/katl/internal/workstationtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcstatus "google.golang.org/grpc/status"
)

func TestManagementIdentityRotateReplacesLiveTrust(t *testing.T) {
	testManagementRotationLiveTrust(t, "", false, false, false)
}

func TestManagementIdentityRotateRetriesWithClockSkew(t *testing.T) {
	testManagementRotationLiveTrust(t, "", false, true, false)
}

func TestManagementIdentityRotateRequiresIssuerRejection(t *testing.T) {
	t.Run("old leaf expires at the server", func(t *testing.T) {
		testManagementRotationLiveTrust(t, "expired leaf", false, false, false)
	})
	t.Run("old leaf expires with a second node pending", func(t *testing.T) {
		testManagementRotationLiveTrust(t, "expired leaf", true, false, false)
	})
	t.Run("TLS policy rejects a fresh leaf generically", func(t *testing.T) {
		testManagementRotationLiveTrust(t, "generic rejection", true, false, false)
	})
}

func TestManagementIdentityRotateUsesServedTrust(t *testing.T) {
	t.Run("replacement alone with a slow node clock", func(t *testing.T) {
		testManagementRotationLiveTrust(t, "", false, true, true)
	})
	t.Run("old authority also served", func(t *testing.T) {
		testManagementRotationLiveTrust(t, "reported extra CA", true, false, true)
	})
}

func testManagementRotationLiveTrust(t *testing.T, rejection string, addWorker, retryWithClockSkew, reportTrust bool) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "no-context.yaml"))
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	configPath := filepath.Join(dir, "cluster.yaml")
	config := strings.Replace(configBundleSource(), "  managementAuthentication: mtls\n", "  managementAuthentication: mtls\n  managementIdentity: "+defaultManagementSecrets+"\n", 1)
	if addWorker {
		config += `    - name: worker-1
      management:
        address: 192.0.2.12
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-worker-root
`
	}
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(dir, defaultManagementSecrets)
	outputPath := filepath.Join(dir, "management-secrets-next.yaml")
	now := time.Now()
	serverTime := now.Add(2 * time.Hour)
	old, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab", Now: now.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if rejection == "expired leaf" {
		shortenRotationTestLeaf(t, &old, serverTime.Add(-time.Hour))
	}
	if err := managementidentity.Write(oldPath, old); err != nil {
		t.Fatal(err)
	}

	var replacement managementidentity.Bundle
	if retryWithClockSkew {
		serverTime = now.Add(-10 * time.Minute)
		issued := now.Add(-time.Hour)
		replacement, err = managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: old.ClusterName, Now: issued})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := managementidentity.EnsureNode(&replacement, "cp-1", issued, nil); err != nil {
			t.Fatal(err)
		}
		if err := managementidentity.Write(outputPath, replacement); err != nil {
			t.Fatal(err)
		}
	}
	oldInfo, err := managementidentity.Validate(old, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	machineID, err := nodeidentity.WriteMachineID(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := nodeidentity.WriteEnrollment(root, "cp-1", machineID, nil)
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := managementidentity.EnsureNode(&old, "cp-1", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.WriteManagementIdentity(root, "cp-1", node); err != nil {
		t.Fatal(err)
	}
	var server *grpc.Server
	var listener net.Listener
	var endpoint string
	var rotated bool
	start := func() {
		t.Helper()
		if server != nil {
			server.Stop()
			listener.Close()
		}
		tlsConfig, authorities, err := transport.ServerTLSConfig(root)
		if err != nil {
			t.Fatal(err)
		}
		installed := authorities[0]
		switch {
		case rotated && retryWithClockSkew:
			tlsConfig.Time = func() time.Time { return serverTime }
		case rotated && rejection != "":
			if !tlsConfig.ClientCAs.AppendCertsFromPEM([]byte(old.CertificateAuthority.Certificate)) {
				t.Fatal("add old authority to deliberately unsafe test server")
			}
			authorities = append(authorities, oldInfo.Fingerprint)
			switch rejection {
			case "expired leaf":
				// The server clock expires only the original operator leaf.
				tlsConfig.Time = func() time.Time { return serverTime }
			case "generic rejection":
				oldCA, _ := pem.Decode([]byte(old.CertificateAuthority.Certificate))
				tlsConfig.VerifyPeerCertificate = func(_ [][]byte, chains [][]*x509.Certificate) error {
					for _, chain := range chains {
						if bytes.Equal(chain[len(chain)-1].Raw, oldCA.Bytes) {
							return errors.New("client certificate denied by policy")
						}
					}
					return nil
				}
			}
		}
		listenAddress := endpoint
		if listenAddress == "" {
			listenAddress = "127.0.0.1:0"
		}
		listener, err = net.Listen("tcp", listenAddress)
		if err != nil {
			t.Fatal(err)
		}
		endpoint = listener.Addr().String()
		server = grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
		status := enrolledStatus("cp-1", enrollment.ID, machineID)
		if reportTrust {
			status.Management = &agentapi.ManagementStatus{
				Authentication: "mtls", Authority: installed,
				ServedClientTrust: &agentapi.ManagementClientTrust{Authorities: authorities},
			}
		}
		agentapi.RegisterKatlcAgentServer(server, &clusterApplyIdentityServer{client: &fakeKatlcAgentClient{nodeStatus: status}})
		go server.Serve(listener)
	}
	start()
	t.Cleanup(func() { server.Stop(); listener.Close() })
	configured, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configured = bytes.ReplaceAll(configured, []byte("10.0.0.11"), []byte(endpoint))
	if err := os.WriteFile(configPath, configured, 0o600); err != nil {
		t.Fatal(err)
	}
	previousDial := dialKatlcAgent
	transientDialFailures := 0
	dialKatlcAgent = func(ctx context.Context, _ string) (katlcAgentConnection, error) {
		if transientDialFailures > 0 {
			transientDialFailures--
			return katlcAgentConnection{}, errors.New("connection refused")
		}
		return dialKatlcAgentTCP(ctx, endpoint)
	}
	t.Cleanup(func() { dialKatlcAgent = previousDial })
	previousSSH := runManagementRotationSSH
	var changed []string
	interrupted := errors.New("SSH disconnected after switching the node")
	runManagementRotationSSH = func(_ context.Context, target managementRotationNode, _ managementRotationOptions, dryRun bool) (nodeidentity.ManagementRotationStatus, error) {
		if target.name != "cp-1" {
			if !dryRun {
				changed = append(changed, target.name)
				return nodeidentity.ManagementRotationStatus{}, errors.New("rotation reached the second node")
			}
			return nodeidentity.ManagementRotationStatus{}, nil
		}
		if dryRun {
			return nodeidentity.InspectManagementRotation(root, target.request)
		}
		changed = append(changed, target.name)
		result, err := nodeidentity.RotateManagementIdentity(root, target.request)
		if err == nil {
			rotated = true
			start()
			switch {
			case retryWithClockSkew && len(changed) == 1:
				return result, interrupted
			case rejection == "" && !retryWithClockSkew:
				transientDialFailures = 1
			}
		}
		return result, err
	}
	t.Cleanup(func() { runManagementRotationSSH = previousSSH })
	args := []string{"management", "identity", "rotate", "--config", configPath, "--output", outputPath, "--timeout", "3s"}
	var stdout, stderr bytes.Buffer
	if retryWithClockSkew {
		if err := run(t.Context(), args, &stdout, &stderr); !errors.Is(err, interrupted) {
			t.Fatalf("first rotation = %v, want an interruption after switching the node", err)
		}
		data, err := os.ReadFile(configPath)
		if err != nil || !bytes.Equal(data, configured) {
			t.Fatalf("rotation published the replacement reference before verification: %v", err)
		}
		target := managementRotationNode{name: "cp-1", endpoint: mustHostPort(t, endpoint), machineID: machineID}
		authority, err := managementidentity.CAFingerprint(replacement.CertificateAuthority.Certificate)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := checkManagementCredential(t.Context(), target, mustManagementClient(t, replacement), authority, time.Second); err != nil {
			t.Fatalf("node with a slow clock did not accept the existing replacement: %v", err)
		}
		stdout.Reset()
		stderr.Reset()
	}
	err = run(t.Context(), args, &stdout, &stderr)
	if rejection != "" {
		data, readErr := os.ReadFile(configPath)
		if readErr != nil || !bytes.Equal(data, configured) {
			t.Fatalf("rotation published the replacement reference: %v", readErr)
		}
		if len(changed) != 1 || changed[0] != "cp-1" {
			t.Fatalf("rotation changed nodes %v after an unproven old-issuer rejection", changed)
		}
		want := "old authority still has management access"
		if rejection == "generic rejection" {
			want = "could not confirm the node stopped trusting the old authority; the node may still trust the old authority"
		}
		if reportTrust {
			want = "listener trusts client CAs"
		}
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "rerun with the same --output") {
			t.Fatalf("rotate = %v\n%s\nwant %q and rerun guidance", err, &stderr, want)
		}
		return
	}
	if err != nil {
		t.Fatalf("rotate: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Rotated management identity") {
		t.Fatalf("rotation result = %q", stdout.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil || !strings.Contains(string(data), "managementIdentity: management-secrets-next.yaml") {
		t.Fatalf("config does not reference replacement: %v", err)
	}
	next, err := readManagementIdentity(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	nextInfo, err := managementidentity.Validate(next, time.Now())
	if err != nil || nextInfo.Fingerprint == oldInfo.Fingerprint {
		t.Fatalf("authority was not replaced: %v", err)
	}
	probe := func() (managementidentity.CertificateKey, error) {
		if reportTrust {
			t.Fatal("requested a legacy probe after receiving an exact trust report")
		}
		return old.Operator, nil
	}
	if err := verifyRotatedManagement(context.Background(), managementRotationNode{name: "cp-1", endpoint: mustHostPort(t, endpoint), machineID: machineID}, mustManagementClient(t, next), nextInfo.Fingerprint, probe, 3*time.Second); err != nil {
		t.Fatalf("independent management check: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{"management", "identity", "rotate", "--config", configPath, "--output", outputPath, "--timeout", "3s"}, &stdout, &stderr); err != nil {
		t.Fatalf("repeat rotation: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already uses") {
		t.Fatalf("repeat result = %q", stdout.String())
	}
}

func shortenRotationTestLeaf(t *testing.T, bundle *managementidentity.Bundle, expires time.Time) {
	t.Helper()
	operator, err := tls.X509KeyPair([]byte(bundle.Operator.Certificate), []byte(bundle.Operator.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := tls.X509KeyPair([]byte(bundle.CertificateAuthority.Certificate), []byte(bundle.CertificateAuthority.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	template := *operator.Leaf
	template.NotAfter = expires
	der, err := x509.CreateCertificate(rand.Reader, &template, ca.Leaf, template.PublicKey, ca.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Operator.Certificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestManagementIdentityRotateConfiguredReplacement(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		wantCause string
	}{
		{"connection refused", "connection refused"},
		{"closes after ServerHello", "EOF"},
		{"different authority", "certificate signed by unknown authority"},
		{"extra reported authority", "listener trusts client CAs"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			testManagementRotationConfiguredReplacement(t, scenario.name, scenario.wantCause)
		})
	}
}

func testManagementRotationConfiguredReplacement(t *testing.T, scenario, wantCause string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "no-context.yaml"))
	var listener net.Listener
	var endpoint string
	if scenario == "connection refused" {
		endpoint = nettest.ReservePort(t).Endpoint
	} else {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		endpoint = listener.Addr().String()
	}
	configPath := filepath.Join(dir, "cluster.yaml")
	config := strings.ReplaceAll(configBundleSource(), "10.0.0.11", endpoint)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	createTestManagementSecrets(t, configPath)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(dir, defaultManagementSecrets)
	var serverResult chan error
	if listener != nil {
		bundle, err := readManagementIdentity(outputPath)
		if err != nil {
			t.Fatal(err)
		}
		if scenario == "different authority" {
			bundle, err = managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: bundle.ClusterName})
			if err != nil {
				t.Fatal(err)
			}
		}
		node, _, err := managementidentity.EnsureNode(&bundle, "cp-1", time.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
		tlsConfig, authorities, err := transport.ServerTLSConfigForNode(node)
		if err != nil {
			t.Fatal(err)
		}
		if scenario == "closes after ServerHello" {
			tlsConfig.NextProtos = []string{"h2"}
			serverResult = make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					err = tls.Server(serverHelloConn{conn}, tlsConfig).HandshakeContext(t.Context())
					conn.Close()
				}
				serverResult <- err
			}()
		} else {
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
			if scenario == "extra reported authority" {
				status := enrolledStatus("cp-1", "installed", "machine")
				status.Management = &agentapi.ManagementStatus{
					Authentication: "mtls", Authority: authorities[0],
					ServedClientTrust: &agentapi.ManagementClientTrust{Authorities: append(authorities, "extra")},
				}
				agentapi.RegisterKatlcAgentServer(server, &clusterApplyIdentityServer{client: &fakeKatlcAgentClient{nodeStatus: status}})
			}
			t.Cleanup(server.Stop)
			go server.Serve(listener)
		}
	}
	var stdout, stderr bytes.Buffer
	err = run(t.Context(), []string{"management", "identity", "rotate", "--config", configPath, "--output", outputPath, "--timeout", "3s"}, &stdout, &stderr)
	if serverResult != nil {
		select {
		case serverErr := <-serverResult:
			if !errors.Is(serverErr, errServerHelloSent) {
				t.Fatalf("server did not close after ServerHello: %v", serverErr)
			}
		case <-time.After(time.Second):
			t.Fatal("server did not complete its side of the handshake")
		}
	}
	for _, want := range []string{
		"could not confirm the node accepts the configured authority; the node may still trust the previous authority",
		wantCause, "keep " + outputPath + ", check the endpoint and rerun the same command",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("rotate = %v, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "restore") || strings.Contains(err.Error(), "different management authority") {
		t.Fatalf("rotate inferred a trust mismatch or advised rollback: %v", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("configured replacement changed after verification failed: %v", err)
	}
}

var errServerHelloSent = errors.New("closed the connection after sending ServerHello")

type serverHelloConn struct{ net.Conn }

func (c serverHelloConn) Write(p []byte) (int, error) {
	const headerSize, handshakeRecord, serverHello = 5, 22, 2
	if len(p) <= headerSize || p[0] != handshakeRecord || p[headerSize] != serverHello {
		return 0, errors.New("first server TLS record does not contain ServerHello")
	}
	recordEnd := headerSize + int(binary.BigEndian.Uint16(p[3:headerSize]))
	if recordEnd > len(p) {
		return 0, errors.New("first server TLS record is incomplete")
	}
	// The TLS server can flush several records together. Forward only the
	// first, so the client sees ServerHello before the connection closes.
	n, err := c.Conn.Write(p[:recordEnd])
	if err != nil {
		return n, err
	}
	return n, errServerHelloSent
}

func TestManagementIdentityRotateLateCredentialCheck(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1", "worker-1"}, func(t *testing.T, world enableMTLSWorld) {
		if _, stderr, err := world.enable(); err != nil {
			t.Fatalf("enable-mtls: %v\n%s", err, stderr)
		}
		node := world.nodes[0]
		// Keep the old-credential probe on loopback even if verification
		// incorrectly proceeds past the late replacement reply.
		config, err := os.ReadFile(world.configPath)
		if err != nil {
			t.Fatal(err)
		}
		unserved := nettest.ReservePort(t).Endpoint
		config = bytes.ReplaceAll(config, []byte("192.0.2.11"), []byte(unserved))
		if err := os.WriteFile(world.configPath, config, 0o600); err != nil {
			t.Fatal(err)
		}
		dial := dialKatlcAgent
		dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
			if endpoint == unserved {
				return node.dial(ctx, endpoint)
			}
			return dial(ctx, endpoint)
		}
		rotate := runManagementRotationSSH
		t.Cleanup(func() { runManagementRotationSSH = rotate })
		var changed []string
		runManagementRotationSSH = func(_ context.Context, target managementRotationNode, _ managementRotationOptions, dryRun bool) (nodeidentity.ManagementRotationStatus, error) {
			if !dryRun {
				changed = append(changed, target.name)
				if target.name != node.name {
					return nodeidentity.ManagementRotationStatus{}, errors.New("rotation reached the second node")
				}
				node.mu.Lock()
				node.staged = target.request.Replacement
				node.rebindLocked()
				node.mu.Unlock()
				node.rebinds.Wait()
				node.interceptRPC = delayManagementStatus
			}
			return nodeidentity.ManagementRotationStatus{}, nil
		}

		output := filepath.Join(filepath.Dir(world.configPath), "next.yaml")
		var stdout, stderr bytes.Buffer
		err = run(t.Context(), []string{"management", "identity", "rotate", "--config", world.configPath, "--output", output, "--timeout", "1s"}, &stdout, &stderr)
		if len(changed) != 1 || changed[0] != node.name {
			t.Fatalf("rotation changed %v after a late reply from %s: %v", changed, node.name, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), output) ||
			!strings.Contains(err.Error(), "could not confirm the node accepts the replacement authority; the node may still trust the old authority") {
			t.Fatalf("rotation = %v\n%s\nwant a deadline error and guidance to keep the replacement secrets", err, &stderr)
		}
		if _, reference := world.configMode(t); reference != defaultManagementSecrets {
			t.Fatalf("rotation published %q after late credential verification", reference)
		}
	})
}

func TestManagementRotationOldAuthorityRefusal(t *testing.T) {
	for _, scenario := range []string{"timely", "late", "other connection", "background after deadline", "unrelated failure"} {
		t.Run(scenario, func(t *testing.T) {
			testEnableMTLSWithVirtualTime(t, []string{"cp-1"}, func(t *testing.T, world enableMTLSWorld) {
				if _, stderr, err := world.enable(); err != nil {
					t.Fatalf("enable-mtls: %v\n%s", err, stderr)
				}
				next, err := readManagementIdentity(world.secretsPath)
				if err != nil {
					t.Fatal(err)
				}
				old, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
				if err != nil {
					t.Fatal(err)
				}
				probe, err := managementidentity.IssueRotationProbe(old.CertificateAuthority, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				node := world.nodes[0]
				dial := dialKatlcAgentWithTransport
				t.Cleanup(func() { dialKatlcAgentWithTransport = dial })
				sawRefusal := false
				dialKatlcAgentWithTransport = func(ctx context.Context, _, endpoint string, creds credentials.TransportCredentials) (katlcAgentConnection, error) {
					if scenario == "other connection" || scenario == "background after deadline" {
						return node.dialWithTransport(ctx, endpoint, creds,
							grpc.WithChainUnaryInterceptor(func(context.Context, string, any, any, *grpc.ClientConn, grpc.UnaryInvoker, ...grpc.CallOption) error {
								return grpcstatus.Error(codes.Unavailable, "TCP connection refused")
							}),
							grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
								err := invoker(ctx, method, req, reply, cc, opts...)
								if scenario == "background after deadline" {
									<-ctx.Done()
								}
								// The RPC already returned its TCP failure. A connection
								// started in the background can still finish a TLS handshake.
								conn, dialErr := node.listener.DialContext(t.Context())
								if dialErr != nil {
									t.Fatal(dialErr)
								}
								defer conn.Close()
								_, _, handshakeErr := creds.ClientHandshake(t.Context(), node.name, conn)
								sawRefusal = handshakeErr != nil && strings.Contains(handshakeErr.Error(), "unknown certificate authority")
								return err
							}),
						)
					}
					return node.dialWithTransport(
						ctx, endpoint, creds,
						grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
							if scenario == "unrelated failure" {
								return errors.New("connection reset")
							}
							err := invoker(ctx, method, req, reply, cc, opts...)
							if err != nil {
								sawRefusal = true
								if scenario == "late" {
									<-ctx.Done()
								}
							}
							return err
						}),
					)
				}

				target := managementRotationNode{name: node.name, endpoint: mustManagement(t, "192.0.2.11").Endpoint(), machineID: "machine-" + node.name}
				err = verifyRotatedManagement(t.Context(), target, mustManagementClient(t, next), node.authority, func() (managementidentity.CertificateKey, error) { return probe, nil }, time.Second)
				if scenario != "unrelated failure" && !sawRefusal {
					t.Fatal("the agent did not refuse the old credential")
				}
				switch scenario {
				case "timely":
					if err != nil {
						t.Fatalf("rotation rejected timely certificate refusal: %v", err)
					}
				case "late":
					if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "could not confirm the node stopped trusting the old authority") {
						t.Fatalf("rotation verification = %v, want an inconclusive deadline error", err)
					}
				default:
					if err == nil || !strings.Contains(err.Error(), "could not confirm the node stopped trusting the old authority; the node may still trust the old authority") {
						t.Fatalf("rotation verification = %v, want an unverified rejection", err)
					}
				}
			})
		})
	}
}

func TestManagementSSHPreflightLateResult(t *testing.T) {
	for _, operation := range []string{"rotate", "recover"} {
		t.Run(operation, func(t *testing.T) {
			testEnableMTLSWithVirtualTime(t, []string{"cp-1", "worker-1"}, func(t *testing.T, world enableMTLSWorld) {
				if _, stderr, err := world.enable(); err != nil {
					t.Fatalf("enable-mtls: %v\n%s", err, stderr)
				}
				before, err := os.ReadFile(world.configPath)
				if err != nil {
					t.Fatal(err)
				}
				var hosts []string
				previous := runManagementSSHCommand
				t.Cleanup(func() { runManagementSSHCommand = previous })
				runManagementSSHCommand = func(cmd *exec.Cmd) error {
					for _, arg := range cmd.Args {
						if strings.HasPrefix(arg, "root@") {
							hosts = append(hosts, arg)
						}
					}
					if len(hosts) > 1 {
						return errors.New("preflight reached the second node")
					}
					time.Sleep(time.Second)
					return json.NewEncoder(cmd.Stdout).Encode(map[string]string{
						"nodeName": "cp-1", "machineID": "machine-cp-1", "authority": world.nodes[0].authority,
					})
				}
				// The deadline has elapsed when Run returns, while cancellation
				// is still pending. The successful reply is otherwise usable.
				ctx := deadlineOnlyContext{Context: t.Context(), deadline: time.Now().Add(time.Second)}
				output := filepath.Join(filepath.Dir(world.configPath), "next.yaml")
				err = run(ctx, []string{"management", "identity", operation, "--config", world.configPath, "--output", output, "--timeout", "2s"}, &bytes.Buffer{}, &bytes.Buffer{})
				if !slices.Equal(hosts, []string{"root@192.0.2.11"}) {
					t.Errorf("%s attempted SSH to %v after a late reply from cp-1", operation, hosts)
				}
				for _, want := range []string{"could not confirm", "the node may still trust", "rerun the same command"} {
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("%s error = %v, missing %q", operation, err, want)
					}
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("%s error = %v, want deadline exceeded", operation, err)
				}
				after, readErr := os.ReadFile(world.configPath)
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatalf("%s changed ClusterConfig after a late preflight reply: %v", operation, readErr)
				}
			})
		})
	}
}

func mustManagementClient(t *testing.T, bundle managementidentity.Bundle) managementidentity.ClientCredentials {
	t.Helper()
	client, err := managementidentity.Client(bundle, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestManagementRotationPreflightLeavesInstalledTrustUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "no-context.yaml"))
	configPath := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(configPath, []byte(configBundleSource()), 0o644); err != nil {
		t.Fatal(err)
	}
	createTestManagementSecrets(t, configPath)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	previous := runManagementRotationSSH
	calledApply := false
	runManagementRotationSSH = func(_ context.Context, _ managementRotationNode, _ managementRotationOptions, dryRun bool) (nodeidentity.ManagementRotationStatus, error) {
		if dryRun {
			return nodeidentity.ManagementRotationStatus{}, errors.New("unknown SSH host key")
		}
		calledApply = true
		return nodeidentity.ManagementRotationStatus{}, nil
	}
	t.Cleanup(func() { runManagementRotationSSH = previous })
	outputPath := filepath.Join(dir, "next.yaml")
	err = run(context.Background(), []string{"management", "identity", "rotate", "--config", configPath, "--output", outputPath}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown SSH host key") {
		t.Fatalf("preflight error = %v", err)
	}
	if calledApply {
		t.Fatal("node changed after failed preflight")
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("config changed after failed preflight: %v", err)
	}
	if _, err := readManagementIdentity(outputPath); err != nil {
		t.Fatalf("replacement identity was not preserved for retry: %v", err)
	}
}

func TestEncryptedManagementRotationCandidate(t *testing.T) {
	for _, command := range []string{"sops", "age-keygen"} {
		workstationtest.Tool(t, command)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "age.key")
	if output, err := exec.Command("age-keygen", "-o", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("create age key: %v: %s", err, output)
	}
	recipient, err := exec.Command("age-keygen", "-y", keyPath).Output()
	if err != nil {
		t.Fatal(err)
	}
	config := "creation_rules:\n  - path_regex: ^management-secrets-next\\.sops\\.yaml$\n    age: " + strings.TrimSpace(string(recipient)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", keyPath)
	configPath := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(configPath, []byte(configBundleSource()), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(dir, "management-secrets-next.sops.yaml")
	if err := writeEncryptedManagementIdentity(configPath, outputPath, bundle); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := os.ReadFile(outputPath)
	if err != nil || !bytes.Contains(ciphertext, []byte("sops:")) || bytes.Contains(ciphertext, []byte(bundle.CertificateAuthority.PrivateKey)) {
		t.Fatalf("replacement was not stored only as SOPS ciphertext: %v", err)
	}
	loaded, err := readManagementIdentity(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := managementidentity.Validate(bundle, time.Now())
	got, _ := managementidentity.Validate(loaded, time.Now())
	if got.Fingerprint != want.Fingerprint {
		t.Fatal("encrypted replacement does not preserve the generated authority")
	}
}
