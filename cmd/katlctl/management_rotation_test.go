package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nettest"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestManagementIdentityRotateReplacesLiveTrust(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "no-context.yaml"))
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	configPath := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(configPath, []byte(configBundleSource()), 0o644); err != nil {
		t.Fatal(err)
	}
	createTestManagementSecrets(t, configPath)
	oldPath := filepath.Join(dir, defaultManagementSecrets)
	old, err := readManagementIdentity(oldPath)
	if err != nil {
		t.Fatal(err)
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
	var allowOldClient bool
	start := func() {
		t.Helper()
		if server != nil {
			server.Stop()
			listener.Close()
		}
		tlsConfig, err := transport.ServerTLSConfig(root)
		if err != nil {
			t.Fatal(err)
		}
		if allowOldClient {
			if !tlsConfig.ClientCAs.AppendCertsFromPEM([]byte(old.CertificateAuthority.Certificate)) {
				t.Fatal("add old authority to deliberately unsafe test server")
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
		agentapi.RegisterKatlcAgentServer(server, &clusterApplyIdentityServer{client: &fakeKatlcAgentClient{nodeStatus: enrolledStatus("cp-1", enrollment.ID, machineID)}})
		go func() { _ = server.Serve(listener) }()
	}
	start()
	t.Cleanup(func() { server.Stop(); listener.Close() })
	configured, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(strings.ReplaceAll(string(configured), "10.0.0.11", endpoint)), 0o600); err != nil {
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
	runManagementRotationSSH = func(_ context.Context, target managementRotationNode, _ managementRotationOptions, dryRun bool) (nodeidentity.ManagementRotationStatus, error) {
		if dryRun {
			return nodeidentity.InspectManagementRotation(root, target.request)
		}
		result, err := nodeidentity.RotateManagementIdentity(root, target.request)
		if err == nil {
			start()
			transientDialFailures = 1
		}
		return result, err
	}
	t.Cleanup(func() { runManagementRotationSSH = previousSSH })
	outputPath := filepath.Join(dir, "management-secrets-next.yaml")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"management", "identity", "rotate", "--config", configPath, "--output", outputPath, "--timeout", "3s"}, &stdout, &stderr); err != nil {
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
	if err := verifyRotatedManagement(context.Background(), managementRotationNode{name: "cp-1", endpoint: endpoint, machineID: machineID}, mustManagementClient(t, next), mustManagementClient(t, old), 3*time.Second); err != nil {
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
	allowOldClient = true
	start()
	if err := verifyRotatedManagement(context.Background(), managementRotationNode{name: "cp-1", endpoint: endpoint, machineID: machineID}, mustManagementClient(t, next), mustManagementClient(t, old), 3*time.Second); err == nil || !strings.Contains(err.Error(), "exposed authority still has management access") {
		t.Fatalf("server that still trusts exposed client was accepted: %v", err)
	}
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
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), output) {
			t.Fatalf("rotation = %v\n%s\nwant a deadline error and guidance to keep the replacement secrets", err, &stderr)
		}
		if _, reference := world.configMode(t); reference != defaultManagementSecrets {
			t.Fatalf("rotation published %q after late credential verification", reference)
		}
	})
}

func TestManagementRotationExposedCredentialRefusal(t *testing.T) {
	for _, scenario := range []string{"timely", "late", "unrelated failure"} {
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
				node := world.nodes[0]
				dial := dialKatlcAgentWithTransport
				t.Cleanup(func() { dialKatlcAgentWithTransport = dial })
				sawRefusal := false
				dialKatlcAgentWithTransport = func(ctx context.Context, _, endpoint string, creds credentials.TransportCredentials) (katlcAgentConnection, error) {
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

				target := managementRotationNode{name: node.name, endpoint: "192.0.2.11:9443", machineID: "machine-" + node.name}
				err = verifyRotatedManagement(t.Context(), target, mustManagementClient(t, next), mustManagementClient(t, old), time.Second)
				if scenario != "unrelated failure" && !sawRefusal {
					t.Fatal("the agent did not refuse the old credential")
				}
				switch scenario {
				case "timely":
					if err != nil {
						t.Fatalf("rotation rejected timely certificate refusal: %v", err)
					}
				case "late":
					if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "rerun with the same --output") {
						t.Fatalf("rotation verification = %v, want a deadline error and rerun guidance", err)
					}
				default:
					if err == nil || !strings.Contains(err.Error(), "could not verify rejection") {
						t.Fatalf("rotation verification = %v, want an unverified rejection", err)
					}
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
		if _, err := exec.LookPath(command); err != nil {
			t.Skip("requires SOPS and age")
		}
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
