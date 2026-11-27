package extensionrevision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

func TestDecide(t *testing.T) {
	current := State{Name: "bird", Present: true, Version: "v3.4.0-katl.7", Inputs: "i1", Validation: "c1"}
	with := func(change func(*State)) State {
		state := current
		change(&state)
		return state
	}

	for _, tc := range []struct {
		name       string
		base, head State
		want       Answer
		reason     string
	}{
		// Row 1. Wrong: deciding from the base alone and building a removed extension.
		{"head absent", current, State{Name: "bird"}, None, ""},
		// Row 2. Wrong: comparing with an absent base's empty version and rejecting.
		{"base absent", State{Name: "bird"}, current, NewVersion, ""},
		// Row 3. Wrong: treating any version change as an advance, which
		// returns to a tag an earlier version already used.
		{"revert to earlier revision", current, with(func(s *State) { s.Version = "v3.4.0-katl.6"; s.Inputs = "i0" }), Invalid, "revert"},
		// Wrong: ordering by katl.N alone, ignoring the payload version.
		{"lower payload with higher revision", current, with(func(s *State) { s.Version = "v3.3.9-katl.9" }), Invalid, "does not advance"},
		{"unordered version", current, with(func(s *State) { s.Version = "v3.4.0-katl.7b" }), Invalid, "katl.<N>"},
		// Row 4. Wrong: comparing versions as strings, which orders katl.10
		// before katl.9 and 3.10 before 3.9.
		{"revision advances", with(func(s *State) { s.Version = "v3.4.0-katl.9" }), with(func(s *State) { s.Version = "v3.4.0-katl.10"; s.Inputs = "i2" }), NewVersion, ""},
		{"payload advances", with(func(s *State) { s.Version = "v3.9.0-katl.7" }), with(func(s *State) { s.Version = "v3.10.0-katl.1" }), NewVersion, ""},
		// Wrong: checking inputs before the version, which calls an advance
		// with unchanged inputs Unchanged and never builds the new version.
		{"advance with unchanged inputs", current, with(func(s *State) { s.Version = "v3.4.0-katl.8" }), NewVersion, ""},
		// Row 5.
		{"unchanged", current, current, Unchanged, ""},
		// Row 6. Wrong: requiring a new version for a validation-only change.
		{"validation changed", current, with(func(s *State) { s.Validation = "c2" }), Revalidate, ""},
		// Row 7. Wrong: letting a validation change mask a build input change.
		{"build and validation changed", current, with(func(s *State) { s.Inputs = "i2"; s.Validation = "c2" }), Unproven, "advance KATL_EXTENSION_ARTIFACT_VERSION past v3.4.0-katl.7"},
		// Restoring earlier inputs under the current version rolls forward too.
		{"revert of inputs", current, with(func(s *State) { s.Inputs = "i0" }), Unproven, "revert"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.base, tc.head)
			if got.Answer != tc.want {
				t.Errorf("Decide = %s (%s), want %s", got.Answer, got.Reason, tc.want)
			}
			if !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("reason %q does not mention %q", got.Reason, tc.reason)
			}
		})
	}
}

const birdDefinition = "KATL_EXTENSION_NAME=bird\n" +
	"KATL_EXTENSION_ARTIFACT_VERSION=v3.4.0-katl.7\n" +
	"KATL_EXTENSION_PAYLOAD_VERSION=v3.4.0\n" +
	"KATL_EXTENSION_RUNTIME_INTERFACE=katl-runtime-1\n"

func TestReadDefinition(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "extensions/bird/extension.env", birdDefinition)
	commit(t, repo)

	state, err := Read(repo, "HEAD", "bird")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Present || state.Version != "v3.4.0-katl.7" {
		t.Errorf("Read = %+v, want present at v3.4.0-katl.7", state)
	}
}

// Wrong: parsing the definition more leniently than a shell would assign it,
// so the gate and the build read different values. Each line assigns
// KATL_EXTENSION_PAYLOAD_VERSION, which Read neither requires nor sees
// assigned elsewhere, so only the plain-line rule can reject it.
func TestReadRejectsNonPlainDefinition(t *testing.T) {
	for _, line := range []string{
		`KATL_EXTENSION_PAYLOAD_VERSION="v3.4.0"`,
		`export KATL_EXTENSION_PAYLOAD_VERSION=v3.4.0`,
		`KATL_EXTENSION_PAYLOAD_VERSION=v3.$(id -u)`,
		`KATL_EXTENSION_PAYLOAD_VERSION=v3.${REV}`,
		`KATL_EXTENSION_PAYLOAD_VERSION=v3.4.0 # pinned`,
		`KATL_EXTENSION_PAYLOAD_VERSION =v3.4.0`,
		`# comment`,
	} {
		t.Run(line, func(t *testing.T) {
			repo := newRepo(t)
			writeFile(t, repo, "extensions/bird/extension.env", "KATL_EXTENSION_NAME=bird\nKATL_EXTENSION_ARTIFACT_VERSION=v3.4.0-katl.7\n"+line+"\n")
			commit(t, repo)

			if state, err := Read(repo, "HEAD", "bird"); err == nil {
				t.Errorf("Read accepted %q as %+v", line, state)
			}
		})
	}
}

