package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

const (
	testRecipe       = "extensions/drbd9/recipe.json"
	testOSRelease    = "mkosi.profiles/runtime/os-release.in"
	testAgentUnit    = "mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katlc-agent.service"
	testAgentCode    = "internal/katlc/agent/executor.go"
	testBIRDReadme   = "extensions/bird/README.md"
	testStandardLock = "kernels/fedora-45/x86_64/standard.json"
)

func TestDriverDigestCandidate(t *testing.T) {
	both := []string{"standard", "lts"}
	for _, tc := range []struct {
		name, path, content string
		want                []string
	}{
		{"agent unit", testAgentUnit, "[Service]\nExecStart=/usr/bin/katlc agent\n", nil},
		{"BIRD documentation", testBIRDReadme, "Updated BIRD documentation\n", nil},
		{"os-release branding", testOSRelease, "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\nPRETTY_NAME=KatlOS\n", nil},
		{"mkosi cache location", "mkosi.conf", "[Distribution]\nRelease=45\n[Build]\nCacheDirectory=new-cache\n", nil},
		{"workflow diagnostics", qualificationWorkflow, testWorkflow("drbd", "upload-new-logs"), nil},
		{"extension recipe", testRecipe, "{\"version\":\"9.3.5\"}\n", both},
		{"new extension recipe", "extensions/new/recipe.json", "{\"version\":\"1\"}\n", both},
		{"sysext compatibility", testOSRelease, "ID=katlos\nSYSEXT_LEVEL=katl-runtime-2\n", both},
		{"mkosi architecture", "mkosi.conf", "[Distribution]\nRelease=45\nArchitecture=arm64\n[Build]\nCacheDirectory=cache\n", both},
		{"driver activation", testAgentCode, "refresh systemd-sysext\n", both},
		{"qualification command", qualificationWorkflow, testWorkflow("drbd-new", "upload-logs"), both},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newDriverRepo(t)
			baseDigests := readDriverDigests(t)
			writeDriverInput(t, tc.path, tc.content, 0o644)
			commitDriverRepo(t)

			plan := ciPlan{}
			plan.selectDrivers([]string{tc.path}, "pull_request", "refs/pull/1/merge",
				committedDriverDigests, fixtureDriverLookup(base, baseDigests))
			if !slices.Equal(plan.DriverFlavours, tc.want) || plan.DriverFlavours == nil {
				t.Fatalf("driver matrix = %v, want %v", plan.DriverFlavours, tc.want)
			}
			for _, flavour := range both {
				if slices.Contains(tc.want, flavour) {
					if plan.DriverDigests[flavour] == baseDigests[flavour] || plan.QualifiedDrivers[flavour] != "" {
						t.Errorf("%s had changed inputs but plan was %+v", flavour, plan)
					}
				} else if plan.QualifiedDrivers[flavour] != base || plan.DriverDigests[flavour] != baseDigests[flavour] {
					t.Errorf("%s should have reused qualification on %s: %+v", flavour, base, plan)
				}
			}
		})
	}
}

func TestDriverDigestTracksPathModeAndLock(t *testing.T) {
	for _, change := range []string{"rename", "remove", "chmod", "lock"} {
		t.Run(change, func(t *testing.T) {
			newDriverRepo(t)
			before := readDriverDigests(t)
			const script = "scripts/build-release-extensions"
			switch change {
			case "rename":
				const src = "mkosi.profiles/kernel-extension-drbd9/mkosi.build"
				if err := os.Rename(src, src+".renamed"); err != nil {
					t.Fatal(err)
				}
			case "remove":
				if err := os.Remove(testRecipe); err != nil {
					t.Fatal(err)
				}
			case "chmod":
				if err := os.Chmod(script, 0o644); err != nil {
					t.Fatal(err)
				}
			case "lock":
				writeDriverInput(t, testStandardLock, "new standard lock\n", 0o644)
			}
			commitDriverRepo(t)
			after := readDriverDigests(t)
			if before["standard"] == after["standard"] || (before["lts"] == after["lts"]) != (change == "lock") {
				t.Fatalf("digest change for %s: before = %v, after = %v", change, before, after)
			}
		})
	}
}

func TestDriverQualificationPlan(t *testing.T) {
	both := []string{"standard", "lts"}
	digests := map[string]string{"standard": "standard-digest", "lts": "lts-digest"}
	currentDigests := func() (map[string]string, error) { return digests, nil }
	qualified := fixtureDriverLookup("qualified-main", digests)
	for _, tc := range []struct {
		name, event, ref string
		paths            []string
		lookup           func(map[string]string) (map[string]string, error)
		wantLookup       []string
		want             []string
	}{
		{"qualified main push", "push", "refs/heads/main", nil, qualified, both, nil},
		{"missing on main push", "push", "refs/heads/main", nil, fixtureDriverLookup("", nil), both, both},
		{"missing on PR", "pull_request", "refs/pull/1/merge", nil, fixtureDriverLookup("", nil), both, both},
		{"release push", "push", "refs/heads/release/2026", nil, qualified, nil, nil},
		{"lookup error", "pull_request", "refs/pull/1/merge", nil, func(map[string]string) (map[string]string, error) {
			return nil, errors.New("status lookup failed")
		}, both, both},
		{"forced lock", "merge_group", "", []string{testStandardLock}, qualified, []string{"lts"}, []string{"standard"}},
		{"scheduled", "schedule", "refs/heads/main", nil, qualified, nil, both},
		{"dispatched", "workflow_dispatch", "refs/heads/topic", nil, qualified, nil, both},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requested []string
			plan := ciPlan{}
			plan.selectDrivers(tc.paths, tc.event, tc.ref, currentDigests, func(wanted map[string]string) (map[string]string, error) {
				for _, flavour := range both {
					if _, ok := wanted[flavour]; ok {
						requested = append(requested, flavour)
					}
				}
				return tc.lookup(wanted)
			})

			if !slices.Equal(plan.DriverFlavours, tc.want) || plan.DriverFlavours == nil || !slices.Equal(requested, tc.wantLookup) {
				t.Fatalf("matrix %v and lookup %v, want %v and %v", plan.DriverFlavours, requested, tc.want, tc.wantLookup)
			}
		})
	}
}

