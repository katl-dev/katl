// Package extensionrevision owns the decision whether a change to the
// repository changes an independently versioned system extension, defined by
// extensions/<name>/extension.env. See
// docs/internal/extension-revision-ownership.md.
package extensionrevision

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/katl-dev/katl/internal/buildinputs"
)

// State is one extension at one commit.
type State struct {
	Name    string
	Present bool   // extensions/<name>/extension.env exists
	Version string // KATL_EXTENSION_ARTIFACT_VERSION
	// Inputs is the fingerprint of the build inputs.
	Inputs string
	// Validation is the fingerprint of the validation inputs.
	Validation string
}

// Bump when changing the fingerprint format or the declared inputs'
// interpretation.
const fingerprintVersion = "v1"

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// buildInputs can change the payload. Until an Unproven decision can be
// proven, the set is an enumerated superset of the paths the earlier
// path-pattern gate matched.
func buildInputs(name string) []string {
	return []string{
		"Containerfile.mkosi",
		"mkosi.conf",
		"extensions/" + name + "/**",
		"mkosi.profiles/system-extension-" + name + "/**",
		"mkosi.profiles/runtime/**",
		"internal/systemdunit/units/**",
		"scripts/build-system-extension",
		"scripts/fedora-release",
		"scripts/mkosi",
	}
}

// validationInputs cannot change the payload but decide whether it is checked
// and packed correctly. The gate's workflow job is included as its whole file.
var validationInputs = []string{
	"scripts/check-system-extension",
	"internal/sysextimage/**",
	"internal/installer/payloadbundle/**",
	"internal/installer/systemextensionbundle/**",
	"cmd/katl-mkosi-artifacts/extract_sysext.go",
	"cmd/katlctl/system_extension.go",
	".github/workflows/system-extensions.yml",
}

func definition(name string) string { return "extensions/" + name + "/extension.env" }

// Read returns the state of extension name at revision in the repository at
// repo. A definition that is not plain KEY=value lines is an error.
func Read(repo, revision, name string) (State, error) {
	state := State{Name: name}
	if !validName.MatchString(name) {
		return state, fmt.Errorf("invalid extension name %q", name)
	}
	listed, err := git(repo, "ls-tree", "--name-only", revision, "--", definition(name))
	if err != nil {
		return state, err
	}
	if len(bytes.TrimSpace(listed)) == 0 {
		return state, nil
	}
	data, err := git(repo, "cat-file", "blob", revision+":"+definition(name))
	if err != nil {
		return state, err
	}
	values, err := parseDefinition(data)
	if err != nil {
		return state, fmt.Errorf("%s at %s: %w", definition(name), revision, err)
	}
	if got := values["KATL_EXTENSION_NAME"]; got != name {
		return state, fmt.Errorf("%s at %s: KATL_EXTENSION_NAME is %q, want %q", definition(name), revision, got, name)
	}
	state.Version = values["KATL_EXTENSION_ARTIFACT_VERSION"]
	if state.Version == "" {
		return state, fmt.Errorf("%s at %s: KATL_EXTENSION_ARTIFACT_VERSION is not set", definition(name), revision)
	}
	state.Present = true

	if state.Inputs, err = fingerprint(repo, revision, "katl-extension-build-inputs", name, buildInputs(name)); err != nil {
		return state, err
	}
	if state.Validation, err = fingerprint(repo, revision, "katl-extension-validation-inputs", name, validationInputs); err != nil {
		return state, err
	}
	return state, nil
}

func fingerprint(repo, revision, domain, name string, include []string) (string, error) {
	digests, err := buildinputs.Digests(repo, revision, buildinputs.Spec{
		Domain:  domain,
		Version: fingerprintVersion,
		Include: include,
		Exclude: []string{"_test.go"},
		// The extension name salts the digest, so two extensions with the
		// same declared files never share a fingerprint.
		Salts: map[string]string{name: ""},
	})
	if err != nil {
		return "", err
	}
	return digests[name], nil
}

var (
	plainKey   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	plainValue = regexp.MustCompile(`^[A-Za-z0-9._+:/@%,=-]*$`)
)