// Wrong: letting a later assignment win, as a shell would, so the gate reads
// one version and a reader taking the first assignment reads another.
func TestReadRejectsDuplicateKey(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "extensions/bird/extension.env", birdDefinition+"KATL_EXTENSION_ARTIFACT_VERSION=v3.4.0-katl.8\n")
	commit(t, repo)

	if state, err := Read(repo, "HEAD", "bird"); err == nil {
		t.Errorf("Read accepted a second version assignment as %+v", state)
	}
}

// Wrong: accepting any plain version, so an extension introduced with an
// unorderable version can never advance.
func TestReadVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		valid   bool
	}{
		{"v3.4.0-katl.7", true},
		{"3.4-katl.1", true},
		{"v3.4.0", false},
		{"v3.4.0-katl.", false},
		{"v3.4.0-katl.7b", false},
		{"v3.4.0-rc1-katl.1", false},
		{"latest", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			repo := newRepo(t)
			writeFile(t, repo, "extensions/bird/extension.env", "KATL_EXTENSION_NAME=bird\nKATL_EXTENSION_ARTIFACT_VERSION="+tc.version+"\n")
			commit(t, repo)

			state, err := Read(repo, "HEAD", "bird")
			switch {
			case tc.valid && err != nil:
				t.Errorf("Read rejected %s: %v", tc.version, err)
			case tc.valid && state.Version != tc.version:
				t.Errorf("Read version = %q, want %q", state.Version, tc.version)
			case !tc.valid && err == nil:
				t.Errorf("Read accepted %s as %+v", tc.version, state)
			}
		})
	}
}

// Wrong: listing paths relative to the working directory, so a caller in a
// subdirectory finds no definition and decides None.
func TestReadFromSubdirectory(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "extensions/bird/extension.env", birdDefinition)
	writeFile(t, repo, "docs/extensions.md", "notes\n")
	commit(t, repo)
	subdirectory := filepath.Join(repo, "docs")

	names, err := Names(subdirectory, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "bird" {
		t.Errorf("Names = %q, want [bird]", names)
	}
	state, err := Read(subdirectory, "HEAD", "bird")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Present {
		t.Errorf("Read = %+v, want present", state)
	}
}

func TestReadInputs(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "extensions/bird/extension.env", "KATL_EXTENSION_NAME=bird\nKATL_EXTENSION_ARTIFACT_VERSION=v3.4.0-katl.7\n")
	writeFile(t, repo, "internal/systemdunit/units/katl-system-extensions-activate.service", "[Unit]\n")
	writeFile(t, repo, "internal/sysextimage/sysextimage.go", "package sysextimage\n")
	writeFile(t, repo, "docs/extensions.md", "notes\n")
	base := commit(t, repo)

	for _, tc := range []struct {
		name, path string
		want       Answer
	}{
		// Wrong: a narrowed recipe that omits the shared activation units.
		{"activation unit", "internal/systemdunit/units/katl-system-extensions-activate.service", Unproven},
		// Wrong: fingerprinting the whole tree, or deciding from any diff.
		{"documentation", "docs/extensions.md", Unchanged},
		// Wrong: folding validation inputs into the build fingerprint.
		{"validation input", "internal/sysextimage/sysextimage.go", Revalidate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runGit(t, repo, "checkout", "-q", "--detach", base)
			writeFile(t, repo, tc.path, "changed\n")
			head := commit(t, repo)

			before, err := Read(repo, base, "bird")
			if err != nil {
				t.Fatal(err)
			}
			after, err := Read(repo, head, "bird")
			if err != nil {
				t.Fatal(err)
			}
			if got := Decide(before, after); got.Answer != tc.want {
				t.Errorf("changing %s: Decide = %s (%s), want %s", tc.path, got.Answer, got.Reason, tc.want)
			}
		})
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	// Read runs git with the process environment, so confine it too.
	for _, entry := range gittest.Env(nil, repo) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	runGit(t, repo, "init", "-q", "-b", "main")
	return repo
}

func writeFile(t *testing.T, repo, name, data string) {
	t.Helper()
	name = filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo string) string {
	t.Helper()
	runGit(t, repo, "add", "--all")
	runGit(t, repo, "commit", "-qm", "fixture")
	return runGit(t, repo, "rev-parse", "HEAD")
}

func runGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
