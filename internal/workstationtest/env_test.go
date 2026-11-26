package workstationtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("KATL_TEST_GO_CACHE_CHILD") == "1" {
		os.Exit(Run(m, nil))
	}
	os.Exit(m.Run())
}

func TestRunPreservesGoCaches(t *testing.T) {
	if os.Getenv("KATL_TEST_GO_CACHE_CHILD") == "1" {
		home := os.Getenv("HOME")
		tmp, err := filepath.EvalSymlinks(os.Getenv("TMPDIR"))
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(home) != tmp {
			t.Fatalf("isolated HOME %q is outside %q", home, tmp)
		}
		output, err := exec.Command("go", "env", "GOPATH", "GOMODCACHE", "GOCACHE", "GOTELEMETRY").CombinedOutput()
		if err != nil {
			t.Fatalf("go env: %v\n%s", err, output)
		}
		if want := os.Getenv("KATL_TEST_GO_CACHE_EXPECTED") + "off\n"; string(output) != want {
			t.Fatalf("go env after setup = %q, want %q", output, want)
		}
		if err := os.WriteFile(filepath.Join(home, "child-ran"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	tmp := t.TempDir()
	home := t.TempDir()
	config := filepath.Join(home, "config")
	if err := os.MkdirAll(filepath.Join(config, "go"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := "GOPATH=" + filepath.Join(home, "gopath") + "\n" +
		"GOMODCACHE=" + filepath.Join(home, "modules") + "\n" +
		"GOCACHE=" + filepath.Join(home, "build-cache") + "\n"
	if err := os.WriteFile(filepath.Join(config, "go", "env"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	env := append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+config, "TMPDIR="+tmp,
		"GOENV=", "GOPATH=", "GOMODCACHE=", "GOCACHE=")
	if err := disableGoTelemetry(env); err != nil {
		t.Fatal(err)
	}
	before := exec.Command("go", "env", "GOPATH", "GOMODCACHE", "GOCACHE")
	before.Env = env
	want, err := before.Output()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestRunPreservesGoCaches$")
	child.Env = append(env, "KATL_TEST_GO_CACHE_CHILD=1", "KATL_TEST_GO_CACHE_EXPECTED="+string(want))
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("isolated test package: %v\n%s", err, output)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("isolated test package left temporary entries: %v", entries)
	}
}

func TestConfigPathBoundary(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	outside := t.TempDir()
	link := filepath.Join(dir, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if err := checkPath(root, filepath.Join(dir, "new", "katlctl.yaml")); err != nil {
		t.Errorf("rejected a new config path in the sandbox: %v", err)
	}
	for _, path := range []string{
		filepath.Join(outside, "katlctl.yaml"),
		dir + "/../katlctl.yaml",
		filepath.Join(link, "katlctl.yaml"),
	} {
		if err := checkPath(root, path); err == nil {
			t.Errorf("accepted config path %q outside the sandbox", path)
		}
	}
}
