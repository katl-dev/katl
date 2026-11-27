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
	"time"

	"github.com/katl-dev/katl/internal/gittest"
)

const (
	testRecipe       = "extensions/drbd9/recipe.json"
	testOSRelease    = "mkosi.profiles/runtime/os-release.in"
	testAgentUnit    = "mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katlc-agent.service"
	testActivation   = "internal/generation/activation.go"
	testRuntimeApply = "internal/installer/configapply/runtime_apply.go"
	testInstaller    = "mkosi.profiles/installer-image/mkosi.conf"
	testKatlctl      = "cmd/katlctl/main.go"
	testBIRDReadme   = "extensions/bird/README.md"
	testStandardLock = "kernels/fedora-45/x86_64/standard.json"
	testClusterPlan  = "internal/installer/clusterplan/compile.go"
	testGuestHelper  = "internal/vmtest/guest.go"
	testBootRecords  = "internal/vmtest/guest_records_test.go"
	testSysupdate    = "internal/vmtest/sysupdate_smoke_test.go"
	testSecurity     = "mkosi.profiles/runtime/mkosi.extra/usr/lib/katl/systemd/service-security.conf"
)

const testGoMod = "module github.com/katl-dev/katl\ngo 1.26.0\n"

func TestDriverDigestCandidate(t *testing.T) {
	both := []string{"standard", "lts"}
	for _, tc := range []struct {
		name, path, content string
		want                []string
	}{
		{"agent unit", testAgentUnit, "[Service]\nExecStart=/usr/bin/katlc agent\n", nil},
		{"katlctl only", testKatlctl, "package main\nfunc main() { println(1) }\n", nil},
		{"cluster plan", testClusterPlan, "package clusterplan\nconst extension = \"drbd9\"\n", nil},
		{"Go direct annotation", "go.mod", testGoMod + "require (\n example.org/z v1.1.0\n example.org/a v1.0.0\n)\n", nil},
		{"Go module version", "go.mod", testGoMod + "require (\n example.org/a v1.0.1\n example.org/z v1.1.0 // indirect\n)\n", both},
		{"BIRD documentation", testBIRDReadme, "Updated BIRD documentation\n", nil},
		{"os-release branding", testOSRelease, "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\nPRETTY_NAME=KatlOS\n", nil},
		{"mkosi cache location", "mkosi.conf", "[Distribution]\nRelease=45\n[Build]\nCacheDirectory=new-cache\n", nil},
		{"workflow diagnostics", qualificationWorkflow, testWorkflow("drbd", "upload-new-logs"), nil},
		{"hosted diagnostics collector", ".github/scripts/hosted-vmtest-diagnostics", "#!/bin/sh\nprintf '%s\\n' 'Collecting VM diagnostics'\n", nil},
		{"hosted Go setup", ".github/actions/setup-go/action.yml", "name: Set up Go\n", both},
		{"hosted package setup", ".github/scripts/hosted-apt-install", "#!/bin/sh\nexit 1\n", both},
		{"hosted libvirt setup", ".github/actions/hosted-vmtest/action.yml", "name: Set up libvirt\n", both},
		{"hosted test entrypoint", ".github/scripts/hosted-vmtest-run", "#!/bin/sh\nexit 0\n", both},
		{"hosted Go test wrapper", ".github/scripts/hosted-vmtest-go", "#!/bin/sh\nexit 0\n", both},
		{"hosted test result wrapper", ".github/scripts/hosted-vmtest-measure", "#!/bin/sh\nexit 0\n", both},
		{"VM test runner", "scripts/vmtest-run", "#!/bin/sh\nexit 0\n", both},
		{"VM test executable wrapper", "scripts/vmtest-exec", "#!/bin/sh\nexit 0\n", both},
		{"kernel lock", testStandardLock, "updated standard lock\n", []string{"standard"}},
		{"extension recipe", testRecipe, "{\"version\":\"9.3.5\"}\n", both},
		{"new extension recipe", "extensions/new/recipe.json", "{\"version\":\"1\"}\n", both},
		{"source retry policy", "internal/retry/retry.go", "package retry\n", both},
		{"sysext compatibility", testOSRelease, "ID=katlos\nSYSEXT_LEVEL=katl-runtime-2\n", both},
		{"mkosi architecture", "mkosi.conf", "[Distribution]\nRelease=45\nArchitecture=arm64\n[Build]\nCacheDirectory=cache\n", both},
		{"driver activation", testActivation, "package generation\nconst activation = \"refresh systemd-sysext\"\n", both},
		{"extension activation unit", "internal/systemdunit/units/katl-system-extensions-activate.service", "[Service]\nExecStart=/usr/bin/false\n", both},
		{"candidate sysext staging", testRuntimeApply, "package configapply\nconst sysextDir = \"wrong-extensions\"\n", both},
		{"installer kernel modules", testInstaller, "[Content]\nKernelModules=\n        /drivers/block/\n", both},
		{"DRBD guest helper", testGuestHelper, "package vmtest\nconst readFile = \"new helper\"\n", both},
		{"hermetic runner", "scripts/hermetic", "#!/bin/sh\nexec bwrap -- \"$@\"\n", both},
		{"boot selection helper", testBootRecords, "package vmtest\nfunc bootSelectionFromGuest() string { return \"new-record\" }\n", both},
		{"sysupdate smoke only", testSysupdate, "package vmtest\nfunc TestInstalledRuntimeSysupdateRootUKITransfer(t *testing.T) { t.Fatal(\"transfer failed\") }\n", nil},
		{"activation security symlink target", testSecurity, "[Service]\nNoNewPrivileges=no\n", both},
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
		lookup           func(map[string]string, bool) (map[string]driverQualification, error)
		wantLookup       []string
		want             []string
	}{
		{"qualified main push", "push", "refs/heads/main", nil, qualified, both, nil},
		{"missing on main push", "push", "refs/heads/main", nil, fixtureDriverLookup("", nil), both, both},
		{"missing on PR", "pull_request", "refs/pull/1/merge", nil, fixtureDriverLookup("", nil), both, both},
		{"release push", "push", "refs/heads/release/2026", nil, qualified, nil, nil},
		{"lookup error", "pull_request", "refs/pull/1/merge", nil, func(map[string]string, bool) (map[string]driverQualification, error) {
			return nil, errors.New("status lookup failed")
		}, both, both},
		{"forced lock", "merge_group", "", []string{testStandardLock}, qualified, []string{"lts"}, []string{"standard"}},
		{"forced main lock with historical success", "push", "refs/heads/main", []string{testStandardLock}, qualified, both, []string{"standard"}},
		{"scheduled", "schedule", "refs/heads/main", nil, qualified, nil, both},
		{"dispatched", "workflow_dispatch", "refs/heads/topic", nil, qualified, nil, both},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requested []string
			plan := ciPlan{}
			plan.selectDrivers(tc.paths, tc.event, tc.ref, currentDigests, func(wanted map[string]string, mainPush bool) (map[string]driverQualification, error) {
				for _, flavour := range both {
					if _, ok := wanted[flavour]; ok {
						requested = append(requested, flavour)
					}
				}
				return tc.lookup(wanted, mainPush)
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
	body := fmt.Sprintf(`#!/bin/sh
case "$*" in *%s*) exit 1;; esac
case "$2" in
  graphql) printf '%%s\n' "$DRIVER_STATUS_RESPONSE";;
  */runs/101/attempts/1) printf '%%s\n' "$DRIVER_RUN_RESPONSE";;
  */runs/102/attempts/1) printf '%%s\n' "$PENDING_RUN_RESPONSE";;
  */runs/101/attempts/1/jobs\?*) printf '%%s\n' "$DRIVER_JOBS_RESPONSE";;
  */runs/102/attempts/1/jobs\?*) printf '%%s\n' "$PENDING_JOBS_RESPONSE";;
  *) exit 1;;
esac
`, side)
	writeDriverInput(t, filepath.Join(stub, "gh"), body, 0o755)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_REPOSITORY", "katl-dev/katl")
	t.Setenv("GITHUB_EVENT_NAME", "pull_request")
	t.Setenv("GITHUB_REF", "refs/pull/1/merge")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	for _, tc := range []struct {
		name, state, conclusion, reason string
		age                             time.Duration
		want                            []string
	}{
		{"live producer", "in_progress", "", "same-digest-qualification-in-progress", time.Minute, nil},
		{"failed producer", "completed", "failure", "producer-not-live", time.Minute, []string{"lts"}},
		{"cancelled producer", "completed", "cancelled", "producer-not-live", time.Minute, []string{"lts"}},
		{"completed without a success record", "completed", "success", "producer-not-live", time.Minute, []string{"lts"}},
		{"stale producer", "in_progress", "", "producer-not-live", 91 * time.Minute, []string{"lts"}},
		{"unavailable producer", "", "", "producer-unavailable", time.Minute, []string{"lts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			producer := driverRunFixture(102, 1, "schedule", "main", base, tc.state, tc.conclusion)
			if tc.state == "" {
				producer = "malformed"
			} else {
				started := time.Now().Add(-tc.age).UTC().Format(time.RFC3339)
				producer = strings.TrimSuffix(producer, "}") + fmt.Sprintf(`,"run_started_at":%q}`, started)
			}
			t.Setenv("DRIVER_RUN_RESPONSE", driverRunFixture(101, 1, "schedule", "main", base, "completed", "success"))
			t.Setenv("DRIVER_JOBS_RESPONSE", driverJobsFixture("standard", digests["standard"], "completed", "success"))
			t.Setenv("PENDING_RUN_RESPONSE", producer)
			t.Setenv("PENDING_JOBS_RESPONSE", driverJobsFixture("lts", digests["lts"], tc.state, tc.conclusion))
			date := time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339)
			t.Setenv("DRIVER_STATUS_RESPONSE", fmt.Sprintf(
				`{"data":{"repository":{"c0":{"oid":%q,"status":null},"c1":{"oid":%q,"status":{
					"standard":%s,"lts":%s
				}}}}}`, main, base, driverStatusFixture("github-actions", "SUCCESS", digests["standard"], date, 101),
				driverStatusFixture("github-actions", "PENDING", digests["lts"], date, 102)))

			plan := readDriverPlan(t, testBIRDReadme)
			if plan.QualifiedDrivers["standard"] != base || plan.QualifiedDrivers["lts"] != "" || !slices.Equal(plan.DriverFlavours, tc.want) {
				t.Fatalf("plan = %+v, want prior success for standard and driver matrix %v", plan, tc.want)
			}
			decision := "skip"
			if len(tc.want) != 0 {
				decision = "run"
			}
			wantLog := fmt.Sprintf("driver-qualification flavour=lts digest=%q record_sha=%q record_context=%q record_state=\"PENDING\" producer_run=\"https://github.com/katl-dev/katl/actions/runs/102/attempts/1\" decision=%s reason=%s",
				digests["lts"], base, "katl/driver-qualification/lts/"+digests["lts"], decision, tc.reason)
			if len(plan.DriverDecisions) != 2 || plan.DriverDecisions[1] != wantLog {
				t.Errorf("decisions = %v, want %s", plan.DriverDecisions, wantLog)
			}
		})
	}
}

func TestMainAdoptsExactMergeGroupQualification(t *testing.T) {
	base := newDriverRepo(t)
	writeDriverInput(t, testStandardLock, "new standard lock\n", 0o644)
	older := commitDriverRepo(t)
	writeDriverInput(t, testBIRDReadme, "Documentation in the merge group\n", 0o644)
	head := commitDriverRepo(t)
	driverGit(t, "update-ref", "refs/remotes/origin/main", older)
	digests := readDriverDigests(t)

	stub := t.TempDir()
	writeDriverInput(t, filepath.Join(stub, "gh"), `#!/bin/sh
case "$2" in
  graphql)
    case "$*" in *"c0: object(oid: \"$DRIVER_HEAD\")"*) ;; *) exit 1;; esac
    printf '%s\n' "$DRIVER_STATUS_RESPONSE";;
  */runs/101/attempts/1) printf '%s\n' "$HISTORICAL_RUN";;
  */runs/102/attempts/1) printf '%s\n' "$EXACT_RUN";;
  */runs/103/attempts/1) printf '%s\n' "$MAIN_RUN";;
  */runs/10[12]/attempts/1/jobs\?*) printf '%s\n' "$STANDARD_JOBS";;
  */runs/103/attempts/1/jobs\?*) printf '%s\n' "$LTS_JOBS";;
  *) exit 1;;
esac
`, 0o755)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_REPOSITORY", "katl-dev/katl")
	t.Setenv("GITHUB_EVENT_NAME", "push")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("DRIVER_HEAD", head)
	const queue = "gh-readonly-queue/main/pr-1-base"
	const bot = "github-actions"
	t.Setenv("HISTORICAL_RUN", driverRunFixture(101, 1, "merge_group", queue, older, "completed", "success"))
	t.Setenv("MAIN_RUN", driverRunFixture(103, 1, "push", "main", older, "completed", "success"))
	t.Setenv("STANDARD_JOBS", driverJobsFixture("standard", digests["standard"], "completed", "success"))
	t.Setenv("LTS_JOBS", driverJobsFixture("lts", digests["lts"], "completed", "success"))
	for _, tc := range []struct {
		name, sha, branch, state, digest string
		attempt                          int
		adopt                            bool
		path                             string
	}{
		{"exact successful result", head, queue, "SUCCESS", digests["standard"], 1, true, testStandardLock},
		{"historical result only", older, queue, "", "", 1, false, testBIRDReadme},
		{"wrong digest", head, queue, "SUCCESS", "wrong digest", 1, false, testStandardLock},
		{"failure", head, queue, "FAILURE", digests["standard"], 1, false, testStandardLock},
		{"wrong producer commit", older, queue, "SUCCESS", digests["standard"], 1, false, testBIRDReadme},
		{"wrong queue", head, "gh-readonly-queue/release/2026/pr-1-base", "SUCCESS", digests["standard"], 1, false, testStandardLock},
		{"wrong run attempt", head, queue, "SUCCESS", digests["standard"], 2, false, testStandardLock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exactStatus := "null"
			if tc.state != "" {
				exactStatus = `{"standard":` + driverStatusFixture(bot, tc.state, tc.digest, "2026-11-23T02:00:00Z", 102) + `}`
			}
			t.Setenv("EXACT_RUN", driverRunFixture(102, tc.attempt, "merge_group", tc.branch, tc.sha, "completed", "success"))
			t.Setenv("DRIVER_STATUS_RESPONSE", fmt.Sprintf(
				`{"data":{"repository":{"c0":{"oid":%q,"status":%s},"c1":{"oid":%q,"status":{"standard":%s,"lts":%s}},"c2":{"oid":%q,"status":null}}}}`,
				head, exactStatus, older,
				driverStatusFixture(bot, "SUCCESS", digests["standard"], "2026-11-23T01:00:00Z", 101),
				driverStatusFixture(bot, "SUCCESS", digests["lts"], "2026-11-23T01:00:00Z", 103), base))

			plan := readDriverPlan(t, tc.path)
			if plan.QualifiedDrivers["lts"] != older {
				t.Fatalf("LTS qualification = %+v, want %s", plan, older)
			}
			if tc.adopt {
				if len(plan.DriverFlavours) != 0 || plan.QualifiedDrivers["standard"] != head ||
					!strings.Contains(plan.DriverDecisions[0], `/runs/102/attempts/1" decision=skip reason=same-digest-merge-group-success`) {
					t.Fatalf("want main to adopt the queue run for %s, got %+v", head, plan)
				}
			} else if !slices.Equal(plan.DriverFlavours, []string{"standard"}) || plan.QualifiedDrivers["standard"] != "" {
				t.Fatalf("want a fresh standard qualification, got %+v", plan)
			}
		})
	}
}

func TestDriverQualificationStatusAuthority(t *testing.T) {
	newDriverRepo(t)
	old := driverGit(t, "rev-parse", "HEAD")
	writeDriverInput(t, testBIRDReadme, "Intervening commit\n", 0o644)
	head := commitDriverRepo(t)
	driverGit(t, "update-ref", "refs/remotes/origin/main", head)
	digest := readDriverDigests(t)["standard"]

	stub := t.TempDir()
	writeDriverInput(t, filepath.Join(stub, "gh"), `#!/bin/sh
case "$2" in
  graphql) printf '%s\n' "$DRIVER_STATUS_RESPONSE";;
  */runs/101/attempts/1) printf '%s\n' "$OLD_RUN";;
  */runs/102/attempts/1) printf '%s\n' "$HEAD_RUN";;
  */runs/10[12]/attempts/1/jobs\?*) printf '%s\n' "$STANDARD_JOBS";;
  *) exit 1;;
esac
`, 0o755)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_REPOSITORY", "katl-dev/katl")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("OLD_RUN", driverRunFixture(101, 1, "schedule", "main", old, "completed", "success"))
	t.Setenv("HEAD_RUN", driverRunFixture(102, 1, "schedule", "main", head, "completed", "success"))
	t.Setenv("STANDARD_JOBS", driverJobsFixture("standard", digest, "completed", "success"))
	status := func(creator, state, date string, id int) string {
		return `{"standard":` + driverStatusFixture(creator, state, digest, date, id) + `}`
	}
	const bot = "github-actions"
	const earlier = "2026-11-23T01:00:00Z"
	const later = "2026-11-23T02:00:00Z"
	for _, tc := range []struct {
		name, headStatus, oldStatus, want string
	}{
		{"trusted publisher", status(bot, "SUCCESS", later, 102), "null", head},
		{"other writer", status("someone-else", "SUCCESS", later, 102), "null", ""},
		{"wrong commit", status(bot, "SUCCESS", later, 101), "null", ""},
		{"wrong branch", status(bot, "SUCCESS", later, 102), "null", ""},
		{"other writer shadows failure", status("someone-else", "SUCCESS", later, 102), status(bot, "SUCCESS", earlier, 101), ""},
		{"unreadable replacement", strings.Replace(status(bot, "SUCCESS", later, 102), digest, "unreadable", 1), status(bot, "SUCCESS", earlier, 101), ""},
		{"failed scheduled requalification", status(bot, "FAILURE", later, 102), status(bot, "SUCCESS", earlier, 101), ""},
		{"pending scheduled requalification", status(bot, "PENDING", later, 102), status(bot, "SUCCESS", earlier, 101), ""},
		{"newer failure from a run on an older commit", status(bot, "SUCCESS", earlier, 102), status(bot, "FAILURE", later, 101), ""},
		{"newer success supersedes failure", status(bot, "FAILURE", earlier, 102), status(bot, "SUCCESS", later, 101), old},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "wrong branch" {
				t.Setenv("HEAD_RUN", driverRunFixture(102, 1, "schedule", "topic", head, "completed", "success"))
			}
			t.Setenv("DRIVER_STATUS_RESPONSE", fmt.Sprintf(
				`{"data":{"repository":{"c0":{"oid":%q,"status":%s},"c1":{"oid":%q,"status":%s}}}}`,
				head, tc.headStatus, old, tc.oldStatus))
			got, err := mainDriverQualifications(map[string]string{"standard": digest}, false)
			reused := ""
			if got["standard"].reason == "same-digest-success" {
				reused = got["standard"].sha
			}
			if err != nil || reused != tc.want {
				t.Fatalf("qualification = %v, %v, want standard commit %q", got, err, tc.want)
			}
		})
	}
}

