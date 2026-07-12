package configapply

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/generation"
)

func TestApplyNodeConfigurationChangeAcceptsLocalFileEnvelope(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyNodeConfigurationChange(context.Background(), strings.NewReader(`
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: next-boot
spec:
  clusterDefaults:
    networkd:
      files:
        - name: 10-common.network
          content: |
            [Match]
            Name=*
            [Network]
            DHCP=yes
  systemRoleOverrides:
    control-plane:
      identity:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl
  nodeOverrides:
    cp-1:
      identity:
        hostname: cp-1-renamed
`), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
	}))
	if err != nil {
		t.Fatalf("ApplyNodeConfigurationChange() error = %v", err)
	}
	if result.Audit.SourceID != "operator" || result.Audit.DesiredVersion != "2" {
		t.Fatalf("audit = %#v", result.Audit)
	}
	if result.Manifest.Node.Identity.Hostname != "cp-1-renamed" {
		t.Fatalf("hostname = %q", result.Manifest.Node.Identity.Hostname)
	}
	if !containsDomain(result.Plan.Decision.ChangedDomains, DomainNetworkd) || containsDomain(result.Plan.Decision.ChangedDomains, DomainSSHOperatorAccess) {
		t.Fatalf("changed domains = %#v", result.Plan.Decision.ChangedDomains)
	}
	if _, err := generation.ReadRecord(filepath.Join(root, "var/lib/katl/generations/2026.06.05-002/metadata.json")); err != nil {
		t.Fatalf("ReadRecord() error = %v", err)
	}
}

func TestDecodeNodeConfigurationChangeRejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown domain",
			body: `
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: next-boot
spec:
  clusterDefaults:
    hostAccountPolicy: {}
`,
			want: "field hostAccountPolicy not found",
		},
		{
			name: "unsupported known domain field",
			body: `
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: next-boot
spec:
  clusterDefaults:
    networkd:
      files:
        - name: 10-common.network
          content: ok
          renderer: unsupported
`,
			want: "field renderer not found",
		},
		{
			name: "unsupported sysext selection",
			body: `
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: next-boot
spec:
  clusterDefaults:
    kubernetes:
      sysext:
        payloadVersion: v1.36.1
`,
			want: "field sysext not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request, err := DecodeNodeConfigurationChange(strings.NewReader(tt.body), TrustedBundleRequest{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeNodeConfigurationChange() error = %v, want %q; request = %#v", err, tt.want, request)
			}
		})
	}
}
