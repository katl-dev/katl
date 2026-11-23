package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

// The fixtures in testdata/v2026.11.1 were written by katlctl v2026.11.1:
// katlctl.yaml by 'context save' and cluster.katlcfg by 'config build', from a
// ClusterConfig whose management addresses are hostnames with underscores,
// which that release accepted. The saved context also holds the
// '[host:port]:9443' endpoint that release saved for a management.address
// naming a port. Data a release has saved must keep loading after upgrade.
func TestReadsDataSavedByV2026_11_1(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "v2026.11.1"))); err != nil {
		t.Fatal(err)
	}

	t.Run("saved context", func(t *testing.T) {
		cfg, err := workstation.Load(filepath.Join(dir, "katlctl.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		topology, err := cfg.SelectedTopology("lab")
		if err != nil {
			t.Fatal(err)
		}

		if got, want := topology.ControlPlaneEndpoint.String(), "cp_1.home.arpa:6443"; got != want {
			t.Errorf("control-plane endpoint = %q, want %q", got, want)
		}
		var endpoints []string
		for _, node := range topology.Nodes {
			endpoints = append(endpoints, node.Name+" "+node.ManagementEndpoint.String())
		}
		if want := []string{"cp-1 cp_1.home.arpa:9443", "worker-1 [worker_1.home.arpa:19443]:9443"}; !slices.Equal(endpoints, want) {
			t.Errorf("management endpoints = %q, want %q", endpoints, want)
		}
	})

	t.Run("config bundle", func(t *testing.T) {
		inv, err := readManagementInventory(filepath.Join(dir, "cluster.katlcfg"))
		if err != nil {
			t.Fatal(err)
		}

		if got, want := inv.ControlPlaneEndpoint.String(), "api.home.arpa:6443"; got != want {
			t.Errorf("control-plane endpoint = %q, want %q", got, want)
		}
		var endpoints []string
		for _, node := range inv.Nodes {
			endpoints = append(endpoints, node.Name+" "+node.ManagementEndpoint().String())
		}
		if want := []string{"cp-1 cp_1.home.arpa:9443", "worker-1 worker_1.home.arpa:19443"}; !slices.Equal(endpoints, want) {
			t.Errorf("management endpoints = %q, want %q", endpoints, want)
		}
	})
}
