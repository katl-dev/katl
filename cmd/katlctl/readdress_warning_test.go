package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

func TestWarnPendingReaddress(t *testing.T) {
	const worker = "worker-1"
	for _, test := range []struct {
		name string
		// configured is worker-1's management.address in the configuration.
		configured string
		// saved is the endpoint the saved context records; baseline is the
		// management.address it recorded when the context was saved.
		saved, baseline string
		endpoints       map[string]string
		selected        []string
		wantWarning     bool
	}{
		{name: "planned move", configured: "10.0.0.23", saved: "10.0.0.13:9443", baseline: "10.0.0.13:9443", wantWarning: true},
		{name: "configuration edited again after a rebind", configured: "10.0.0.23", saved: "10.0.0.33:9443", baseline: "10.0.0.13:9443", wantWarning: true},
		{name: "rebind without editing the configuration", configured: "10.0.0.13", saved: "10.0.0.23:9443", baseline: "10.0.0.13:9443"},
		{name: "node reached at its configured address", configured: "10.0.0.23", saved: "10.0.0.23:9443", baseline: "10.0.0.13:9443"},
		{name: "explicit endpoint", configured: "10.0.0.23", saved: "10.0.0.13:9443", baseline: "10.0.0.13:9443", endpoints: map[string]string{worker: "10.0.0.13:9443"}},
		{name: "node not selected", configured: "10.0.0.23", saved: "10.0.0.13:9443", baseline: "10.0.0.13:9443", selected: []string{"cp-1"}},
		{name: "context without a recorded configuration", configured: "10.0.0.23", saved: "10.0.0.13:9443"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeMovedWorkerConfig(t, "")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			source := strings.Replace(string(data), "managementAuthentication: trusted-network", "managementAuthentication: mtls", 1)
			source = strings.Replace(source, "address: 10.0.0.23", "address: "+test.configured, 1)
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			saved := workstation.Config{}.UpsertCluster("lab", workstation.Cluster{Name: "lab", Nodes: []workstation.Node{
				{Name: "cp-1", ManagementEndpoint: "10.0.0.11:9443", ConfiguredEndpoint: "10.0.0.11:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine-cp-1"},
				{Name: worker, ManagementEndpoint: test.saved, ConfiguredEndpoint: test.baseline, SystemRole: inventory.RoleWorker, EnrollmentID: "enrollment-worker-1", MachineID: "machine-worker-1"},
			}})
			if err := workstation.Save(os.Getenv("KATLCTL_CONFIG"), saved); err != nil {
				t.Fatal(err)
			}

			var stderr bytes.Buffer
			endpoints := map[string]inventory.HostPort{}
			for name, endpoint := range test.endpoints {
				endpoints[name] = mustHostPort(t, endpoint)
			}
			warnPendingReaddress(context.Background(), path, endpoints, test.selected, &stderr)

			got := stderr.String()
			if !test.wantWarning {
				if got != "" {
					t.Fatalf("stderr = %q, want no warning", got)
				}
				return
			}
			if n := strings.Count(got, "warning:"); n != 1 {
				t.Fatalf("stderr has %d warnings, want 1:\n%s", n, got)
			}
			oldHost := strings.TrimSuffix(test.saved, ":9443")
			for _, want := range []string{
				worker,
				"management.address " + test.configured,
				oldHost,
				"katlctl node reboot worker-1 --config " + path + " --endpoint " + oldHost,
				"katlctl context rebind --node worker-1 --endpoint " + test.configured,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("warning lacks %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestClusterApplyWarnsOfPendingReaddress(t *testing.T) {
	path := writeMovedWorkerConfig(t, "")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), "managementAuthentication: trusted-network", "managementAuthentication: mtls", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := workstation.Config{}.UpsertCluster("lab", workstation.Cluster{Name: "lab", Nodes: []workstation.Node{
		{Name: "cp-1", ManagementEndpoint: "10.0.0.11:9443", ConfiguredEndpoint: "10.0.0.11:9443", SystemRole: inventory.RoleControlPlane},
		{Name: "worker-1", ManagementEndpoint: workerCurrentEndpoint, ConfiguredEndpoint: workerCurrentEndpoint, SystemRole: inventory.RoleWorker, EnrollmentID: "enrollment-worker-1", MachineID: "machine-worker-1"},
	}})
	if err := workstation.Save(os.Getenv("KATLCTL_CONFIG"), saved); err != nil {
		t.Fatal(err)
	}
	worker := movedWorkerClient("agent-1")
	installKatlcDial(t, nil, worker)

	var stderr bytes.Buffer
	_ = run(context.Background(), []string{"cluster", "apply", "--plan", "--config", path, "--node", "worker-1"}, &bytes.Buffer{}, &stderr)

	want := "warning: worker-1 is configured at management.address 10.0.0.23, but the saved context reaches it at 10.0.0.13"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}
