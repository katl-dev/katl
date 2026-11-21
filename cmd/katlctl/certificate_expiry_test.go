package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

var certificateTestNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func fixCertificateNow(t *testing.T) {
	t.Helper()
	previous := certificateNow
	certificateNow = func() time.Time { return certificateTestNow }
	t.Cleanup(func() { certificateNow = previous })
}

func TestNodeStatusShowsCertificateExpiry(t *testing.T) {
	fixCertificateNow(t)
	for _, tc := range []struct {
		name      string
		notAfter  time.Time
		wantLine  string
		wantState string
	}{
		{name: "ok", notAfter: certificateTestNow.Add(100 * 24 * time.Hour), wantLine: "certificate apiserver expires on 2027-03-01 (in 100 days)\n", wantState: "ok"},
		{name: "expiring", notAfter: certificateTestNow.Add(30 * 24 * time.Hour), wantLine: "certificate apiserver expires on 2026-12-21 (in 30 days); renew the control-plane certificates; see docs/operations/renew-certificates.md\n", wantState: "expiring"},
		{name: "expired", notAfter: certificateTestNow.Add(-time.Second), wantLine: "certificate apiserver expired on 2026-11-21; renew the control-plane certificates; see docs/operations/renew-certificates.md\n", wantState: "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := healthyHostClient("machine-a", "agent-a", "generation-0")
			fake.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{
				State: "ready", Role: "control-plane", NodeName: "node-a", KubeletActive: true, NodeReady: true, ControlPlaneComponentsReady: true,
				CertificateExpiry: &agentapi.CertificateExpiry{Name: "apiserver", NotAfter: tc.notAfter.Format(time.RFC3339)},
			}
			installKatlcDial(t, nil, fake)

			var text, stderr bytes.Buffer
			if err := run(context.Background(), []string{"node", "status", "node-a", "--endpoint", "node-a.test:9443"}, &text, &stderr); err != nil {
				t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
			}
			if !strings.Contains(text.String(), tc.wantLine) {
				t.Fatalf("output missing %q:\n%s", tc.wantLine, text.String())
			}

			var encoded bytes.Buffer
			if err := run(context.Background(), []string{"node", "status", "node-a", "--endpoint", "node-a.test:9443", "--output", "json"}, &encoded, &stderr); err != nil {
				t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
			}
			var report struct {
				Kubernetes struct {
					CertificateExpiry certificateExpiryReport `json:"certificateExpiry"`
				} `json:"kubernetes"`
			}
			if err := json.Unmarshal(encoded.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if got := report.Kubernetes.CertificateExpiry; got.Name != "apiserver" || got.NotAfter != tc.notAfter.Format(time.RFC3339) || got.State != tc.wantState {
				t.Fatalf("certificate expiry = %#v", got)
			}
		})
	}
}

func TestClusterStatusWarnsOnlyNearCertificateExpiry(t *testing.T) {
	fixCertificateNow(t)
	contextPath := writeKatlctlConfig(t, `currentContext: lab
contexts:
- name: lab
  cluster: lab
clusters:
- name: lab
  nodes:
  - name: cp-1
    managementEndpoint: 192.0.2.11:9443
    systemRole: control-plane
  - name: cp-2
    managementEndpoint: 192.0.2.12:9443
    systemRole: control-plane
`)
	clientFor := func(name string, notAfter time.Time) *fakeKatlcAgentClient {
		client := healthyHostClient("machine-"+name, "agent-"+name, "generation-1")
		client.nodeStatus.InventoryNodeName = name
		client.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{
			State: "ready", Role: "control-plane", NodeName: name, KubeletActive: true, NodeReady: true, ControlPlaneComponentsReady: true,
			CertificateExpiry: &agentapi.CertificateExpiry{Name: "admin.conf", NotAfter: notAfter.Format(time.RFC3339)},
		}
		return client
	}
	clients := map[string]*fakeKatlcAgentClient{
		"192.0.2.11:9443": clientFor("cp-1", certificateTestNow.Add(200*24*time.Hour)),
		"192.0.2.12:9443": clientFor("cp-2", certificateTestNow.Add(5*24*time.Hour)),
	}
	previous := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: clients[endpoint], Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = previous })

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "status", "--context-file", contextPath}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}

	output := stdout.String()
	if want := "certificate admin.conf expires on 2026-11-26 (in 5 days); renew the control-plane certificates"; !strings.Contains(output, want) {
		t.Fatalf("output missing %q:\n%s", want, output)
	}
	if strings.Contains(output, "2027-06-09") {
		t.Fatalf("output reports a distant expiry as a warning:\n%s", output)
	}
}

