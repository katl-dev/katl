package scriptstest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func devShellWorkstationConfig(current string, contexts ...string) string {
	var data strings.Builder
	data.WriteString("currentContext: " + current + "\ncontexts:\n")
	for _, name := range contexts {
		data.WriteString("  - name: " + name + "\n    cluster: " + name + "\n")
	}
	data.WriteString("clusters:\n")
	for _, name := range contexts {
		data.WriteString("  - name: " + name + "\n    nodes:\n      - name: cp-1\n        managementEndpoint: 192.0.2.10:9443\n        systemRole: control-plane\n")
	}
	return data.String()
}

// TestDevShellKeepsKatlctlContextsInWorld runs katlctl context commands the
// way a katldev journey does, in a dev shell whose operator has their own
// katlctl config selected, and checks that only the world's config changes.
func TestDevShellKeepsKatlctlContextsInWorld(t *testing.T) {
	repo := repoRoot(t)
	bin := t.TempDir()
	buildCommand(t, bin, "katlctl")

	operatorHome := t.TempDir()
	operatorConfig := filepath.Join(operatorHome, ".config", "katl", "katlctl.yaml")
	sentinel := []byte(devShellWorkstationConfig("operator-homelab", "operator-homelab", "dev-world"))
	if err := os.MkdirAll(filepath.Dir(operatorConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operatorConfig, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}

	checkout := t.TempDir()
	worldConfig := filepath.Join(checkout, "_build", "katldev", "katlctl.yaml")
	if err := os.MkdirAll(filepath.Dir(worldConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worldConfig, []byte(devShellWorkstationConfig("vmtest", "vmtest", "dev-world")), 0o600); err != nil {
		t.Fatal(err)
	}

	script := `source "$1" "$2" && katlctl context use dev-world && katlctl context current`
	shell := exec.Command("bash", "-c", script, "bash", filepath.Join(repo, "scripts", "dev-shell-env"), checkout)
	shell.Dir = checkout
	shell.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+operatorHome,
		"XDG_CONFIG_HOME="+filepath.Join(operatorHome, ".config"),
		"KATLCTL_CONFIG="+operatorConfig,
		"KATLCTL_CONFIG_DIR="+filepath.Dir(operatorConfig),
		"KATLCTL_CONTEXT=operator-homelab",
	)
	output, err := shell.CombinedOutput()
	if err != nil {
		t.Fatalf("dev shell katlctl: %v\n%s", err, output)
	}
	if !strings.HasSuffix(string(output), "dev-world\n") {
		t.Fatalf("katlctl context current output:\n%s", output)
	}

	world, err := os.ReadFile(worldConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(world), "currentContext: dev-world") {
		t.Fatalf("world katlctl config did not record the selected context:\n%s", world)
	}
	after, err := os.ReadFile(operatorConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, sentinel) {
		t.Fatalf("operator katlctl config changed:\n%s", after)
	}
}