// parseDefinition accepts only lines that a shell sourcing the file assigns
// literally: no quoting, expansion, substitution, commands or comments.
func parseDefinition(data []byte) (map[string]string, error) {
	values := map[string]string{}
	for number, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !plainKey.MatchString(key) || !plainValue.MatchString(value) {
			return nil, fmt.Errorf("line %d is not a plain KEY=value assignment: %q", number+1, line)
		}
		if _, ok := values[key]; ok {
			return nil, fmt.Errorf("line %d assigns %s again", number+1, key)
		}
		values[key] = value
	}
	return values, nil
}

// Names returns the extensions defined at any of revisions, in order.
func Names(repo string, revisions ...string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for _, revision := range revisions {
		listed, err := git(repo, "ls-tree", "-rz", "--name-only", revision, "--", "extensions/")
		if err != nil {
			return nil, err
		}
		for entry := range strings.SplitSeq(strings.TrimSuffix(string(listed), "\x00"), "\x00") {
			name, _ := strings.CutPrefix(entry, "extensions/")
			name, ok := strings.CutSuffix(name, "/extension.env")
			if !ok || strings.Contains(name, "/") || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

// Answer is the outcome of comparing a candidate with its base.
type Answer string

const (
	None       Answer = "none"
	NewVersion Answer = "new-version"
	Invalid    Answer = "invalid"
	Unchanged  Answer = "unchanged"
	Revalidate Answer = "revalidate"
	Unproven   Answer = "unproven"
)

// Decision is an Answer with the reason for it.
type Decision struct {
	Answer Answer `json:"decision"`
	Reason string `json:"reason"`
}

// Decide compares a candidate with the commit it is applied to. It returns
// the first answer whose condition holds.
func Decide(base, head State) Decision {
	switch {
	case !head.Present:
		return Decision{None, "no definition at head"}
	case !base.Present:
		return Decision{NewVersion, fmt.Sprintf("new extension at %s", head.Version)}
	case head.Version != base.Version:
		if err := advances(base.Version, head.Version); err != nil {
			return Decision{Invalid, fmt.Sprintf("%v; restoring an earlier version is not allowed, so a revert carries a new, higher KATL_EXTENSION_ARTIFACT_VERSION", err)}
		}
		return Decision{NewVersion, fmt.Sprintf("version advances from %s to %s", base.Version, head.Version)}
	case head.Inputs == base.Inputs && head.Validation == base.Validation:
		return Decision{Unchanged, fmt.Sprintf("inputs unchanged at %s", head.Version)}
	case head.Inputs == base.Inputs:
		return Decision{Revalidate, fmt.Sprintf("validation inputs changed at %s; no new version is required", head.Version)}
	default:
		return Decision{Unproven, fmt.Sprintf("build inputs changed without a new version; advance KATL_EXTENSION_ARTIFACT_VERSION past %s in %s (a revert of an extension change also carries a new, higher version)", head.Version, definition(head.Name))}
	}
}

var artifactVersion = regexp.MustCompile(`^v?([0-9]+(?:\.[0-9]+)*)-katl\.([0-9]+)$`)

// advances reports why head is not a later version than base, or nil if it
// is: its payload version increases, or its katl.N revision increases under
// the same payload version.
func advances(base, head string) error {
	baseParts := artifactVersion.FindStringSubmatch(base)
	headParts := artifactVersion.FindStringSubmatch(head)
	switch {
	case baseParts == nil:
		return fmt.Errorf("base version %q is not <payload>-katl.<N>", base)
	case headParts == nil:
		return fmt.Errorf("version %q is not <payload>-katl.<N>", head)
	}
	order := compareNumbers(strings.Split(headParts[1], "."), strings.Split(baseParts[1], "."))
	if order == 0 {
		order = compareNumbers([]string{headParts[2]}, []string{baseParts[2]})
	}
	if order <= 0 {
		return fmt.Errorf("version %s does not advance past %s", head, base)
	}
	return nil
}

// compareNumbers orders dotted numeric components, treating missing trailing
// components as zero.
func compareNumbers(a, b []string) int {
	for i := range max(len(a), len(b)) {
		var x, y string
		if i < len(a) {
			x = strings.TrimLeft(a[i], "0")
		}
		if i < len(b) {
			y = strings.TrimLeft(b[i], "0")
		}
		// Without leading zeros, a longer decimal number is larger.
		if order := cmp.Compare(len(x), len(y)); order != 0 {
			return order
		}
		if order := strings.Compare(x, y); order != 0 {
			return order
		}
	}
	return 0
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(exit.Stderr))
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