func TestClusterKubeconfigRefusesExpiredAdminCredentials(t *testing.T) {
	fixCertificateNow(t)
	nodes := []workstation.Node{
		{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine-cp-1"},
		{Name: "cp-2", ManagementEndpoint: "192.0.2.2:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-2", MachineID: "machine-cp-2"},
	}
	clusterConfig, _ := writeKubernetesUpgradeConfig(t, "v1.36.1", nodes...)
	clientFor := func(name string, notAfter time.Time) *fakeKatlcAgentClient {
		return &fakeKatlcAgentClient{
			nodeStatus:         &agentapi.NodeStatus{MachineId: "machine-" + name, EnrollmentId: "enrollment-" + name, InventoryNodeName: name, CurrentGenerationId: "gen-1"},
			kubeconfigResponse: &agentapi.KubeconfigResponse{Kubeconfig: []byte(adminKubeconfigExpiringAt(t, notAfter))},
		}
	}
	clients := map[string]*fakeKatlcAgentClient{
		"192.0.2.1:9443": clientFor("cp-1", certificateTestNow.Add(-time.Hour)),
		"192.0.2.2:9443": clientFor("cp-2", certificateTestNow.Add(300*24*time.Hour)),
	}
	previous := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: clients[endpoint], Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = previous })
	path := filepath.Join(t.TempDir(), "kubeconfig")

	err := run(context.Background(), []string{"cluster", "kubeconfig", path, "--config", clusterConfig, "--node", "cp-1"}, &bytes.Buffer{}, &bytes.Buffer{})
	if want := "cp-1: certificate admin.conf expired on 2026-11-21; renew the control-plane certificates; see docs/operations/renew-certificates.md"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expired credentials were written: %v", statErr)
	}

	if err := run(context.Background(), []string{"cluster", "kubeconfig", path, "--config", clusterConfig}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("fallback to cp-2: %v", err)
	}
}

func TestKubernetesUpgradeRefusesExpiredCertificates(t *testing.T) {
	fixCertificateNow(t)
	clusterConfig, contextPath := writeKubernetesUpgradeConfig(t, "v1.36.1", workstation.Node{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine"})
	client := healthyKubernetesUpgradeClient()
	client.generation.Sysexts[0].PayloadVersion = "v1.36.0"
	client.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{
		State: "waiting-for-control-plane", Role: "control-plane", NodeName: "cp-1", KubeletActive: true,
		CertificateExpiry: &agentapi.CertificateExpiry{Name: "admin.conf", NotAfter: certificateTestNow.Add(-24 * time.Hour).Format(time.RFC3339)},
	}
	installKatlcDial(t, nil, client)

	err := run(context.Background(), []string{"kubernetes", "upgrade", "--config", clusterConfig, "--context-file", contextPath, "--plan", "--timeout", "1m"}, &bytes.Buffer{}, &bytes.Buffer{})

	if want := "node cp-1 cannot be upgraded: certificate admin.conf expired on 2026-11-20; renew the control-plane certificates"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if len(client.submitRequests) != 0 {
		t.Fatalf("upgrade submitted operations: %#v", client.submitRequests)
	}
}

func adminKubeconfigExpiringAt(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kubernetes-admin"}, NotBefore: notAfter.Add(-365 * 24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(block *pem.Block) string { return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(block)) }
	certificate := encode(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return `apiVersion: v1
kind: Config
current-context: admin@kubernetes
clusters:
- name: kubernetes
  cluster:
    certificate-authority-data: ` + certificate + `
    server: https://192.0.2.1:6443
contexts:
- name: admin@kubernetes
  context:
    cluster: kubernetes
    user: admin
users:
- name: admin
  user:
    client-certificate-data: ` + certificate + `
    client-key-data: ` + encode(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}) + `
`
}