func TestDriverQualificationNeedsMatchingJob(t *testing.T) {
	head := newDriverRepo(t)
	driverGit(t, "update-ref", "refs/remotes/origin/main", head)
	digests := readDriverDigests(t)
	digest := digests["standard"]
	stub := t.TempDir()
	writeDriverInput(t, filepath.Join(stub, "gh"), `#!/bin/sh
case "$2" in
  graphql) printf '%s\n' "$DRIVER_STATUS_RESPONSE";;
  repos/katl-dev/katl/actions/runs/101/attempts/1) printf '%s\n' "$DRIVER_RUN_RESPONSE";;
  "repos/katl-dev/katl/actions/runs/101/attempts/1/jobs?per_page=100") printf '%s\n' "$DRIVER_JOBS_RESPONSE";;
  *) exit 1;;
esac
`, 0o755)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_REPOSITORY", "katl-dev/katl")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_EVENT_NAME", "pull_request")
	t.Setenv("GITHUB_REF", "refs/pull/1/merge")
	now := time.Now().UTC()
	const standardJob = "Drivers (standard) / drivers"
	jobName := standardJob + " (" + digest + ")"
	for _, tc := range []struct {
		name, state                 string
		runStatus, runConclusion    string
		jobName, status, conclusion string
		wantReuse                   bool
	}{
		{"successful job in failed CI", "SUCCESS", "completed", "failure", jobName, "completed", "success", true},
		{"skipped job in successful CI", "SUCCESS", "completed", "success", jobName, "completed", "skipped", false},
		{"failed job", "SUCCESS", "completed", "failure", jobName, "completed", "failure", false},
		{"different flavour", "SUCCESS", "completed", "success", "Drivers (lts) / drivers (" + digests["lts"] + ")", "completed", "success", false},
		{"different digest", "SUCCESS", "completed", "success", standardJob + " (" + digests["lts"] + ")", "completed", "success", false},
		{"only status writer passed", "SUCCESS", "completed", "success", "Drivers (standard) / Record final driver qualification", "completed", "success", false},
		{"skipped job in live CI", "PENDING", "in_progress", "", jobName, "completed", "skipped", false},
		{"missing job in live CI", "PENDING", "in_progress", "", "", "", "", false},
		{"finished job with pending record", "PENDING", "in_progress", "", jobName, "completed", "success", false},
		{"queued job", "PENDING", "in_progress", "", jobName, "queued", "", true},
		{"running job", "PENDING", "in_progress", "", jobName, "in_progress", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := driverRunFixture(101, 1, "schedule", "main", head, tc.runStatus, tc.runConclusion)
			run = strings.TrimSuffix(run, "}") + fmt.Sprintf(`,"run_started_at":%q}`, now.Add(-time.Minute).Format(time.RFC3339))
			t.Setenv("DRIVER_RUN_RESPONSE", run)
			jobs := `[{"jobs":[]}]`
			if tc.jobName != "" {
				jobs = fmt.Sprintf(`[{"jobs":[{"name":%q,"status":%q,"conclusion":%q}]}]`, tc.jobName, tc.status, tc.conclusion)
			}
			t.Setenv("DRIVER_JOBS_RESPONSE", jobs)
			t.Setenv("DRIVER_STATUS_RESPONSE", fmt.Sprintf(`{"data":{"repository":{"c0":{"oid":%q,"status":{"standard":%s}}}}}`,
				head, driverStatusFixture("github-actions", tc.state, digest, now.Format(time.RFC3339), 101)))

			plan := readDriverPlan(t, testBIRDReadme)
			if reused := !slices.Contains(plan.DriverFlavours, "standard"); reused != tc.wantReuse {
				t.Fatalf("standard reused = %v, want %v: %v", reused, tc.wantReuse, plan.DriverDecisions)
			}
		})
	}
}

