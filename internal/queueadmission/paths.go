package queueadmission

import (
	"path"
	"regexp"
	"strings"
)

type pathClass int

const (
	classReviewRequired pathClass = iota
	classMergeAlone
)

type pathRow struct {
	globs  []string
	class  pathClass
	reason string
}

// pathRows classifies changed paths. A glob ending in /** matches every path
// below its prefix; any other glob is matched with path.Match, so * stays
// within one segment. A path may match rows of both classes.
var pathRows = []pathRow{
	{[]string{".github/workflows/**", ".github/actions/**", ".github/scripts/**", ".github/rulesets/**"},
		classReviewRequired, "workflows"},
	{[]string{"internal/queueadmission/**", "internal/reviewrecord/**", "internal/queuemarker/**",
		"cmd/katl-queue/facts.go", "cmd/katl-queue/conflicts.go", "cmd/katl-queue/simulate.go"},
		classReviewRequired, "admission"},
	{[]string{"internal/efiboot/**", "internal/boothealth/**", "cmd/katl-boot-health/**", "internal/kernelcmdline/**"},
		classReviewRequired, "boot"},
	{[]string{"internal/systemdisk/**", "cmd/katl-system-disk-generator/**", "internal/installer/**", "cmd/katlos-install/**"},
		classReviewRequired, "disk"},
	{[]string{"internal/generation/**", "cmd/katl-generation-activate/**", "internal/releasestate/**", "internal/sysextimage/**"},
		classReviewRequired, "update"},
	{[]string{"internal/managementidentity/**", "internal/sshhostkey/**", "internal/katlc/transport/**",
		"containers-policy.json", "scripts/katl-release-artifacts"},
		classReviewRequired, "security and trust"},
	{[]string{"kernels/**", "mkosi-builder.lock", "mkosi.conf", "mkosi.profiles/runtime/**", "mkosi.profiles/installer-image/**"},
		classReviewRequired, "boot inputs"},
	// These are not sharedVMPath in cmd/katl-vmtest-plan/vm_selection.go:
	// that list answers which suites a change runs, while this one answers
	// which changes alter what every merge group runs. internal/vmtest is
	// deliberately absent.
	{[]string{".github/workflows/ci.yml", ".github/workflows/hosted-vmtests.yml", ".github/actions/hosted-vmtest/**",
		".github/scripts/hosted-vmtest-*", "scripts/vmtest-run", "scripts/vmtest-exec", "Containerfile.mkosi", "mkosi-builder.lock"},
		classMergeAlone, "changes what every merge group runs"},
}

func globMatch(glob, p string) bool {
	if prefix, ok := strings.CutSuffix(glob, "/**"); ok {
		n := strings.Count(prefix, "/") + 1
		segments := strings.Split(p, "/")
		if len(segments) <= n {
			return false
		}
		matched, _ := path.Match(prefix, strings.Join(segments[:n], "/"))
		return matched
	}
	matched, _ := path.Match(glob, p)
	return matched
}

// classify returns the reasons of the rows of class that p matches.
func classify(p string, class pathClass) []string {
	var reasons []string
	for _, row := range pathRows {
		if row.class != class {
			continue
		}
		for _, glob := range row.globs {
			if globMatch(glob, p) {
				reasons = append(reasons, row.reason)
				break
			}
		}
	}
	return reasons
}

// automationLogin is the automation App's login. kernel-automerge.yml derives
// it from the App token at run time; the policy holds it as a constant.
const automationLogin = "katl-bot"

// automationBranches restate the allowlists of kernel-automerge.yml, which
// admits these pull requests itself. A parity test holds the two equal.
var automationBranches = []struct {
	prefix  string
	allowed *regexp.Regexp
}{
	{"automation/kernels-", regexp.MustCompile(`^kernels/fedora-[0-9]+/x86_64/(standard|lts)\.json$`)},
	{"automation/mkosi-builder-", regexp.MustCompile(`^mkosi-builder\.lock$`)},
}

// automationPaths reports whether headRef is an automation branch and every
// one of files is a path that branch family may change.
func automationPaths(headRef string, files []string) bool {
	for _, b := range automationBranches {
		if !strings.HasPrefix(headRef, b.prefix) {
			continue
		}
		for _, f := range files {
			if !b.allowed.MatchString(f) {
				return false
			}
		}
		return len(files) > 0
	}
	return false
}
