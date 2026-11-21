package agent

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/kubeconfig"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

const certificateRenewalHint = "renew the control-plane certificates; see docs/operations/renew-certificates.md"

// kubeadmCertificates is the set that 'kubeadm certs renew all' renews, named
// as 'kubeadm certs check-expiration' reports them. CAs and kubelet
// certificates are excluded because that procedure does not renew them.
var kubeadmCertificates = []struct {
	name, path string
	kubeconfig bool
}{
	{name: "admin.conf", path: "/etc/kubernetes/admin.conf", kubeconfig: true},
	{name: "apiserver", path: "/etc/kubernetes/pki/apiserver.crt"},
	{name: "apiserver-etcd-client", path: "/etc/kubernetes/pki/apiserver-etcd-client.crt"},
	{name: "apiserver-kubelet-client", path: "/etc/kubernetes/pki/apiserver-kubelet-client.crt"},
	{name: "controller-manager.conf", path: "/etc/kubernetes/controller-manager.conf", kubeconfig: true},
	{name: "etcd-healthcheck-client", path: "/etc/kubernetes/pki/etcd/healthcheck-client.crt"},
	{name: "etcd-peer", path: "/etc/kubernetes/pki/etcd/peer.crt"},
	{name: "etcd-server", path: "/etc/kubernetes/pki/etcd/server.crt"},
	{name: "front-proxy-client", path: "/etc/kubernetes/pki/front-proxy-client.crt"},
	{name: "scheduler.conf", path: "/etc/kubernetes/scheduler.conf", kubeconfig: true},
	{name: "super-admin.conf", path: "/etc/kubernetes/super-admin.conf", kubeconfig: true},
}

type certificateExpiry struct {
	name     string
	notAfter time.Time
}

func (e certificateExpiry) report() *agentapi.CertificateExpiry {
	return &agentapi.CertificateExpiry{Name: e.name, NotAfter: e.notAfter.UTC().Format(time.RFC3339)}
}

// soonestKubeadmCertificate reads certificates without failing: status must
// remain available when kubeadm state is absent, partial, or unreadable.
func soonestKubeadmCertificate(root string) (certificateExpiry, bool) {
	var soonest certificateExpiry
	found := false
	for _, candidate := range kubeadmCertificates {
		certificate, err := readKubeadmCertificate(rootedRuntimePath(root, candidate.path), candidate.kubeconfig)
		if err != nil {
			continue
		}
		if !found || certificate.NotAfter.Before(soonest.notAfter) {
			soonest, found = certificateExpiry{name: candidate.name, notAfter: certificate.NotAfter}, true
		}
	}
	return soonest, found
}

func readKubeadmCertificate(path string, embedded bool) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if embedded {
		credentials, err := kubeconfig.ParseCredentials(data)
		if err != nil {
			return nil, err
		}
		return credentials.ClientCertificate()
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, os.ErrInvalid
	}
	return x509.ParseCertificate(block.Bytes)
}
