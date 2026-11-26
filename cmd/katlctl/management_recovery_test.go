package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// recoveryNode is an installed mTLS node whose management server restarts
// when its authority is replaced, with SSH replaced by direct calls.
type recoveryNode struct {
	name     string
	root     string
	endpoint string
	server   *grpc.Server
	listener net.Listener
	status   *agentapi.NodeStatus
	applied  int
	failOnce bool
}

func newRecoveryNode(t *testing.T, name string, installed managementidentity.Bundle) *recoveryNode {
	t.Helper()
	node := &recoveryNode{name: name, root: t.TempDir()}
	machineID, err := nodeidentity.WriteMachineID(node.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := nodeidentity.WriteEnrollment(node.root, name, machineID, nil)
	if err != nil {
		t.Fatal(err)
	}
	credentials, _, err := managementidentity.EnsureNode(&installed, name, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.WriteManagementIdentity(node.root, name, credentials); err != nil {
		t.Fatal(err)
	}
	node.status = enrolledStatus(name, enrollment.ID, machineID)
	node.start(t)
	t.Cleanup(func() { node.server.Stop(); node.listener.Close() })
	return node
}

func (n *recoveryNode) start(t *testing.T) {
	t.Helper()
	if n.server != nil {
		n.server.Stop()
		n.listener.Close()
	}
	tlsConfig, err := transport.ServerTLSConfig(n.root)
	if err != nil {
		t.Fatal(err)
	}
	address := n.endpoint
	if address == "" {
		address = "127.0.0.1:0"
	}
	n.listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	n.endpoint = n.listener.Addr().String()
	n.server = grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	agentapi.RegisterKatlcAgentServer(n.server, &clusterApplyIdentityServer{client: &fakeKatlcAgentClient{nodeStatus: n.status}})
	go func() { _ = n.server.Serve(n.listener) }()
}

// installRecoverySSH routes the SSH-backed node commands to the node each
// request names.
func installRecoverySSH(t *testing.T, nodes ...*recoveryNode) {
	t.Helper()
	byName := make(map[string]*recoveryNode, len(nodes))
	for _, n := range nodes {
		byName[n.name] = n
	}
	previousInspect, previousRotate := runManagementAuthoritySSH, runManagementRotationSSH
	runManagementAuthoritySSH = func(_ context.Context, node managementRotationNode, _ managementRotationOptions) (nodeidentity.ManagementAuthorityStatus, error) {
		return nodeidentity.InspectManagementAuthority(byName[node.name].root)
	}
	runManagementRotationSSH = func(_ context.Context, node managementRotationNode, _ managementRotationOptions, dryRun bool) (nodeidentity.ManagementRotationStatus, error) {
		n := byName[node.name]
		if dryRun {
			return nodeidentity.InspectManagementRotation(n.root, node.request)
		}
		if n.failOnce {
			n.failOnce = false
			return nodeidentity.ManagementRotationStatus{}, errors.New("connection reset")
		}
		n.applied++
		status, err := nodeidentity.RotateManagementIdentity(n.root, node.request)
		if err == nil {
			n.start(t)
		}
		return status, err
	}
	t.Cleanup(func() { runManagementAuthoritySSH, runManagementRotationSSH = previousInspect, previousRotate })
}

type recoveryWorld struct {
	configPath string
	secrets    string
	old        managementidentity.Bundle
	node       *recoveryNode
	nodes      []*recoveryNode
}

const recoveryWorkerNode = `    - name: wk-1
      management:
        address: 10.0.0.12
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-wk-root
`

// newRecoveryWorld installs a node under an authority whose secrets file is
// then deleted, as after a lost workstation.
func newRecoveryWorld(t *testing.T) recoveryWorld {
	return newRecoveryWorldOf(t, "cp-1")
}

func TestManagementIdentityRecoverLateCredentialCheck(t *testing.T) {
	testEnableMTLSWithVirtualTime(t, []string{"cp-1", "worker-1"}, func(t *testing.T, world enableMTLSWorld) {
		if _, stderr, err := world.enable(); err != nil {
			t.Fatalf("enable-mtls: %v\n%s", err, stderr)
		}
		output := filepath.Join(filepath.Dir(world.configPath), "next.yaml")
		if err := os.Rename(world.secretsPath, output); err != nil {
			t.Fatal(err)
		}
		// The node already uses the replacement authority, as on a rerun
		// after SSH succeeded and before the config reference was written.
		node := world.nodes[0]
		inspect, rotate := runManagementAuthoritySSH, runManagementRotationSSH
		t.Cleanup(func() { runManagementAuthoritySSH, runManagementRotationSSH = inspect, rotate })
		runManagementAuthoritySSH = func(_ context.Context, target managementRotationNode, _ managementRotationOptions) (nodeidentity.ManagementAuthorityStatus, error) {
			authority := node.authority
			if target.name != node.name {
				authority = "previous-authority"
			}
			return nodeidentity.ManagementAuthorityStatus{
				NodeName: target.name, MachineID: "machine-" + target.name, Authority: authority,
			}, nil
		}
		runManagementRotationSSH = func(_ context.Context, target managementRotationNode, _ managementRotationOptions, dryRun bool) (nodeidentity.ManagementRotationStatus, error) {
			if !dryRun {
				t.Errorf("recovery advanced to %s after an inconclusive result", target.name)
			}
			return nodeidentity.ManagementRotationStatus{}, nil
		}
		node.interceptRPC = delayManagementStatus

		var stdout, stderr bytes.Buffer
		err := run(t.Context(), []string{"management", "identity", "recover", "--config", world.configPath, "--output", output, "--timeout", "1s"}, &stdout, &stderr)
		if _, reference := world.configMode(t); reference != defaultManagementSecrets {
			t.Fatalf("recovery published %q after late credential verification", reference)
		}
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "rerun the same command") ||
			!strings.Contains(err.Error(), "could not confirm the node accepts the replacement authority; the node may still trust the previous authority") {
			t.Fatalf("recovery = %v\n%s\nwant a deadline error with rerun guidance", err, &stderr)
		}
	})
}

// newRecoveryWorldOf installs the named nodes (cp-1 and optionally wk-1)
// under one lost authority.
func newRecoveryWorldOf(t *testing.T, names ...string) recoveryWorld {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "no-context.yaml"))
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	configPath := filepath.Join(dir, "cluster.yaml")
	source := configBundleSource()
	if len(names) > 1 {
		source += recoveryWorkerNode
	}
	if err := os.WriteFile(configPath, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	createTestManagementSecrets(t, configPath)
	secrets := filepath.Join(dir, defaultManagementSecrets)
	old, err := readManagementIdentity(secrets)
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*recoveryNode
	created, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config := string(created)
	for i, name := range names {
		node := newRecoveryNode(t, name, old)
		nodes = append(nodes, node)
		config = strings.ReplaceAll(config, "10.0.0.1"+string(rune('1'+i)), node.endpoint)
	}
	installRecoverySSH(t, nodes...)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(secrets); err != nil {
		t.Fatal(err)
	}
	previousDial := dialKatlcAgent
	dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
		return dialKatlcAgentTCP(ctx, endpoint)
	}
	t.Cleanup(func() { dialKatlcAgent = previousDial })
	return recoveryWorld{configPath: configPath, secrets: secrets, old: old, node: nodes[0], nodes: nodes}
}

