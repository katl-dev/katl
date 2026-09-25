package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

func TestFlagsRejectBeforeEffects(t *testing.T) {
	for _, args := range [][]string{
		{"install", "discover", "--output", "invalid"},
		{"cluster", "bootstrap", "--config", "missing.yaml", "--output", "invalid"},
		{"cluster", "apply", "--config", "missing.yaml", "--mode", "invalid"},
		{"node", "reboot", "--endpoint", "unreachable", "--timeout", "0s"},
		{"context", "delete", "lab", "--output", "invalid"},
		{"cluster", "bootstrap", "--config", "missing.yaml", "--node-address", "cp-1=192.0.2.1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			err := run(context.Background(), args, &out, io.Discard)
			if err == nil || (!strings.Contains(err.Error(), "--output") && !strings.Contains(err.Error(), "--mode") && !strings.Contains(err.Error(), "--timeout") && !strings.Contains(err.Error(), "--node-address")) {
				t.Fatalf("expected flag validation before missing file/network access, got %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("unexpected output %s", &out)
			}
		})
	}
}

func TestOptionEnvironmentDefaults(t *testing.T) {
	config := writeClusterConfig(t)
	t.Setenv("KATLCTL_CLUSTER_CONFIG", config)
	t.Setenv("KATLCTL_NODE", "cp-1")

	var out bytes.Buffer
	err := run(context.Background(), []string{"config", "render-node", "--desired-version", "1"}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cp-1") {
		t.Fatalf("rendered configuration did not select cp-1: %s", &out)
	}
}

func TestExplicitOptionOverridesEnvironment(t *testing.T) {
	t.Setenv("KATLCTL_CLUSTER_CONFIG", "missing-from-env.yaml")
	t.Setenv("KATLCTL_NODE", "missing-from-env")
	config := writeClusterConfig(t)

	var out bytes.Buffer
	err := run(context.Background(), []string{"config", "render-node", "--config", config, "--node", "cp-1", "--desired-version", "1"}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cp-1") {
		t.Fatalf("rendered configuration did not select cp-1: %s", &out)
	}
}

func TestContextEnvironmentSelectsSavedContext(t *testing.T) {
	path := writeKatlctlConfig(t, `currentContext: first
contexts:
- name: first
  cluster: first
- name: second
  cluster: second
clusters:
- name: first
  nodes:
  - name: first-node
    managementEndpoint: 192.0.2.1:9443
    systemRole: control-plane
- name: second
  nodes:
  - name: second-node
    managementEndpoint: 192.0.2.2:9443
    systemRole: control-plane
`)
	t.Setenv("KATLCTL_CONFIG", path)
	t.Setenv("KATLCTL_CONTEXT", "second")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: nil, want: "second"},
		{args: []string{"--context", "first"}, want: "first"},
	} {
		var out bytes.Buffer
		args := append([]string{"context", "show", "--output", "json"}, tc.args...)
		if err := run(context.Background(), args, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		var topology workstation.ResolvedTopology
		if err := json.Unmarshal(out.Bytes(), &topology); err != nil {
			t.Fatal(err)
		}
		if topology.ContextName != tc.want {
			t.Fatalf("context = %q, want %q", topology.ContextName, tc.want)
		}
	}
}

func TestRepeatedNodeFlagDoesNotInheritEnvironment(t *testing.T) {
	t.Setenv("KATLCTL_NODE", "cp-1")
	root := newKatlctlCommand(context.Background(), io.Discard, io.Discard)
	wipe, _, err := root.Find([]string{"cluster", "wipe"})
	if err != nil {
		t.Fatal(err)
	}
	if got := wipe.Flags().Lookup("node").Value.String(); got != "" {
		t.Fatalf("repeated --node inherited %q", got)
	}
}

func TestConfigApplyHasOneOperatorPath(t *testing.T) {
	bundle, _ := writeConfigBundle(t)
	for _, path := range []string{writeClusterConfig(t), bundle} {
		var out bytes.Buffer
		err := run(context.Background(), []string{"node", "apply", "cp-1", "--config", path}, &out, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "katlctl cluster apply") || out.Len() != 0 {
			t.Fatalf("got output %q error %v", &out, err)
		}
	}
}

func TestContextDeleteNeverRetargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.yaml")
	t.Setenv("KATLCTL_CONFIG", path)
	cfg := workstation.Config{CurrentContext: "one", Contexts: []workstation.Context{{Name: "one", Cluster: "a"}, {Name: "alias", Cluster: "a"}, {Name: "two", Cluster: "b"}}, Clusters: []workstation.Cluster{{Name: "a", Nodes: []workstation.Node{{Name: "a", ManagementEndpoint: "192.0.2.1:9443", SystemRole: "control-plane"}}}, {Name: "b", Nodes: []workstation.Node{{Name: "b", ManagementEndpoint: "192.0.2.2:9443", SystemRole: "control-plane"}}}}}
	if err := workstation.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "alias", "two"} {
		if err := run(context.Background(), []string{"context", "delete", name}, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		got, err := workstation.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if got.CurrentContext != "" {
			t.Fatalf("implicitly selected %s", got.CurrentContext)
		}
		expected := map[string]int{"one": 2, "alias": 1, "two": 0}[name]
		if len(got.Clusters) != expected {
			t.Fatalf("delete %s left clusters %#v", name, got.Clusters)
		}
	}
}

func TestClusterApplyPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(configBundleSource(), "managementAuthentication: mtls", "managementAuthentication: trusted-network", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeKatlcAgentClient{
		nodeStatus:     &agentapi.NodeStatus{InventoryNodeName: "cp-1", EnrollmentId: "e", MachineId: "m", CurrentGenerationId: "generation-0"},
		validateResult: &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "next-boot", ChangedDomains: []string{"host-configuration"}},
	}
	old := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = old })
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	contextPath := filepath.Join(t.TempDir(), "context.yaml")
	t.Setenv("KATLCTL_CONFIG", contextPath)
	var out bytes.Buffer
	err := run(context.Background(), []string{"cluster", "apply", "--config", path, "--node", "cp-1", "--plan", "--mode", "next-boot", "-o", "json"}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var report clusterApplyReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "planned" || len(report.NodePlans) != 1 || report.NodePlans[0].ApplyMode != "next-boot" {
		t.Fatalf("report %#v", report)
	}
	if fake.submitRequest != nil || fake.stageRequest != nil || fake.applyRequest != nil {
		t.Fatal("plan mutated node")
	}
	if _, err := os.Stat(contextPath); !os.IsNotExist(err) {
		t.Fatalf("plan persisted context: %v", err)
	}
}
