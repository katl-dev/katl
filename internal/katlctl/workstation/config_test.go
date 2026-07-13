package workstation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
)

func TestResolvePathPrecedence(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "xdg")
	configDir := filepath.Join(t.TempDir(), "katlctl")
	configFile := filepath.Join(t.TempDir(), "custom.yaml")
	env := map[string]string{
		"XDG_CONFIG_HOME":    configHome,
		"KATLCTL_CONFIG_DIR": configDir,
	}
	path, err := ResolvePath(func(name string) string { return env[name] }, func() (string, error) {
		return configHome, nil
	})
	if err != nil {
		t.Fatalf("ResolvePath() error = %v", err)
	}
	if want := filepath.Join(configDir, "katlctl.yaml"); path != want {
		t.Fatalf("ResolvePath() = %q, want %q", path, want)
	}

	env["KATLCTL_CONFIG"] = configFile
	path, err = ResolvePath(func(name string) string { return env[name] }, func() (string, error) {
		return configHome, nil
	})
	if err != nil {
		t.Fatalf("ResolvePath() with file override error = %v", err)
	}
	if path != configFile {
		t.Fatalf("ResolvePath() = %q, want %q", path, configFile)
	}
}

func TestResolvePathReportsUserConfigDirFailure(t *testing.T) {
	_, err := ResolvePath(func(string) string { return "" }, func() (string, error) {
		return "", errors.New("no home")
	})
	if err == nil || !strings.Contains(err.Error(), "locate katlctl config directory") {
		t.Fatalf("ResolvePath() error = %v", err)
	}
}

func TestLoadSelectedTopology(t *testing.T) {
	path := writeConfig(t, validConfigYAML())
	resolved, err := ResolveTopology(ResolveRequest{ConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveTopology() error = %v", err)
	}
	if resolved.Source != SourceConfigContext {
		t.Fatalf("Source = %q", resolved.Source)
	}
	if resolved.ContextName != "prod" || resolved.ClusterName != "katl-prod" || resolved.ControlPlaneEndpoint != "api.prod.test:6443" {
		t.Fatalf("topology = %#v", resolved.Topology)
	}
	if len(resolved.Nodes) != 3 {
		t.Fatalf("nodes = %#v", resolved.Nodes)
	}
	if got := resolved.Nodes[0]; got.Name != "cp-1" || got.ManagementEndpoint != "cp-1.prod.test:9443" || got.SystemRole != inventory.RoleControlPlane || got.CredentialRef != "file:/secure/katl/cp-1.token" {
		t.Fatalf("first node = %#v", got)
	}
	controlPlanes := resolved.ControlPlaneNodes()
	if len(controlPlanes) != 2 || controlPlanes[0].Name != "cp-1" || controlPlanes[1].Name != "cp-2" {
		t.Fatalf("control planes = %#v", controlPlanes)
	}
}

func TestLoadExplicitContext(t *testing.T) {
	path := writeConfig(t, validConfigYAML())
	resolved, err := ResolveTopology(ResolveRequest{ConfigPath: path, ContextName: "stage"})
	if err != nil {
		t.Fatalf("ResolveTopology() error = %v", err)
	}
	if resolved.ContextName != "stage" || resolved.ClusterName != "katl-stage" {
		t.Fatalf("topology = %#v", resolved.Topology)
	}
	if len(resolved.Nodes) != 1 || resolved.Nodes[0].Name != "stage-cp" {
		t.Fatalf("nodes = %#v", resolved.Nodes)
	}
}

func TestValidateRejectsSchemaErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "duplicate context",
			yaml: strings.Replace(validConfigYAML(), "name: stage", "name: prod", 1),
			want: "duplicate context name",
		},
		{
			name: "duplicate cluster",
			yaml: strings.Replace(validConfigYAML(), "name: katl-stage", "name: katl-prod", 1),
			want: "duplicate cluster name",
		},
		{
			name: "duplicate node",
			yaml: strings.Replace(validConfigYAML(), "name: worker-1", "name: cp-1", 1),
			want: "duplicate node name",
		},
		{
			name: "unknown current context",
			yaml: strings.Replace(validConfigYAML(), "currentContext: prod", "currentContext: missing", 1),
			want: "currentContext",
		},
		{
			name: "unknown context cluster",
			yaml: strings.Replace(validConfigYAML(), "cluster: katl-prod", "cluster: missing", 1),
			want: "unknown cluster",
		},
		{
			name: "missing management endpoint",
			yaml: strings.Replace(validConfigYAML(), "managementEndpoint: worker-1.prod.test:9443", "managementEndpoint: ''", 1),
			want: "managementEndpoint is required",
		},
		{
			name: "invalid role",
			yaml: strings.Replace(validConfigYAML(), "systemRole: worker", "systemRole: storage", 1),
			want: "unsupported",
		},
		{
			name: "endpoint without port",
			yaml: strings.Replace(validConfigYAML(), "managementEndpoint: worker-1.prod.test:9443", "managementEndpoint: worker-1.prod.test", 1),
			want: "must be host:port",
		},
		{
			name: "endpoint nonnumeric port",
			yaml: strings.Replace(validConfigYAML(), "managementEndpoint: worker-1.prod.test:9443", "managementEndpoint: worker-1.prod.test:https", 1),
			want: "port must be a number",
		},
		{
			name: "endpoint out of range port",
			yaml: strings.Replace(validConfigYAML(), "managementEndpoint: worker-1.prod.test:9443", "managementEndpoint: worker-1.prod.test:999999", 1),
			want: "port must be a number",
		},
		{
			name: "inline secret material",
			yaml: strings.Replace(validConfigYAML(), "credentialRef: file:/secure/katl/worker-1.token", "credentialRef: abcdef.0123456789abcdef", 1),
			want: "inline secret material",
		},
		{
			name: "inline bearer token",
			yaml: strings.Replace(validConfigYAML(), "credentialRef: file:/secure/katl/worker-1.token", "credentialRef: Bearer secret-token", 1),
			want: "inline secret material",
		},
		{
			name: "inline kubeconfig data",
			yaml: strings.Replace(validConfigYAML(), "credentialRef: file:/secure/katl/worker-1.token", `credentialRef: "client-key-data: LS0tKEY"`, 1),
			want: "inline secret material",
		},
		{
			name: "unknown field",
			yaml: strings.Replace(validConfigYAML(), "clusters:", "unknownField: true\nclusters:", 1),
			want: "field unknownField not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsMultipleDocuments(t *testing.T) {
	data := validConfigYAML() + "\n---\ncredentialRef: abcdef.0123456789abcdef\n"
	_, err := Load(writeConfig(t, data))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("Load() error = %v, want multiple document rejection", err)
	}
}