func (w recoveryWorld) recover(t *testing.T, extra ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"management", "identity", "recover", "--config", w.configPath, "--timeout", "3s"}, extra...)
	err := run(context.Background(), args, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func TestManagementIdentityRecoverReplacesLostAuthority(t *testing.T) {
	w := newRecoveryWorld(t)
	configBefore, err := os.ReadFile(w.configPath)
	if err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := w.recover(t)
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "Recovered management trust") || !strings.Contains(stdout, w.secrets) {
		t.Fatalf("recover output = %q", stdout)
	}

	// The node itself, not the command, reports which authority it trusts.
	next, err := readManagementIdentity(w.secrets)
	if err != nil {
		t.Fatalf("recovered secrets were not written at the referenced path: %v", err)
	}
	nextInfo, err := managementidentity.Validate(next, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	installed, err := nodeidentity.InspectManagementAuthority(w.node.root)
	if err != nil || installed.Authority != nextInfo.Fingerprint {
		t.Fatalf("node authority = %+v, %v; want %s", installed, err, nextInfo.Fingerprint)
	}
	oldInfo, err := managementidentity.Validate(w.old, time.Now())
	if err != nil || oldInfo.Fingerprint == nextInfo.Fingerprint {
		t.Fatalf("authority was not replaced: %v", err)
	}
	if err := checkManagementCredential(context.Background(), managementRotationNode{name: "cp-1", endpoint: w.node.endpoint}, mustManagementClient(t, next), 3*time.Second); err != nil {
		t.Fatalf("recovered credentials cannot manage the node: %v", err)
	}

	// Recovery must not widen access: the lost authority and an anonymous
	// client are both refused, and the node stays mTLS.
	node := managementRotationNode{name: "cp-1", endpoint: w.node.endpoint}
	if err := verifyOldManagementAuthorityRejected(context.Background(), node, next.CertificateAuthority.Certificate, w.old.Operator, 2*time.Second); err != nil {
		t.Fatalf("lost authority was not verifiably rejected: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(next.CertificateAuthority.Certificate))
	conn, err := tls.Dial("tcp", w.node.endpoint, &tls.Config{RootCAs: roots, ServerName: "cp-1", MinVersion: tls.VersionTLS13})
	if err == nil {
		// TLS 1.3 reports the missing client certificate on first use.
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err = conn.Read(make([]byte, 1))
		conn.Close()
	}
	var timeout net.Error
	if err == nil || (errors.As(err, &timeout) && timeout.Timeout()) {
		t.Fatalf("anonymous client was not refused: %v", err)
	}
	mode, err := nodeidentity.ManagementAuthentication(w.node.root)
	if err != nil || mode != managementidentity.MutualTLS {
		t.Fatalf("node mode = %q, %v", mode, err)
	}

	configAfter, err := os.ReadFile(w.configPath)
	if err != nil || !bytes.Equal(configBefore, configAfter) {
		t.Fatalf("config changed although the referenced path was reused: %v", err)
	}
	if info, err := os.Stat(w.secrets); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file mode = %v, %v", info, err)
	}
}

func TestManagementIdentityRecoverIsIdempotent(t *testing.T) {
	w := newRecoveryWorld(t)
	if _, stderr, err := w.recover(t); err != nil {
		t.Fatalf("recover: %v\n%s", err, stderr)
	}
	if w.node.applied != 1 {
		t.Fatalf("applied = %d, want 1", w.node.applied)
	}

	stdout, stderr, err := w.recover(t)
	if err != nil {
		t.Fatalf("repeat recover: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "nothing to recover") || w.node.applied != 1 {
		t.Fatalf("repeat output = %q, applied = %d", stdout, w.node.applied)
	}
}

func TestManagementIdentityRecoverResumesAfterFailure(t *testing.T) {
	w := newRecoveryWorld(t)
	w.node.failOnce = true

	_, _, err := w.recover(t)
	if err == nil || !strings.Contains(err.Error(), "connection reset") || !strings.Contains(err.Error(), "rerun the same command") {
		t.Fatalf("interrupted recover = %v", err)
	}
	kept, err := readManagementIdentity(w.secrets)
	if err != nil {
		t.Fatalf("replacement secrets were not kept for resume: %v", err)
	}
	trusted, err := nodeidentity.InspectManagementAuthority(w.node.root)
	if err != nil {
		t.Fatal(err)
	}
	oldInfo, err := managementidentity.Validate(w.old, time.Now())
	if err != nil || trusted.Authority != oldInfo.Fingerprint {
		t.Fatalf("node authority changed by a failed recover: %+v, %v", trusted, err)
	}

	if _, stderr, err := w.recover(t); err != nil {
		t.Fatalf("resume: %v\n%s", err, stderr)
	}
	resumed, err := readManagementIdentity(w.secrets)
	if err != nil || resumed.CertificateAuthority != kept.CertificateAuthority {
		t.Fatalf("resume generated another authority: %v", err)
	}
	trusted, err = nodeidentity.InspectManagementAuthority(w.node.root)
	keptInfo, _ := managementidentity.Validate(kept, time.Now())
	if err != nil || trusted.Authority != keptInfo.Fingerprint {
		t.Fatalf("node authority after resume = %+v, %v", trusted, err)
	}
}

func TestManagementIdentityRecoverWritesNothingWhenANodeIsUnreachable(t *testing.T) {
	w := newRecoveryWorld(t)
	runManagementAuthoritySSH = func(context.Context, managementRotationNode, managementRotationOptions) (nodeidentity.ManagementAuthorityStatus, error) {
		return nodeidentity.ManagementAuthorityStatus{}, errors.New("host key verification failed")
	}
	before, err := os.ReadFile(w.configPath)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = w.recover(t)
	if err == nil || !strings.Contains(err.Error(), "host key verification failed") || !strings.Contains(err.Error(), "no node credentials were changed") {
		t.Fatalf("recover = %v", err)
	}
	if _, err := os.Stat(w.secrets); !os.IsNotExist(err) {
		t.Fatalf("secrets were generated before every node was reached: %v", err)
	}
	if w.node.applied != 0 {
		t.Fatal("node changed although inspection failed")
	}
	after, err := os.ReadFile(w.configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("config changed: %v", err)
	}
}

func TestManagementIdentityRecoverRefusesTrustedNetwork(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "no-context.yaml"))
	configPath := filepath.Join(dir, "cluster.yaml")
	config := strings.Replace(configBundleSource(), "managementAuthentication: mtls", "managementAuthentication: trusted-network", 1)
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := runManagementAuthoritySSH
	t.Cleanup(func() { runManagementAuthoritySSH = previous })
	runManagementAuthoritySSH = func(context.Context, managementRotationNode, managementRotationOptions) (nodeidentity.ManagementAuthorityStatus, error) {
		t.Fatal("trusted-network config reached a node")
		return nodeidentity.ManagementAuthorityStatus{}, nil
	}

	err := run(context.Background(), []string{"management", "identity", "recover", "--config", configPath}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "trusted-network") {
		t.Fatalf("recover = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, defaultManagementSecrets)); !os.IsNotExist(err) {
		t.Fatalf("secrets were created: %v", err)
	}
}

func TestMissingManagementIdentityNamesRecover(t *testing.T) {
	err := missingManagementIdentity("lab", "/x/secrets.yaml")
	if !strings.Contains(err.Error(), "katlctl management identity recover --config") {
		t.Fatalf("error = %v", err)
	}
}

func TestManagementIdentityRecoverNamesOutputWhenSecretsUnreadable(t *testing.T) {
	w := newRecoveryWorld(t)
	if err := os.WriteFile(w.secrets, []byte("clusterName: lab\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := nodeidentity.InspectManagementAuthority(w.node.root)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = w.recover(t)
	if err == nil || !strings.Contains(err.Error(), w.secrets+" cannot be read") || !strings.Contains(err.Error(), "--output <new path>") {
		t.Fatalf("recover = %v", err)
	}
	after, err := nodeidentity.InspectManagementAuthority(w.node.root)
	if err != nil || after != before || w.node.applied != 0 {
		t.Fatalf("node changed: %+v, %v, applied %d", after, err, w.node.applied)
	}
}

func TestManagementIdentityRecoverReportsAuthoritySource(t *testing.T) {
	w := newRecoveryWorld(t)
	w.node.failOnce = true

	_, stderr, _ := w.recover(t)
	generated := regexp.MustCompile(`Generated new authority ([0-9a-f]{64}) at ` + regexp.QuoteMeta(w.secrets)).FindStringSubmatch(stderr)
	if generated == nil || strings.Index(stderr, "Generated new authority") > strings.Index(stderr, "Preflight") {
		t.Fatalf("first run stderr = %q", stderr)
	}

	_, stderr, err := w.recover(t)
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, stderr)
	}
	want := "Reusing existing secrets at " + w.secrets + " (authority " + generated[1] + ")"
	if !strings.Contains(stderr, want) || strings.Contains(stderr, "Generated new authority") {
		t.Fatalf("resume stderr = %q, want %q", stderr, want)
	}
}

func TestManagementIdentityRecoverRefusesChangedMachine(t *testing.T) {
	w := newRecoveryWorld(t)
	inspect := runManagementAuthoritySSH
	runManagementAuthoritySSH = func(ctx context.Context, node managementRotationNode, opts managementRotationOptions) (nodeidentity.ManagementAuthorityStatus, error) {
		status, err := inspect(ctx, node, opts)
		if err != nil {
			return status, err
		}
		// The node is reinstalled after it was inspected.
		other, err := nodeidentity.GenerateMachineID(nil)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(w.node.root, nodeidentity.EnrollmentPath)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.ReplaceAll(data, []byte(status.MachineID), []byte(other)), 0o444); err != nil {
			t.Fatal(err)
		}
		return status, nil
	}
	before, err := os.ReadFile(w.configPath)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = w.recover(t)
	if err == nil || !strings.Contains(err.Error(), "machine ID changed") || !strings.Contains(err.Error(), "no node credentials were changed") {
		t.Fatalf("recover = %v", err)
	}
	if w.node.applied != 0 {
		t.Fatal("node changed although its machine ID differs from the inspected install")
	}
	after, err := os.ReadFile(w.configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("config changed: %v", err)
	}
}

func TestManagementIdentityRecoverRefusesChangedAuthority(t *testing.T) {
	w := newRecoveryWorld(t)
	inspect := runManagementAuthoritySSH
	runManagementAuthoritySSH = func(ctx context.Context, node managementRotationNode, opts managementRotationOptions) (nodeidentity.ManagementAuthorityStatus, error) {
		status, err := inspect(ctx, node, opts)
		if err != nil {
			return status, err
		}
		// Another operator installs a different authority after inspection.
		other, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
		if err != nil {
			t.Fatal(err)
		}
		path, _, _, err := nodeidentity.ManagementCredentialsPaths(w.node.root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(other.CertificateAuthority.Certificate), 0o644); err != nil {
			t.Fatal(err)
		}
		return status, nil
	}

	_, _, err := w.recover(t)
	if err == nil || !strings.Contains(err.Error(), "differs from both expected and replacement") {
		t.Fatalf("recover = %v", err)
	}
	if w.node.applied != 0 {
		t.Fatal("node changed although its authority differs from the inspected one")
	}
}

func TestManagementIdentityRecoverResumesTwoNodes(t *testing.T) {
	w := newRecoveryWorldOf(t, "cp-1", "wk-1")
	a, b := w.nodes[0], w.nodes[1]
	b.failOnce = true
	output := filepath.Join(filepath.Dir(w.configPath), "next.yaml")
	before, err := os.ReadFile(w.configPath)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = w.recover(t, "--output", output)
	if err == nil || !strings.Contains(err.Error(), "wk-1") {
		t.Fatalf("interrupted recover = %v", err)
	}
	if a.applied != 1 || b.applied != 0 {
		t.Fatalf("applied = %d, %d; want 1, 0", a.applied, b.applied)
	}
	mid, err := os.ReadFile(w.configPath)
	if err != nil || !bytes.Equal(before, mid) {
		t.Fatalf("config rewritten before every node verified: %v", err)
	}

	if _, stderr, err := w.recover(t, "--output", output); err != nil {
		t.Fatalf("resume: %v\n%s", err, stderr)
	}
	if a.applied != 1 || b.applied != 1 {
		t.Fatalf("resume applied = %d, %d; want node A skipped and node B applied once", a.applied, b.applied)
	}
	next, err := readManagementIdentity(output)
	if err != nil {
		t.Fatal(err)
	}
	nextInfo, err := managementidentity.Validate(next, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range w.nodes {
		trusted, err := nodeidentity.InspectManagementAuthority(node.root)
		if err != nil || trusted.Authority != nextInfo.Fingerprint {
			t.Fatalf("%s authority = %+v, %v; want %s", node.name, trusted, err, nextInfo.Fingerprint)
		}
	}
	after, err := os.ReadFile(w.configPath)
	if err != nil || !strings.Contains(string(after), "managementIdentity: "+filepath.Base(output)) {
		t.Fatalf("config does not reference %s after recovery: %v\n%s", output, err, after)
	}
}

// fakeSSH replaces ssh on PATH with a script that records its arguments and
// prints the given output.
func fakeSSH(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	reply := filepath.Join(dir, "reply")
	if err := os.WriteFile(reply, []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argv + "\ncat " + reply + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argv
}

func TestRootSSHPinsHostKeys(t *testing.T) {
	argvPath := fakeSSH(t, `{"nodeName":"cp-1","machineID":"m","authority":"a"}`)
	node := managementRotationNode{name: "cp-1", host: "192.0.2.10"}

	_, err := sshManagementAuthority(context.Background(), node, managementRotationOptions{sshPort: 22, timeout: 5 * time.Second, knownHosts: "/keys/known_hosts"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	have := strings.Join(args, " ")
	for _, want := range []string{"StrictHostKeyChecking=yes", "BatchMode=yes", "UserKnownHostsFile=/keys/known_hosts", "GlobalKnownHostsFile=/dev/null"} {
		if !strings.Contains(have, want) {
			t.Errorf("ssh arguments %q lack %s", have, want)
		}
	}
	for _, arg := range args {
		if strings.Contains(arg, "accept-new") || arg == "StrictHostKeyChecking=no" {
			t.Errorf("ssh arguments %q weaken host key checking", have)
		}
	}
}

func TestSSHManagementAuthorityRefusesOtherNode(t *testing.T) {
	fakeSSH(t, `{"nodeName":"cp-9","machineID":"m","authority":"a"}`)

	_, err := sshManagementAuthority(context.Background(), managementRotationNode{name: "cp-1", host: "192.0.2.10"}, managementRotationOptions{sshPort: 22, timeout: 5 * time.Second})
	if err == nil || !strings.Contains(err.Error(), `SSH endpoint is node "cp-9", not cp-1`) {
		t.Fatalf("error = %v", err)
	}
}
