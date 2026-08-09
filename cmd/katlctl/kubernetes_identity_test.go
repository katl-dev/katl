package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/kubernetesidentity"
)

func TestKubernetesIdentityCreateAndInspect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.katlkey")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"kubernetes", "identity", "create", "--cluster-name", "lab", "--output", path}, &stdout, &stderr); err != nil {
		t.Fatalf("create error = %v, stderr=%s", err, stderr.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %04o", info.Mode().Perm())
	}
	if !strings.Contains(stdout.String(), "cluster=lab fingerprint=sha256:") || !strings.Contains(stdout.String(), "Store this secret independently") {
		t.Fatalf("create stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{"kubernetes", "identity", "inspect", path}, &stdout, &stderr); err != nil {
		t.Fatalf("inspect error = %v, stderr=%s", err, stderr.String())
	}
	for _, public := range []string{"cluster: lab", "fingerprint: sha256:", "kubernetes CA: sha256:", "service-account key: sha256:"} {
		if !strings.Contains(stdout.String(), public) {
			t.Errorf("inspect output is missing %q:\n%s", public, stdout.String())
		}
	}
	for _, secret := range []string{"PRIVATE KEY", "privateKey", "certificate:"} {
		if strings.Contains(stdout.String(), secret) {
			t.Errorf("inspect output exposed %q:\n%s", secret, stdout.String())
		}
	}
}

func TestKubernetesIdentityInspectRefusesExposedPermissions(t *testing.T) {
	path := writeTestKubernetesIdentity(t, "lab")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{"kubernetes", "identity", "inspect", path}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("inspect error = %v", err)
	}
}

func TestClusterBootstrapCarriesMatchingIdentityOnlyToInit(t *testing.T) {
	configPath := writeClusterConfig(t)
	identityPath := writeTestKubernetesIdentity(t, "lab")
	var got cluster.Request
	old := runAgentBootstrap
	runAgentBootstrap = func(_ context.Context, request cluster.Request, _ cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		got = request
		return cluster.Result{Plan: inventory.Plan{InitNode: request.InitNode}}, nil
	}
	t.Cleanup(func() { runAgentBootstrap = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"cluster", "bootstrap", "--config", configPath, "--identity", identityPath, "--init-node", "cp-1", "--dry-run"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("bootstrap error = %v, stderr=%s", err, stderr.String())
	}
	if got.ClusterName != "lab" || len(got.KubernetesIdentity) == 0 || !strings.HasPrefix(got.KubernetesIdentityFingerprint, "sha256:") {
		t.Fatalf("bootstrap request identity = cluster %q bytes=%d fingerprint=%q", got.ClusterName, len(got.KubernetesIdentity), got.KubernetesIdentityFingerprint)
	}
}

func TestClusterBootstrapRejectsIdentityForDifferentCluster(t *testing.T) {
	configPath := writeClusterConfig(t)
	identityPath := writeTestKubernetesIdentity(t, "other")
	err := run(context.Background(), []string{"cluster", "bootstrap", "--config", configPath, "--identity", identityPath, "--init-node", "cp-1", "--dry-run"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `belongs to cluster "other"`) || !strings.Contains(err.Error(), `config names cluster "lab"`) {
		t.Fatalf("bootstrap error = %v", err)
	}
}

func writeTestKubernetesIdentity(t *testing.T, clusterName string) string {
	t.Helper()
	bundle, err := kubernetesidentity.Generate(kubernetesidentity.GenerateOptions{ClusterName: clusterName, Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity.katlkey")
	if err := kubernetesidentity.Write(path, bundle); err != nil {
		t.Fatal(err)
	}
	return path
}
