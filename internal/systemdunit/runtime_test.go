package systemdunit_test

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/systemdunit"
)

func runtimeUnitNames(t *testing.T) []string {
	t.Helper()
	entries, err := systemdunit.RuntimeUnits.ReadDir("units")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		t.Fatal("no runtime units are embedded")
	}
	return names
}

func TestRuntimeUnitsParse(t *testing.T) {
	names := runtimeUnitNames(t)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			data, err := systemdunit.RuntimeUnits.ReadFile("units/" + name)
			if err != nil {
				t.Fatal(err)
			}
			values, err := parseUnit(string(data))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(values["Unit.Description"]) == 0 {
				t.Fatal("no [Unit] Description=")
			}

			switch path.Ext(name) {
			case ".service":
				if len(values["Service.ExecStart"]) == 0 {
					t.Fatal("no [Service] ExecStart=")
				}
			case ".timer":
				if len(values["Timer.OnActiveSec"]) != 1 {
					t.Fatalf("OnActiveSec = %q, want one schedule", values["Timer.OnActiveSec"])
				}
				// A timer instance activates the same instance of an embedded
				// template, so the target must ship beside it.
				target := values["Timer.Unit"]
				if len(target) != 1 || !strings.Contains(target[0], "@%i.") {
					t.Fatalf("Unit = %q, want one template instance named by %%i", target)
				}
				template := strings.Replace(target[0], "@%i.", "@.", 1)
				if _, err := systemdunit.RuntimeUnits.ReadFile("units/" + template); err != nil {
					t.Fatalf("timer target %s is not embedded: %v", template, err)
				}
			default:
				t.Fatalf("unexpected unit type %q", path.Ext(name))
			}
		})
	}
}

// The runtime image installs these files verbatim; an installed system's state
// tree must receive the same bytes.
func TestGenerationWritesRuntimeUnits(t *testing.T) {
	root := t.TempDir()
	assets, err := generation.WriteState(root, generation.StateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range runtimeUnitNames(t) {
		want, err := systemdunit.RuntimeUnits.ReadFile("units/" + name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(root, "etc/systemd/system", name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("state tree %s differs from the embedded unit (read error %v):\n%s", name, err, got)
		}
		if name == "katl-system-extensions-activate.service" && assets.ExtensionActivate != string(want) {
			t.Errorf("rendered extension activation differs from the embedded unit:\n%s", assets.ExtensionActivate)
		}
	}
}

// parseUnit reads systemd's unit file syntax strictly enough to reject lines
// systemd would ignore with a warning. Values are keyed "Section.Key".
func parseUnit(data string) (map[string][]string, error) {
	values := map[string][]string{}
	section := ""
	lines := strings.Split(data, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		for strings.HasSuffix(line, "\\") && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, "\\") + " " + strings.TrimSpace(lines[i])
		}
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			if section == "" || strings.ContainsAny(section, "[] ") {
				return nil, fmt.Errorf("line %d: invalid section %q", i+1, line)
			}
		default:
			key, value, ok := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			if !ok || key == "" || strings.ContainsAny(key, " \t") {
				return nil, fmt.Errorf("line %d: not an assignment: %q", i+1, line)
			}
			if section == "" {
				return nil, fmt.Errorf("line %d: assignment outside a section: %q", i+1, line)
			}
			values[section+"."+key] = append(values[section+"."+key], strings.TrimSpace(value))
		}
	}
	return values, nil
}