func TestDriverQualificationMainHistory(t *testing.T) {
	base := newDriverRepo(t)
	digests := readDriverDigests(t)
	driverGit(t, "switch", "-c", "side")
	writeDriverInput(t, testBIRDReadme, "Update on side branch\n", 0o644)
	side := commitDriverRepo(t)
	driverGit(t, "switch", "main")
	driverGit(t, "merge", "--no-ff", "-m", "intervening skipped commit", "side")
	main := driverGit(t, "rev-parse", "HEAD")
	driverGit(t, "update-ref", "refs/remotes/origin/main", main)

	stub := t.TempDir()
	body := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *%s*) exit 1;; esac\nprintf '%%s\\n' \"$DRIVER_STATUS_RESPONSE\"\n", side)
	writeDriverInput(t, filepath.Join(stub, "gh"), body, 0o755)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_REPOSITORY", "katl-dev/katl")
	t.Setenv("GITHUB_EVENT_NAME", "pull_request")
	t.Setenv("GITHUB_REF", "refs/pull/1/merge")
	t.Setenv("DRIVER_STATUS_RESPONSE", fmt.Sprintf(
		`{"data":{"repository":{"c0":{"oid":%q,"status":null},"c1":{"oid":%q,"status":{
			"standard":{"state":"SUCCESS","description":%q},"lts":{"state":"PENDING","description":%q}
		}}}}}`, main, base, digests["standard"], digests["lts"]))

	var result bytes.Buffer
	if err := run([]string{"--ci"}, strings.NewReader(testBIRDReadme+"\x00"), &result); err != nil {
		t.Fatal(err)
	}
	var plan ciPlan
	if err := json.Unmarshal(result.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.QualifiedDrivers["standard"] != base || plan.QualifiedDrivers["lts"] != "" ||
		!slices.Equal(plan.DriverFlavours, []string{"lts"}) {
		t.Fatalf("want older main record for standard and a one-flavour matrix for pending LTS, got %s", &result)
	}
}

func fixtureDriverLookup(commit string, records map[string]string) func(map[string]string) (map[string]string, error) {
	return func(wanted map[string]string) (map[string]string, error) {
		matches := map[string]string{}
		for flavour, digest := range wanted {
			if records[flavour] == digest {
				matches[flavour] = commit
			}
		}
		return matches, nil
	}
}

func newDriverRepo(t *testing.T) string {
	t.Helper()
	releaseScript, err := os.ReadFile("../../scripts/fedora-release")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	for _, entry := range gittest.Env(nil, dir) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	driverGit(t, "init", "-q", "-b", "main")
	driverGit(t, "config", "core.fileMode", "true")

	writeDriverInput(t, "scripts/fedora-release", string(releaseScript), 0o755)
	writeDriverInput(t, "scripts/build-release-extensions", "#!/bin/sh\n", 0o755)
	writeDriverInput(t, "mkosi.profiles/kernel-extension-drbd9/mkosi.build", "#!/bin/sh\n", 0o755)
	writeDriverInput(t, "mkosi.conf", "[Distribution]\nRelease=45\n[Build]\nCacheDirectory=cache\n", 0o644)
	writeDriverInput(t, testStandardLock, "standard kernel lock\n", 0o644)
	writeDriverInput(t, "kernels/fedora-45/x86_64/lts.json", "lts kernel lock\n", 0o644)
	writeDriverInput(t, testRecipe, "{\"version\":\"9.3.4\"}\n", 0o644)
	writeDriverInput(t, testOSRelease, "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\n", 0o644)
	writeDriverInput(t, testAgentUnit, "[Service]\nExecStart=/usr/bin/katlc\n", 0o644)
	writeDriverInput(t, testAgentCode, "activate systemd-sysext\n", 0o644)
	writeDriverInput(t, testBIRDReadme, "BIRD documentation\n", 0o644)
	writeDriverInput(t, qualificationWorkflow, testWorkflow("drbd", "upload-logs"), 0o644)
	return commitDriverRepo(t)
}

func testWorkflow(command, diagnostics string) string {
	return fmt.Sprintf("jobs:\n  drivers:\n    steps:\n      - run: %s\n      - id: upload-diagnostics\n        run: %s\n", command, diagnostics)
}

func writeDriverInput(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func readDriverDigests(t *testing.T) map[string]string {
	t.Helper()
	digests, err := committedDriverDigests()
	if err != nil || digests["standard"] == "" || digests["lts"] == "" || digests["standard"] == digests["lts"] {
		t.Fatalf("distinct driver digests required, got %v, %v", digests, err)
	}
	return digests
}

func commitDriverRepo(t *testing.T) string {
	t.Helper()
	driverGit(t, "add", "--all")
	driverGit(t, "commit", "-qm", "qualification fixture")
	return driverGit(t, "rev-parse", "HEAD")
}

func driverGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
