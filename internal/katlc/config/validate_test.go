package config

import (
	"reflect"
	"testing"
)

func TestValidateNodeConfigurationChangeDiagnostics(t *testing.T) {
	input := `
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: next-boot
spec:
  clusterDefaults:
    identity:
      hostname: Bad_Host
      authorizedKeys: []
    hostAccountPolicy: {}
    networkd:
      files:
        - name: ../escape.network
          content: ok
          renderer: unsupported
    kubernetes:
      kubeadm:
        configRef: missing
      sysext:
        activationPath: /run/extensions/kubernetes.raw
  nodeOverrides:
    cp-1:
      identity:
        authorizedKeys:
          - not-a-key
          - ssh-ed25519 not-a-real-public-key
    cp-1:
      identity:
        hostname: cp-1
`
	result := ValidateNodeConfigurationChange(input, Options{
		CheckKubeadmRefs: true,
		KubeadmConfigNames: map[string]struct{}{
			"control-plane": {},
		},
	})
	got := result.Strings()
	want := []string{
		`duplicate-node-name: spec.nodeOverrides.cp-1: node name "cp-1" is duplicated`,
		`invalid-hostname: spec.clusterDefaults.identity.hostname: "Bad_Host" must be a DNS hostname label`,
		`invalid-kubeadm-ref: spec.clusterDefaults.kubernetes.kubeadm.configRef: KubeadmConfig "missing" was not resolved`,
		`invalid-ssh-key: spec.nodeOverrides.cp-1.identity.authorizedKeys[0]: must be an SSH public key`,
		`invalid-ssh-key: spec.nodeOverrides.cp-1.identity.authorizedKeys[1]: must be an SSH public key`,
		`missing-ssh-key: spec.clusterDefaults.identity.authorizedKeys: authorizedKeys must contain at least one SSH public key`,
		`unsafe-render-path: spec.clusterDefaults.networkd.files[0].name: "../escape.network" must be a single render path segment`,
		`unsupported-activation-input: spec.clusterDefaults.kubernetes.sysext: direct Kubernetes sysext/confext activation input is not supported`,
		`unsupported-domain: spec.clusterDefaults.hostAccountPolicy: configuration domain is not supported`,
		`unsupported-field: spec.clusterDefaults.networkd.files[0].renderer: networkd file field is not supported`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics = %#v, want %#v", got, want)
	}
	if result.Accepted() {
		t.Fatal("Accepted() = true, want false")
	}
}

func TestValidateNodeConfigurationChangeAcceptsMinimalRuntimeConfig(t *testing.T) {
	input := `
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: next-boot
spec:
  clusterDefaults:
    identity:
      hostname: cp-2
      authorizedKeys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl
    networkd:
      files:
        - name: 20-uplink.network
          content: |
            [Match]
            Name=ens3
            [Network]
            DHCP=yes
    kubernetes:
      kubeadm:
        configRef: control-plane
`
	result := ValidateNodeConfigurationChange(input, Options{
		CheckKubeadmRefs: true,
		KubeadmConfigNames: map[string]struct{}{
			"control-plane": {},
		},
	})
	if !result.Accepted() {
		t.Fatalf("diagnostics = %#v, want accepted", result.Strings())
	}
}

func TestValidateNodeConfigurationChangeRejectsInvalidSysctlValues(t *testing.T) {
	input := `
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: live
spec:
  clusterDefaults:
    sysctl:
      settings:
        net.ipv4.ip_forward: "true"
        kernel.hostname: node-1
        vm.max_map_count: "0"
`
	result := ValidateNodeConfigurationChange(input, Options{})
	got := result.Strings()
	want := []string{
		`invalid-sysctl-value: spec.clusterDefaults.sysctl.settings.net.ipv4.ip_forward: expected 0 or 1`,
		`invalid-sysctl-value: spec.clusterDefaults.sysctl.settings.vm.max_map_count: expected a positive base-10 integer`,
		`unsupported-sysctl-key: spec.clusterDefaults.sysctl.settings.kernel.hostname: sysctl key is not supported`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics = %#v, want %#v", got, want)
	}
	if result.Accepted() {
		t.Fatal("Accepted() = true, want false")
	}
}