func TestSameNodeNameAcrossClustersIsAllowed(t *testing.T) {
	data := strings.Replace(validConfigYAML(), "name: stage-cp", "name: cp-1", 1)
	cfg, err := Load(writeConfig(t, data))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	topology, err := cfg.SelectedTopology("stage")
	if err != nil {
		t.Fatalf("SelectedTopology() error = %v", err)
	}
	if len(topology.Nodes) != 1 || topology.Nodes[0].Name != "cp-1" || topology.ClusterName != "katl-stage" {
		t.Fatalf("topology = %#v", topology)
	}
}

func TestCredentialReferenceCanNameKeyFile(t *testing.T) {
	data := strings.Replace(validConfigYAML(), "credentialRef: file:/secure/katl/worker-1.token", "credentialRef: file:/secure/katl/worker-1.private-key", 1)
	if _, err := Load(writeConfig(t, data)); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestRejectsInlineSecretWithoutLeakingValue(t *testing.T) {
	secret := "abcdef.0123456789abcdef"
	data := strings.Replace(validConfigYAML(), "credentialRef: file:/secure/katl/worker-1.token", "credentialRef: "+secret, 1)
	_, err := Load(writeConfig(t, data))
	if err == nil {
		t.Fatal("Load() error = nil, want inline secret rejection")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
	if !strings.Contains(err.Error(), "inline secret material") {
		t.Fatalf("Load() error = %v, want inline secret material", err)
	}
}

func TestResolveTopologyPrecedence(t *testing.T) {
	missingConfigPath := filepath.Join(t.TempDir(), "missing.yaml")
	inv := inventory.Inventory{
		ControlPlaneEndpoint: "api.inventory.test:6443",
		Nodes: []inventory.Node{{
			Name:       "inventory-cp",
			Address:    "inventory-cp.test",
			SystemRole: inventory.RoleControlPlane,
			Access:     inventory.Access{CredentialRef: "file:/secure/inventory-cp.token"},
		}},
	}
	plan := inventory.Plan{
		ControlPlaneEndpoint: "api.plan.test:6443",
		Nodes: []inventory.PlannedNode{{
			Name:       "plan-cp",
			Address:    "plan-cp.test:9443",
			SystemRole: inventory.RoleControlPlane,
			Access:     inventory.Access{CredentialRef: "file:/secure/plan-cp.token"},
		}},
	}
	resolved, err := ResolveTopology(ResolveRequest{
		ConfigPath:        missingConfigPath,
		ExplicitInventory: &inv,
	})
	if err != nil {
		t.Fatalf("ResolveTopology(inventory) error = %v", err)
	}
	if resolved.Source != SourceExplicitInventory || resolved.ClusterName != "inventory" || resolved.Nodes[0].Name != "inventory-cp" {
		t.Fatalf("resolved inventory topology = %#v", resolved)
	}
	if resolved.Nodes[0].ManagementEndpoint != "inventory-cp.test:9443" {
		t.Fatalf("inventory management endpoint = %q", resolved.Nodes[0].ManagementEndpoint)
	}

	resolved, err = ResolveTopology(ResolveRequest{
		ConfigPath:        missingConfigPath,
		ExplicitInventory: &inv,
		ExplicitPlan:      &plan,
	})
	if err != nil {
		t.Fatalf("ResolveTopology(plan) error = %v", err)
	}
	if resolved.Source != SourceExplicitPlan || resolved.ClusterName != "plan" || resolved.Nodes[0].Name != "plan-cp" {
		t.Fatalf("resolved plan topology = %#v", resolved)
	}
}

func TestExplicitInventoryDoesNotBorrowFromConfig(t *testing.T) {
	path := writeConfig(t, validConfigYAML())
	inv := inventory.Inventory{
		ControlPlaneEndpoint: "api.inventory.test:6443",
		Nodes: []inventory.Node{{
			Name:       "inventory-cp",
			Address:    "inventory-cp.test",
			SystemRole: inventory.RoleControlPlane,
			Access:     inventory.Access{},
		}},
	}
	_, err := ResolveTopology(ResolveRequest{
		ConfigPath:        path,
		ExplicitInventory: &inv,
	})
	if err == nil || !strings.Contains(err.Error(), "credentialRef is required") {
		t.Fatalf("ResolveTopology() error = %v, want explicit inventory credential failure", err)
	}
}

func TestSaveAndUpsertClusterPreservePrivateConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "katlctl.yaml")
	cfg := Config{}.UpsertCluster("lab", Cluster{
		Name:  "lab",
		Nodes: []Node{{Name: "cp-1", ManagementEndpoint: "cp-1.test:9443", SystemRole: inventory.RoleControlPlane, CredentialRef: "file:/secure/cp-1.token"}},
	})
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CurrentContext != "lab" || len(loaded.Clusters) != 1 {
		t.Fatalf("loaded config = %#v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v", info.Mode().Perm())
	}
}

func TestCredentialPathLivesBesideConfig(t *testing.T) {
	path, err := CredentialPath("/tmp/katl/katlctl.yaml", "lab", "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/katl/credentials/lab/cp-1.token" {
		t.Fatalf("CredentialPath() = %q", path)
	}
}

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "katlctl.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validConfigYAML() string {
	return `currentContext: prod
contexts:
- name: prod
  cluster: katl-prod
- name: stage
  cluster: katl-stage
clusters:
- name: katl-prod
  controlPlaneEndpoint: api.prod.test:6443
  nodes:
  - name: cp-1
    managementEndpoint: cp-1.prod.test:9443
    systemRole: control-plane
    credentialRef: file:/secure/katl/cp-1.token
  - name: cp-2
    managementEndpoint: cp-2.prod.test:9443
    systemRole: control-plane
    credentialRef: file:/secure/katl/cp-2.token
  - name: worker-1
    managementEndpoint: worker-1.prod.test:9443
    systemRole: worker
    credentialRef: file:/secure/katl/worker-1.token
- name: katl-stage
  nodes:
  - name: stage-cp
    managementEndpoint: stage-cp.test:9443
    systemRole: control-plane
    credentialRef: file:/secure/katl/stage-cp.token
`
}
