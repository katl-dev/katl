package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Earlier releases accepted a management.address that is not a DNS name, such
// as one with an underscore, and resolvers commonly resolve such names, so an
// existing ClusterConfig that uses one keeps working with a warning. A value
// that cannot name a host is still refused.
func TestConfiguredManagementAddress(t *testing.T) {
	write := func(t *testing.T, workerAddress string) string {
		t.Helper()
		node := func(name, controlPlane, address string) string {
			return "    - name: " + name + "\n      controlPlane: " + controlPlane + "\n      management:\n        address: " + address + "\n" +
				"      install:\n        systemDisk:\n          byID: /dev/disk/by-id/ata-" + name + "\n"
		}
		source := "apiVersion: config.katl.dev/v1alpha1\nkind: ClusterConfig\nmetadata:\n  name: lab\nspec:\n  managementAuthentication: trusted-network\n" +
			"  kubernetes:\n    version: v1.36.1\n  defaults:\n    install:\n      systemDisk:\n        minSizeMiB: 32768\n    access:\n      ssh:\n        authorizedKeys:\n          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example\n" +
			"  nodes:\n" + node("cp-1", "true", "10.0.0.11") + node("worker-1", "false", workerAddress)
		path := filepath.Join(t.TempDir(), "cluster.yaml")
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	for _, tc := range []struct {
		address  string
		endpoint string
	}{
		{address: "worker_1.home.arpa", endpoint: "worker_1.home.arpa:9443"},
		{address: "worker_1.home.arpa:19443", endpoint: "worker_1.home.arpa:19443"},
	} {
		t.Run("warns on "+tc.address, func(t *testing.T) {
			path := write(t, tc.address)

			var stderr bytes.Buffer
			if err := run(context.Background(), []string{"config", "bundle", path, "--output", filepath.Join(t.TempDir(), "cluster.katlcfg")}, &bytes.Buffer{}, &stderr); err != nil {
				t.Fatalf("config bundle error = %v\nstderr=%s", err, stderr.String())
			}
			for _, want := range []string{`spec.nodes["worker-1"].management.address`, `"worker_1.home.arpa"`, "not a valid DNS hostname"} {
				if !strings.Contains(stderr.String(), "warning: ") || !strings.Contains(stderr.String(), want) {
					t.Errorf("config bundle stderr = %q, want a warning naming %q", stderr.String(), want)
				}
			}

			inv, err := readManagementInventory(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := inv.Nodes[1].ManagementEndpoint().String(); got != tc.endpoint {
				t.Errorf("worker-1 management endpoint = %q, want %q", got, tc.endpoint)
			}
		})
	}

	t.Run("no warning for a DNS name", func(t *testing.T) {
		path := write(t, "worker-1.home.arpa")

		var stderr bytes.Buffer
		if err := run(context.Background(), []string{"config", "bundle", path, "--output", filepath.Join(t.TempDir(), "cluster.katlcfg")}, &bytes.Buffer{}, &stderr); err != nil {
			t.Fatalf("config bundle error = %v\nstderr=%s", err, stderr.String())
		}
		if strings.Contains(stderr.String(), "management.address") {
			t.Errorf("config bundle stderr = %q, want no management.address warning", stderr.String())
		}
	})

	for _, address := range []string{`"worker 1.home.arpa"`, "worker/1.home.arpa", "worker..home.arpa", "http://worker-1.home.arpa", "fe80::1%eth0", "worker-1.home.arpa:0"} {
		t.Run("refuses "+address, func(t *testing.T) {
			path := write(t, address)

			if _, err := readManagementInventory(path); err == nil || !strings.Contains(err.Error(), `node "worker-1" management.address`) {
				t.Errorf("katlctl error = %v, want it to name worker-1's management.address", err)
			}
			err := run(context.Background(), []string{"config", "bundle", path, "--output", filepath.Join(t.TempDir(), "cluster.katlcfg")}, &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "management.address") {
				t.Errorf("config bundle error = %v, want it to name management.address", err)
			}
		})
	}
}
