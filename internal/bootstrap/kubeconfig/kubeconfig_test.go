package kubeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	testCA   = "Y2EtZGF0YQ=="
	testCert = "Y2VydC1kYXRh"
	testKey  = "a2V5LWRhdGE="
)

func TestWriteDefaultPathAndMode(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldwd)

	result, err := Write(validRequest(""))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if result.Path != DefaultPath {
		t.Fatalf("path = %q, want %q", result.Path, DefaultPath)
	}
	if result.Server != "https://10.0.0.10:6443" {
		t.Fatalf("server = %q", result.Server)
	}
	if !result.Written || result.Overwritten || result.Idempotent {
		t.Fatalf("unexpected result flags: %#v", result)
	}
	info, err := os.Stat(DefaultPath)
	if err != nil {
		t.Fatalf("stat kubeconfig: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %v, want 0600", got)
	}
	assertKubeconfigServer(t, DefaultPath, "https://10.0.0.10:6443")
}

func TestWriteUsesExplicitOutputAndCreatesParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "operator.conf")
	result, err := Write(validRequest(path))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if result.Path != path {
		t.Fatalf("path = %q, want %q", result.Path, path)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if got := parent.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent mode = %v, want 0700", got)
	}
	assertKubeconfigServer(t, path, "https://10.0.0.10:6443")
}

func TestEndpointSelection(t *testing.T) {
	tests := []struct {
		name      string
		selection EndpointSelection
		want      string
	}{
		{
			name: "initial endpoint",
			selection: EndpointSelection{
				InitialEndpoint: "10.0.0.10:6443",
			},
			want: "https://10.0.0.10:6443",
		},
		{
			name: "control plane endpoint",
			selection: EndpointSelection{
				InitialEndpoint:      "10.0.0.10:6443",
				ControlPlaneEndpoint: "api.katl.test:6443",
			},
			want: "https://api.katl.test:6443",
		},
		{
			name: "stable endpoint after handoff",
			selection: EndpointSelection{
				InitialEndpoint:      "10.0.0.10:6443",
				ControlPlaneEndpoint: "api-bootstrap.katl.test:6443",
				StableEndpoint:       "https://api.katl.test:6443",
				StableEndpointReady:  true,
			},
			want: "https://api.katl.test:6443",
		},
		{
			name: "stable endpoint waits for readiness",
			selection: EndpointSelection{
				InitialEndpoint:      "10.0.0.10:6443",
				ControlPlaneEndpoint: "api-bootstrap.katl.test:6443",
				StableEndpoint:       "api.katl.test:6443",
			},
			want: "https://api-bootstrap.katl.test:6443",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SelectServer(tt.selection)
			if err != nil {
				t.Fatalf("SelectServer() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("SelectServer() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteRefusesDifferentExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.conf")
	if err := os.WriteFile(path, []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Write(validRequest(path))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("Write() error = %v, want ErrExists", err)
	}
}

func TestWriteAllowsIdempotentExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.conf")
	content, err := Render(validRender("https://10.0.0.10:6443"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Write(validRequest(path))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !result.Idempotent || result.Written || result.Overwritten {
		t.Fatalf("unexpected result flags: %#v", result)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %v, want 0600", got)
	}
}

func TestWriteOverwritesDifferentExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.conf")
	if err := os.WriteFile(path, []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := validRequest(path)
	request.Overwrite = true
	result, err := Write(request)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !result.Written || !result.Overwritten || result.Idempotent {
		t.Fatalf("unexpected result flags: %#v", result)
	}
	assertKubeconfigServer(t, path, "https://10.0.0.10:6443")
}

func TestResultDoesNotExposeCredentialMaterial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.conf")
	result, err := Write(validRequest(path))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	rendered := result.NextStep()
	for _, secret := range []string{testCA, testCert, testKey} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("NextStep() exposed credential material")
		}
	}
	want := []string{"kubectl", "--kubeconfig", path, "get", "nodes"}
	if got := result.KubectlArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("KubectlArgs() = %#v, want %#v", got, want)
	}
}

func TestSelectServerRejectsMissingOrNonHTTPS(t *testing.T) {
	if _, err := SelectServer(EndpointSelection{}); err == nil {
		t.Fatal("SelectServer() error = nil, want missing endpoint error")
	}
	if _, err := SelectServer(EndpointSelection{InitialEndpoint: "http://api.katl.test:6443"}); err == nil {
		t.Fatal("SelectServer() error = nil, want non-https rejection")
	}
	if _, err := SelectServer(EndpointSelection{InitialEndpoint: "https://user:pass@api.katl.test:6443"}); err == nil {
		t.Fatal("SelectServer() error = nil, want userinfo rejection")
	}
	if _, err := SelectServer(EndpointSelection{InitialEndpoint: "https://api.katl.test"}); err == nil {
		t.Fatal("SelectServer() error = nil, want missing port rejection")
	}
}

func validRequest(path string) Request {
	request := Request{
		Path: path,
		Endpoint: EndpointSelection{
			InitialEndpoint: "10.0.0.10:6443",
		},
		ClusterName:              "katl-test",
		ContextName:              "katl-test",
		UserName:                 "katl-admin",
		CertificateAuthorityData: testCA,
		ClientCertificateData:    testCert,
		ClientKeyData:            testKey,
	}
	return request
}

func validRender(server string) RenderRequest {
	return RenderRequest{
		Server:                   server,
		ClusterName:              "katl-test",
		ContextName:              "katl-test",
		UserName:                 "katl-admin",
		CertificateAuthorityData: testCA,
		ClientCertificateData:    testCert,
		ClientKeyData:            testKey,
	}
}

func assertKubeconfigServer(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}
	var decoded struct {
		Clusters []struct {
			Cluster struct {
				Server string `yaml:"server"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal kubeconfig: %v", err)
	}
	if len(decoded.Clusters) != 1 {
		t.Fatalf("clusters len = %d, want 1", len(decoded.Clusters))
	}
	if got := decoded.Clusters[0].Cluster.Server; got != want {
		t.Fatalf("server = %q, want %q", got, want)
	}
}
