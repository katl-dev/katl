package externalworker

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestBootstrap(t *testing.T) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, private)
	if err != nil {
		t.Fatal(err)
	}
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	valid := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: external\n  cluster:\n    server: https://api.lab.example:6443\n    certificate-authority-data: %s\nusers:\n- name: bootstrap\n  user:\n    token: test-restricted-bootstrap-token\ncontexts:\n- name: bootstrap\n  context:\n    cluster: external\n    user: bootstrap\ncurrent-context: bootstrap\n", ca)
	for _, tt := range []struct {
		name, data string
		valid      bool
	}{
		{"restricted token", valid, true},
		{"different cluster", strings.Replace(valid, "api.lab.example", "other.lab.example", 1), false},
		{"no CA", strings.Replace(valid, ca, "", 1), false},
		{"exec plugin", strings.Replace(valid, "token: test-restricted-bootstrap-token", "exec:\n      command: credential-helper", 1), false},
		{"administrator certificate", strings.Replace(valid, "token: test-restricted-bootstrap-token", "client-certificate-data: admin", 1), false},
		{"token file", strings.Replace(valid, "token: test-restricted-bootstrap-token", "tokenFile: /tmp/token", 1), false},
		{"insecure TLS", strings.Replace(valid, "server: https", "insecure-skip-tls-verify: true\n    server: https", 1), false},
		{"multiple documents", valid + "---\nkind: Config\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Bootstrap([]byte(tt.data), "api.lab.example:6443")
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v error=%v", tt.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "test-restricted-bootstrap-token") {
				t.Fatal("credential leaked in validation error")
			}
		})
	}
}

func TestKubeletConfig(t *testing.T) {
	data := []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nclusterDNS: [10.43.0.10]\nclusterDomain: home.internal\nmaxPods: 83\nserverTLSBootstrap: true\n")
	rendered, err := KubeletConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(rendered, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"clusterDomain": "home.internal", "maxPods": 83, "serverTLSBootstrap": true, "rotateCertificates": true, "cgroupDriver": "systemd", "containerRuntimeEndpoint": "unix:///run/containerd/containerd.sock"} {
		if got[key] != want {
			t.Errorf("%s=%v want=%v", key, got[key], want)
		}
	}
	dns := got["clusterDNS"].([]any)
	if len(dns) != 1 || dns[0] != "10.43.0.10" {
		t.Fatalf("clusterDNS=%v", dns)
	}
	if _, err := KubeletConfig([]byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\n")); err == nil {
		t.Fatal("accepted absent cluster networking")
	}
}

func TestEndpoint(t *testing.T) {
	for _, tt := range []struct {
		url   string
		valid bool
	}{
		{"https://api.lab.example:6443", true}, {"https://[fd00::1]:6443", true}, {"https://api.lab.example:443", true},
		{"http://api.lab.example:6443", false}, {"https://user:pass@api.lab.example:6443", false}, {"https://api.lab.example:6443/path", false}, {"https://api.lab.example", false}, {"https://api.lab.example:0", false},
	} {
		t.Run(tt.url, func(t *testing.T) {
			_, err := Endpoint(tt.url)
			if (err == nil) != tt.valid {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
