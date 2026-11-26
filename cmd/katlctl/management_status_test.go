package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/managementidentity"
)

func TestStatusDuringEnableMTLSTrial(t *testing.T) {
	world, deadline := newManagementStatusTrial(t, 4*time.Minute)
	wantDeadline := deadline.Add(-4 * time.Minute).Local().Format(time.TimeOnly)
	wantResume := "cp-1 is on an enable-mtls trial; rerun katlctl management enable-mtls --config '" + world.configPath +
		"' before " + wantDeadline + " to confirm it, or wait for it to return to trusted-network"
	for _, command := range [][]string{{"node", "status", "cp-1"}, {"cluster", "status"}} {
		t.Run(command[0], func(t *testing.T) {
			args := append(command, "--config", world.configPath, "--timeout", "2s")
			var out, stderr bytes.Buffer
			if err := run(context.Background(), args, &out, &stderr); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"trusted-network (mtls trial)", "mTLS trial until " + wantDeadline, wantResume} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("status lacks %q:\n%s", want, &out)
				}
			}

			out.Reset()
			if err := run(context.Background(), append(args, "--output", "json"), &out, &stderr); err != nil {
				t.Fatal(err)
			}
			var report struct {
				Management *managementStatusReport
				Nodes      []clusterNodeStatus
			}
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			management := report.Management
			if command[0] == "cluster" && len(report.Nodes) == 1 && report.Nodes[0].Reachable {
				management = report.Nodes[0].Management
			}
			if management == nil || management.Authentication != "trusted-network" || management.Transition == nil ||
				management.Transition.State != "trial" || management.Transition.Deadline != deadline.Format(time.RFC3339) {
				t.Errorf("status lost the live management transition:\n%s", &out)
			}
		})
	}
}

func TestStatusWithoutEnableMTLSSecrets(t *testing.T) {
	world := newEnableMTLSWorld(t, "cp-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node := world.nodes[0]
	node.afterBegin = func(context.Context) { cancel() }
	custom := filepath.Join(t.TempDir(), "lab-secrets.yaml")
	if _, _, err := world.enableContext(ctx, "--output", custom); err == nil || !strings.Contains(err.Error(), "--output '"+custom+"'") {
		t.Fatalf("interrupted enable-mtls = %v, want the original command with its custom output", err)
	}
	node.rebinds.Wait()

	var out, stderr bytes.Buffer
	err := run(context.Background(), []string{"node", "status", "cp-1", "--config", world.configPath}, &out, &stderr)
	if err == nil {
		t.Fatal("status succeeded with an unreferenced secrets file")
	}
	for _, want := range []string{
		"likely serves mTLS management", "may be on an interrupted enable-mtls trial",
		"katlctl management enable-mtls --config '" + world.configPath + "' --output '<original-secrets-path>'",
		"replace the placeholder with the original path", "returns to trusted-network by itself at its deadline",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("status error lacks %q: %v", want, err)
		}
	}
	if _, err := os.Stat(world.secretsPath); !os.IsNotExist(err) {
		t.Fatalf("status left a file at the missing secrets path: %v", err)
	}
	if _, stderr, err := world.enable("--output", custom); err != nil {
		t.Fatalf("resume with the original custom output: %v\n%s", err, stderr)
	}
	if _, err := os.Stat(world.secretsPath); !os.IsNotExist(err) {
		t.Errorf("resume created default secrets: %v", err)
	}
}

func TestStatusWithReferencedEnableMTLSSecrets(t *testing.T) {
	world, _ := newManagementStatusTrial(t, 0)
	custom := filepath.Join(filepath.Dir(world.configPath), "lab-secrets.yaml")
	if err := os.Rename(world.secretsPath, custom); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(world.configPath)
	if err != nil {
		t.Fatal(err)
	}
	config := strings.Replace(string(data), "managementAuthentication: trusted-network", "managementAuthentication: trusted-network\n  managementIdentity: lab-secrets.yaml", 1)
	if err := os.WriteFile(world.configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	node := world.nodes[0]
	node.mu.Lock()
	node.mode = managementidentity.MutualTLS
	node.authority = node.trial.GetTargetAuthority()
	node.trial.State = "committed"
	node.mu.Unlock()

	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"node", "status", "cp-1", "--config", world.configPath}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 2 || !strings.HasSuffix(strings.TrimSpace(lines[1]), " mtls") {
		t.Errorf("status lacks the confirmed mTLS row:\n%s", &out)
	}
	if !strings.Contains(out.String(), "rerun katlctl management enable-mtls --config '"+world.configPath+"' to finish moving the cluster and update the config") {
		t.Errorf("status lacks guidance to finish the move:\n%s", &out)
	}
}

func TestStatusUsesConfiguredManagementWhenItAnswers(t *testing.T) {
	t.Run("plaintext", func(t *testing.T) {
		world := newEnableMTLSWorld(t, "cp-1")
		if err := os.WriteFile(world.secretsPath, []byte("invalid secrets"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		if err := run(context.Background(), []string{"node", "status", "cp-1", "--config", world.configPath}, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(out.String(), "\n")
		if len(lines) < 2 || !strings.HasSuffix(strings.TrimSpace(lines[1]), " trusted-network") || strings.Contains(out.String(), "rerun katlctl") {
			t.Errorf("plaintext status changed:\n%s", &out)
		}
	})
	t.Run("mtls", func(t *testing.T) {
		world, deadline := newManagementStatusTrial(t, 0)
		world.useMTLSConfig(t)
		var out, stderr bytes.Buffer
		if err := run(context.Background(), []string{"node", "status", "cp-1", "--config", world.configPath}, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "mTLS trial until "+deadline.Local().Format(time.TimeOnly)) || strings.Contains(out.String(), "rerun katlctl") {
			t.Errorf("mTLS status changed:\n%s", &out)
		}
	})
}

func TestRebootUsesConfiguredManagementDuringEnableMTLSTrial(t *testing.T) {
	world, _ := newManagementStatusTrial(t, 0)
	// Reboot shares the node status RPC as a preflight. A retry in the common
	// dialer or state reader would reach the fake agent over mTLS.
	var out, stderr bytes.Buffer
	err := run(context.Background(), []string{"node", "reboot", "cp-1", "--config", world.configPath, "--no-wait"}, &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "did not answer plaintext gRPC") || strings.Contains(err.Error(), "interrupted enable-mtls trial") {
		t.Errorf("reboot = %v, want the configured plaintext connection error", err)
	}
}

func newManagementStatusTrial(t *testing.T, clockOffset time.Duration) (enableMTLSWorld, time.Time) {
	t.Helper()
	world := newEnableMTLSWorld(t, "cp-1")
	node := world.nodes[0]
	bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	staged, _, err := managementidentity.EnsureNode(&bundle, node.name, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := managementidentity.Write(world.secretsPath, bundle); err != nil {
		t.Fatal(err)
	}
	authority, err := managementidentity.CAFingerprint(bundle.CertificateAuthority.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Add(4*time.Minute + clockOffset).Truncate(time.Second)
	node.mu.Lock()
	node.clockOffset = clockOffset
	node.staged = staged
	node.trial = &agentapi.ManagementTransition{State: "trial", TargetAuthority: authority, Deadline: deadline.Format(time.RFC3339)}
	node.mu.Unlock()
	node.rebind()
	node.rebinds.Wait()
	return world, deadline
}
