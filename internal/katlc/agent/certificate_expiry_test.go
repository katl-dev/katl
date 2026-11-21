package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestKubernetesStatusReportsSoonestKubeadmCertificate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root := t.TempDir()
	writeKubernetesStatusFile(t, root, "etc/hostname", "cp-1\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/admin.conf", kubeconfigExpiringAt(t, now.Add(200*24*time.Hour)))
	writeKubernetesStatusFile(t, root, "etc/kubernetes/pki/etcd/peer.crt", certificateExpiringAt(t, now.Add(90*24*time.Hour), false))
	writeKubernetesStatusFile(t, root, "etc/kubernetes/pki/apiserver.crt", certificateExpiringAt(t, now.Add(300*24*time.Hour), false))
	// Neither a CA nor an unreadable certificate can hide the soonest leaf.
	writeKubernetesStatusFile(t, root, "etc/kubernetes/pki/ca.crt", certificateExpiringAt(t, now.Add(24*time.Hour), true))
	writeKubernetesStatusFile(t, root, "etc/kubernetes/pki/front-proxy-client.crt", "not a certificate\n")

	status, err := nodeKubernetesStatus(context.Background(), root, func(context.Context, []string, func(int)) ToolResult {
		return ToolResult{ExitStatus: 1}
	})
	if err != nil {
		t.Fatal(err)
	}

	got := status.GetCertificateExpiry()
	if got.GetName() != "etcd-peer" || got.GetNotAfter() != now.Add(90*24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("certificate expiry = %v", got)
	}
}

func TestKubernetesStatusExpiredCertificate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name       string
		notAfter   time.Time
		failAPI    bool
		wantState  string
		wantReason string
	}{
		{name: "explains failed health", notAfter: now.Add(-time.Hour), failAPI: true, wantState: "waiting-for-control-plane", wantReason: "certificate apiserver expired at " + now.Add(-time.Hour).Format(time.RFC3339) + "; renew the control-plane certificates; see docs/operations/renew-certificates.md"},
		{name: "does not mask unrelated failure", notAfter: now.Add(24 * time.Hour), failAPI: true, wantState: "waiting-for-control-plane", wantReason: "local Kubernetes API or control-plane pods are not ready"},
		{name: "does not fail passing health", notAfter: now.Add(-time.Hour), wantState: "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubernetesStatusFile(t, root, "etc/hostname", "cp-1\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/admin.conf", kubeconfigExpiringAt(t, now.Add(365*24*time.Hour)))
			writeKubernetesStatusFile(t, root, "etc/kubernetes/pki/apiserver.crt", certificateExpiringAt(t, tc.notAfter, false))
			writeKubernetesStatusFile(t, root, "etc/kubernetes/manifests/kube-apiserver.yaml", `spec:
  containers:
    - name: kube-apiserver
      command:
        - kube-apiserver
        - --advertise-address=192.0.2.10
`)
			run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if tc.failAPI && strings.Contains(strings.Join(argv, " "), "--raw=/readyz") {
					return ToolResult{ExitStatus: 1}
				}
				return ToolResult{Stdout: []byte("True")}
			}

			status, err := nodeKubernetesStatus(context.Background(), root, run)
			if err != nil {
				t.Fatal(err)
			}

			if status.GetState() != tc.wantState || status.GetFailureReason() != tc.wantReason {
				t.Fatalf("state=%q reason=%q, want %q %q", status.GetState(), status.GetFailureReason(), tc.wantState, tc.wantReason)
			}
			if status.GetCertificateExpiry().GetName() != "apiserver" {
				t.Fatalf("certificate expiry = %v", status.GetCertificateExpiry())
			}
		})
	}
}

func TestKubernetesStatusOmitsCertificateExpiryOnWorkers(t *testing.T) {
	root := t.TempDir()
	writeKubernetesStatusFile(t, root, "etc/hostname", "worker-1\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/pki/ca.crt", certificateExpiringAt(t, time.Now().Add(time.Hour), true))

	status, err := nodeKubernetesStatus(context.Background(), root, func(context.Context, []string, func(int)) ToolResult {
		return ToolResult{Stdout: []byte("True")}
	})
	if err != nil {
		t.Fatal(err)
	}

	if status.GetState() != "ready" || status.GetCertificateExpiry() != nil {
		t.Fatalf("status = %v", status)
	}
}

func certificateExpiringAt(t *testing.T, notAfter time.Time, ca bool) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:              notAfter,
		IsCA:                  ca,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func kubeconfigExpiringAt(t *testing.T, notAfter time.Time) string {
	t.Helper()
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	return `apiVersion: v1
kind: Config
current-context: admin@kubernetes
clusters:
- name: kubernetes
  cluster:
    certificate-authority-data: ` + encode(certificateExpiringAt(t, notAfter, true)) + `
    server: https://192.0.2.10:6443
contexts:
- name: admin@kubernetes
  context:
    cluster: kubernetes
    user: admin
users:
- name: admin
  user:
    client-certificate-data: ` + encode(certificateExpiringAt(t, notAfter, false)) + `
    client-key-data: ` + encode("key") + `
`
}
