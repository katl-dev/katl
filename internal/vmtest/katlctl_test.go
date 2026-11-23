package vmtest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const operatorSentinelContext = "operator-homelab"

func workstationConfig(contexts ...string) string {
	var data strings.Builder
	data.WriteString("currentContext: " + contexts[0] + "\ncontexts:\n")
	for _, name := range contexts {
		data.WriteString("  - name: " + name + "\n    cluster: " + name + "\n")
	}
	data.WriteString("clusters:\n")
	for _, name := range contexts {
		data.WriteString("  - name: " + name + "\n    nodes:\n      - name: cp-1\n        managementEndpoint: 192.0.2.10:9443\n        systemRole: control-plane\n")
	}
	return data.String()
}

// TestKatlctlCommandIgnoresOperatorConfig places an operator config at every
// location katlctl could find it from the harness environment and checks that
// a harness-run katlctl neither reads nor writes it.
func TestKatlctlCommandIgnoresOperatorConfig(t *testing.T) {
	ctx := context.Background()
	katlctl := BuildKatlctl(t, ctx, repoRoot(t))

	operatorHome := t.TempDir()
	operatorConfig := filepath.Join(operatorHome, ".config", "katl", "katlctl.yaml")
	sentinel := []byte(workstationConfig(operatorSentinelContext, "operator-other"))
	if err := os.MkdirAll(filepath.Dir(operatorConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operatorConfig, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", operatorHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(operatorHome, ".config"))
	t.Setenv("KATLCTL_CONFIG", operatorConfig)
	t.Setenv("KATLCTL_CONFIG_DIR", filepath.Dir(operatorConfig))
	t.Setenv("KATLCTL_CONTEXT", operatorSentinelContext)

	runDir := t.TempDir()
	testConfig := filepath.Join(runDir, "katlctl.yaml")
	run := func(args ...string) (string, error) {
		t.Helper()
		cmd, err := katlctl.Command(ctx, testConfig, args...)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		err = cmd.Run()
		return output.String(), err
	}

	if output, err := run("context", "list"); err == nil || strings.Contains(output, "operator-") {
		t.Fatalf("katlctl context list without a test config: err = %v, output:\n%s", err, output)
	}

	if err := os.WriteFile(testConfig, []byte(workstationConfig("vmtest", "vmtest-next")), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run("context", "use", "vmtest-next"); err != nil {
		t.Fatalf("katlctl context use: %v\n%s", err, output)
	}
	output, err := run("context", "current")
	if err != nil || strings.TrimSpace(output) != "vmtest-next" {
		t.Fatalf("katlctl context current = %q, %v; want vmtest-next", output, err)
	}

	after, err := os.ReadFile(operatorConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, sentinel) {
		t.Fatalf("operator config changed:\n%s", after)
	}
}

func TestKatlctlCommandRefusesUnsafeConfig(t *testing.T) {
	operatorHome := t.TempDir()
	t.Setenv("HOME", operatorHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(operatorHome, ".config"))
	built := NewKatlctl("/bin/true")

	for name, test := range map[string]struct {
		katlctl Katlctl
		config  string
	}{
		"unbuilt":         {katlctl: Katlctl{}, config: filepath.Join(t.TempDir(), "katlctl.yaml")},
		"relative":        {katlctl: built, config: "katlctl.yaml"},
		"operator config": {katlctl: built, config: filepath.Join(operatorHome, ".config", "katl", "katlctl.yaml")},
		"operator dir":    {katlctl: built, config: filepath.Join(operatorHome, ".config", "katl", "other", "katlctl.yaml")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := test.katlctl.Command(context.Background(), test.config, "version"); err == nil {
				t.Fatalf("Command(%q) succeeded", test.config)
			}
		})
	}
}

// TestVMTestsRunKatlctlThroughCommand keeps every VM test on the isolated
// katlctl path: building or running ./cmd/katlctl elsewhere, or pointing the
// test process at a katlctl config, would bypass Katlctl.Command.
func TestVMTestsRunKatlctlThroughCommand(t *testing.T) {
	forbidden := []string{`"./cmd/katlctl"`, `t.Setenv("KATLCTL_`, `"KATLCTL_CONFIG=`}
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		switch path {
		// runner_scripts_test.go gives scripts/vmtest-run an operator katlctl
		// environment to check that the harness replaces it; it runs no katlctl.
		case "katlctl.go", "katlctl_test.go", "runner_scripts_test.go":
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, pattern := range forbidden {
			if bytes.Contains(data, []byte(pattern)) {
				t.Errorf("%s contains %s; run katlctl through vmtest.Katlctl.Command", path, pattern)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