func TestDriverQualificationTiedFailure(t *testing.T) {
	now := time.Now()
	const digest = "standard-digest"
	for _, states := range [][2]string{{"PENDING", "FAILURE"}, {"FAILURE", "PENDING"}} {
		t.Run(strings.Join(states[:], "-"), func(t *testing.T) {
			history := []string{"head", "parent"}
			readStatuses := func([]string) ([]commitDriverStatuses, error) {
				statuses := make([]commitDriverStatuses, len(history))
				for i, sha := range history {
					statuses[i] = commitDriverStatuses{OID: sha, Status: map[string]*driverStatus{
						"standard": {State: states[i], Description: digest, CreatedAt: now},
					}}
				}
				return statuses, nil
			}
			got, err := findDriverQualifications(history, map[string]string{"standard": digest}, "", now, readStatuses,
				func(string, string, driverStatus) (driverProducer, error) {
					return driverProducer{event: "push", jobStatus: "in_progress", startedAt: now.Add(-time.Minute)}, nil
				})
			if err != nil || got["standard"].reason != "record-failed" {
				t.Fatalf("want tied failure to require a fresh run, got %v, %v", got, err)
			}
		})
	}
}

func readDriverPlan(t *testing.T, path string) ciPlan {
	t.Helper()
	var result bytes.Buffer
	if err := run([]string{"--ci", "--vm-mode", "off"}, strings.NewReader(path+"\x00"), &result); err != nil {
		t.Fatal(err)
	}
	var plan ciPlan
	if err := json.Unmarshal(result.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func driverStatusFixture(creator, state, digest, date string, id int) string {
	return fmt.Sprintf(`{"state":%q,"description":%q,"creator":{"login":%q,"type":"Bot","databaseId":41898282},
		"createdAt":%q,"targetUrl":"https://github.com/katl-dev/katl/actions/runs/%d/attempts/1"}`, state, digest, creator, date, id)
}

func driverRunFixture(id, attempt int, event, branch, sha, state, conclusion string) string {
	return fmt.Sprintf(`{"id":%d,"run_attempt":%d,"path":".github/workflows/ci.yml","head_branch":%q,
		"head_sha":%q,"event":%q,"status":%q,"conclusion":%q,"repository":{"full_name":"katl-dev/katl"},
		"head_repository":{"full_name":"katl-dev/katl"}}`, id, attempt, branch, sha, event, state, conclusion)
}

func driverJobsFixture(flavour, digest, status, conclusion string) string {
	name := fmt.Sprintf("Drivers (%s) / drivers (%s)", flavour, digest)
	return fmt.Sprintf(`[{"jobs":[{"name":%q,"status":%q,"conclusion":%q}]}]`, name, status, conclusion)
}

func fixtureDriverLookup(commit string, records map[string]string) func(map[string]string, bool) (map[string]driverQualification, error) {
	return func(wanted map[string]string, _ bool) (map[string]driverQualification, error) {
		matches := map[string]driverQualification{}
		for flavour, digest := range wanted {
			if records[flavour] == digest {
				matches[flavour] = driverQualification{sha: commit, reason: "same-digest-success"}
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
	writeDriverInput(t, "go.mod", testGoMod+"require example.org/a v1.0.0\nrequire example.org/z v1.1.0 // indirect\n", 0o644)
	writeDriverInput(t, testKatlctl, "package main\nfunc main() {}\n", 0o644)
	writeDriverInput(t, testActivation, "package generation\nconst activation = \"activate systemd-sysext\"\n", 0o644)
	writeDriverInput(t, testRuntimeApply, "package configapply\nconst sysextDir = \"extensions\"\n", 0o644)
	writeDriverInput(t, testInstaller, "[Content]\nKernelModules=\n        /drivers/block/\n        /drivers/virtio/\n", 0o644)
	writeDriverInput(t, testClusterPlan, "package clusterplan\nconst extension = \"none\"\n", 0o644)
	writeDriverInput(t, "internal/vmtest/drbd_smoke_test.go", "package vmtest\nvar observed = readFile\n", 0o644)
	writeDriverInput(t, testGuestHelper, "package vmtest\nconst readFile = \"guest helper\"\n", 0o644)
	writeDriverInput(t, testBootRecords, "package vmtest\nfunc bootSelectionFromGuest() string { return \"record\" }\n", 0o644)
	writeDriverInput(t, testSysupdate, "package vmtest\nfunc TestInstalledRuntimeSysupdateRootUKITransfer(t *testing.T) {}\n", 0o644)
	writeDriverInput(t, testSecurity, "[Service]\nNoNewPrivileges=yes\n", 0o644)
	const dropIn = "mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-system-extensions-activate.service.d/10-security.conf"
	if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../katl/systemd/service-security.conf", dropIn); err != nil {
		t.Fatal(err)
	}
	writeDriverInput(t, testBIRDReadme, "BIRD documentation\n", 0o644)
	writeDriverInput(t, qualificationWorkflow, testWorkflow("drbd", "upload-logs"), 0o644)
	writeDriverInput(t, ".github/scripts/hosted-apt-install", "#!/bin/sh\nsudo apt-get install \"$@\"\n", 0o644)
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
