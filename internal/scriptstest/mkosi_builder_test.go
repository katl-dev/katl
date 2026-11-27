package scriptstest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	builderRepository = "ghcr.io/katl-dev/katl/mkosi-builder"
	builderDigest     = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
)

// builderFixture is a checkout holding only the builder's definition and the
// owner that resolves it.
func builderFixture(t *testing.T, release, containerfile string) string {
	t.Helper()
	repo, fixture := repoRoot(t), t.TempDir()
	for name, mode := range map[string]os.FileMode{"scripts/mkosi-builder": 0o755, "scripts/fedora-release": 0o755, "containers-policy.json": 0o644} {
		path := filepath.Join(fixture, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, mustReadFile(t, filepath.Join(repo, name)), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeBuilderInputs(t, fixture, release, containerfile)
	return fixture
}

func writeBuilderInputs(t *testing.T, fixture, release, containerfile string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture, "mkosi.conf"), []byte("[Distribution]\nRelease="+release+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "Containerfile.mkosi"), []byte(containerfile), 0o644); err != nil {
		t.Fatal(err)
	}
}

// contentTag derives the documented tag independently of the script.
func contentTag(release, containerfile string) string {
	sum := sha256.Sum256([]byte(containerfile))
	return "fedora-" + release + "-" + hex.EncodeToString(sum[:])[:16]
}

func writeBuilderLock(t *testing.T, fixture, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture, "mkosi-builder.lock"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runBuilder(t *testing.T, fixture string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(fixture, "scripts", "mkosi-builder"), args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "GITHUB_ACTIONS=", "GITHUB_STEP_SUMMARY=")
	cmd.Env = append(cmd.Env, env...)
	output, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(output), exit.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(output), 0
}

// The pin is current only for the exact Containerfile and Fedora release it was
// built from. Wrong implementations this separates: hashing only the
// Containerfile, accepting any well-formed lock, or treating a malformed lock
// as a reason to build locally.
func TestBuilderPin(t *testing.T) {
	const containerfile = "ARG FEDORA_RELEASE\nFROM registry.fedoraproject.org/fedora:${FEDORA_RELEASE}\n"
	pinned := builderRepository + "@" + builderDigest
	current := "tag=" + contentTag("99", containerfile) + "\nimage=" + pinned + "\n"
	for _, tc := range []struct {
		name, lock, release, containerfile string
		want                               string
		exit                               int
	}{
		{name: "no lock", exit: 1},
		{name: "current", lock: current, want: pinned + "\n"},
		{name: "current without final newline", lock: strings.TrimSuffix(current, "\n"), want: pinned + "\n"},
		{name: "Containerfile changed", lock: current, containerfile: containerfile + "RUN true\n", exit: 1},
		{name: "Fedora release changed", lock: current, release: "100", exit: 1},
		{name: "tag reference", lock: "tag=" + contentTag("99", containerfile) + "\nimage=" + builderRepository + ":latest\n", exit: 3},
		{name: "other repository", lock: "tag=" + contentTag("99", containerfile) + "\nimage=ghcr.io/example/builder@" + builderDigest + "\n", exit: 3},
		{name: "unknown key", lock: current + "digest=" + builderDigest + "\n", exit: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release, file := "99", containerfile
			if tc.release != "" {
				release = tc.release
			}
			if tc.containerfile != "" {
				file = tc.containerfile
			}
			fixture := builderFixture(t, release, file)
			if tc.lock != "" {
				writeBuilderLock(t, fixture, tc.lock)
			}
			output, exit := runBuilder(t, fixture, nil, "pinned")
			if exit != tc.exit || output != tc.want {
				t.Fatalf("pinned = %q, exit %d; want %q, exit %d", output, exit, tc.want, tc.exit)
			}
		})
	}
}

// The lock the publishing workflow writes is the one builds accept.
func TestBuilderLockIsCurrent(t *testing.T) {
	fixture := builderFixture(t, "99", "FROM scratch\n")
	lock, exit := runBuilder(t, fixture, nil, "lock", builderDigest)
	if exit != 0 {
		t.Fatalf("lock exit %d", exit)
	}
	writeBuilderLock(t, fixture, lock)
	if output, exit := runBuilder(t, fixture, nil, "pinned"); exit != 0 || output != builderRepository+"@"+builderDigest+"\n" {
		t.Fatalf("pinned = %q, exit %d", output, exit)
	}
	if output, _ := runBuilder(t, fixture, nil, "tag"); output != contentTag("99", "FROM scratch\n")+"\n" {
		t.Fatalf("tag = %q", output)
	}
}

// fakeRuntime is a container runtime whose image store is a directory, so a
// test can observe which image each name resolves to.
const fakeRuntime = `key() { printf '%s' "$1" | sha256sum | cut -c1-32; }
printf '%s\n' "$*" >> "$KATL_FAKE_CALLS"
name="${@: -1}"
case "$1" in
  image)
    [[ -f "$KATL_FAKE_STORE/$(key "$name")" ]] || exit 1
    [[ "$2" == exists ]] || cat "$KATL_FAKE_STORE/$(key "$name")"
    ;;
  pull)
    [[ -z "${KATL_FAKE_PULL_FAILS:-}" ]] || exit 1
    if [[ -n "${KATL_FAKE_PULL_FAILURES:-}" ]]; then
      failed=$(cat "$KATL_FAKE_STORE/failed-pulls" 2>/dev/null || echo 0)
      if ((failed < KATL_FAKE_PULL_FAILURES)); then echo $((failed + 1)) > "$KATL_FAKE_STORE/failed-pulls"; exit 1; fi
    fi
    echo "pulled $name" > "$KATL_FAKE_STORE/$(key "$name")"
    ;;
  build)
    ls -A "$name" > "$KATL_FAKE_CONTEXT"
    args=("$@")
    for i in "${!args[@]}"; do [[ "${args[i]}" != --tag ]] || tag="${args[i+1]}"; done
    echo "built $tag" > "$KATL_FAKE_STORE/$(key "$tag")"
    ;;
  tag) cp "$KATL_FAKE_STORE/$(key "$2")" "$KATL_FAKE_STORE/$(key "$3")" ;;
  run) printf 'mkosi 26\n' ;;
  buildx)
    ref="$4"
    [[ -z "${KATL_FAKE_REGISTRY_ERROR:-}" ]] || { echo "ERROR: unexpected status: 502 Bad Gateway" >&2; exit 1; }
    [[ -f "$KATL_FAKE_REMOTE/$(key "$ref")" ]] || { echo "ERROR: $ref: not found" >&2; exit 1; }
    printf '{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":1}' "$(cat "$KATL_FAKE_REMOTE/$(key "$ref")")"
    ;;
  push)
    [[ -f "$KATL_FAKE_STORE/$(key "$name")" ]] || exit 1
    printf 'sha256:%s' "$(sha256sum "$KATL_FAKE_STORE/$(key "$name")" | cut -c1-64)" > "$KATL_FAKE_REMOTE/$(key "$name")"
    ;;
  *) exit 2 ;;
esac
`

type fakeContainers struct {
	bin, store, calls, context, remote string
}

func newFakeContainers(t *testing.T, runtime string) fakeContainers {
	t.Helper()
	dir := t.TempDir()
	f := fakeContainers{bin: filepath.Join(dir, "bin"), store: filepath.Join(dir, "store"), calls: filepath.Join(dir, "calls"), context: filepath.Join(dir, "context"), remote: filepath.Join(dir, "remote")}
	for _, path := range []string{f.bin, f.store, f.remote} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFakeExecutable(t, f.bin, runtime, fakeRuntime)
	return f
}

func (f fakeContainers) env(extra ...string) []string {
	return append([]string{
		"PATH=" + f.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"KATL_FAKE_STORE=" + f.store, "KATL_FAKE_CALLS=" + f.calls, "KATL_FAKE_CONTEXT=" + f.context, "KATL_FAKE_REMOTE=" + f.remote,
	}, extra...)
}

// image reports what a name resolves to in the fake store.
func (f fakeContainers) image(t *testing.T, name string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	data, err := os.ReadFile(filepath.Join(f.store, hex.EncodeToString(sum[:])[:32]))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func (f fakeContainers) callsStartingWith(t *testing.T, verb string) []string {
	t.Helper()
	data, err := os.ReadFile(f.calls)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, verb+" ") {
			calls = append(calls, line)
		}
	}
	return calls
}

func recordValue(record, key string) string {
	for _, line := range strings.Split(record, "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			return value
		}
	}
	return ""
}

// prepare pulls the pinned digest when it can and otherwise builds the same
// file, and the alias and record name whatever it used. Wrong implementations
// this separates: pulling a stale pin, pulling by tag, building from the
// repository as context, or leaving the alias on an earlier image.
func TestBuilderPrepare(t *testing.T) {
	const containerfile = "FROM scratch\n"
	alias := "localhost/katl-mkosi-builder:alias"
	pinned := builderRepository + "@" + builderDigest
	local := "localhost/katl-mkosi-builder:" + contentTag("99", containerfile)
	current := "tag=" + contentTag("99", containerfile) + "\nimage=" + pinned + "\n"
	stale := "tag=" + contentTag("99", "FROM earlier\n") + "\nimage=" + pinned + "\n"
	for _, tc := range []struct {
		name, lock, pullFails   string
		reference, source, uses string
		pulls                   int
	}{
		{name: "pinned", lock: current, reference: pinned, source: "pinned", uses: "pulled " + pinned, pulls: 1},
		{name: "pull fails", lock: current, pullFails: "1", reference: local, source: "built", uses: "built " + local, pulls: 1},
		{name: "stale pin", lock: stale, reference: local, source: "built", uses: "built " + local},
		{name: "no pin", reference: local, source: "built", uses: "built " + local},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := builderFixture(t, "99", containerfile)
			if tc.lock != "" {
				writeBuilderLock(t, fixture, tc.lock)
			}
			containers := newFakeContainers(t, "podman")
			// The alias starts on an unrelated image, as after a previous checkout.
			if err := os.WriteFile(filepath.Join(containers.store, keyFor(alias)), []byte("old image\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			summary := filepath.Join(t.TempDir(), "summary.md")
			record, exit := runBuilder(t, fixture, containers.env("KATL_FAKE_PULL_FAILS="+tc.pullFails, "GITHUB_STEP_SUMMARY="+summary), "prepare", "podman", alias)
			if exit != 0 {
				t.Fatalf("prepare exit %d: %s", exit, record)
			}

			if got := recordValue(record, "reference"); got != tc.reference {
				t.Fatalf("reference = %q, want %q", got, tc.reference)
			}
			if got := recordValue(record, "source"); got != tc.source {
				t.Fatalf("source = %q, want %q", got, tc.source)
			}
			wantDigest := ""
			if tc.source == "pinned" {
				wantDigest = builderDigest
			}
			if got := recordValue(record, "digest"); got != wantDigest {
				t.Fatalf("digest = %q, want %q", got, wantDigest)
			}
			if got := containers.image(t, alias); got != tc.uses {
				t.Fatalf("alias resolves to %q, want %q", got, tc.uses)
			}
			if !strings.Contains(string(mustReadFile(t, summary)), "reference="+tc.reference) {
				t.Fatalf("job summary does not record the builder:\n%s", mustReadFile(t, summary))
			}

			pulls := containers.callsStartingWith(t, "pull")
			if len(pulls) != tc.pulls {
				t.Fatalf("pulls = %q, want %d", pulls, tc.pulls)
			}
			for _, pull := range pulls {
				if !strings.HasSuffix(pull, " "+pinned) {
					t.Fatalf("pull %q does not name the pinned digest", pull)
				}
			}
			builds := containers.callsStartingWith(t, "build")
			if tc.source == "built" {
				if len(builds) != 1 || !strings.Contains(builds[0], "--build-arg FEDORA_RELEASE=99") ||
					!strings.Contains(builds[0], "--file "+filepath.Join(fixture, "Containerfile.mkosi")) {
					t.Fatalf("builds = %q", builds)
				}
				if context := mustReadFile(t, containers.context); len(context) != 0 {
					t.Fatalf("build context is not empty: %s", context)
				}
			} else if len(builds) != 0 {
				t.Fatalf("pinned builder was also built: %q", builds)
			}

			// A second preparation reuses what the first one left: it builds
			// nothing again, and pulls again only while the pin is still missing.
			if _, exit := runBuilder(t, fixture, containers.env("KATL_FAKE_PULL_FAILS="+tc.pullFails), "prepare", "podman", alias); exit != 0 {
				t.Fatalf("repeated prepare exit %d", exit)
			}
			wantPulls := tc.pulls
			if tc.pullFails != "" {
				wantPulls++
			}
			if got := len(containers.callsStartingWith(t, "pull")); got != wantPulls {
				t.Fatalf("repeated prepare made %d pulls in total, want %d", got, wantPulls)
			}
			if got := len(containers.callsStartingWith(t, "build")); got != len(builds) {
				t.Fatalf("repeated prepare built again")
			}
		})
	}
}

func keyFor(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:32]
}

// scripts/mkosi runs the image the owner resolved, not the mutable alias, and
// keeps the alias that sibling scripts run on the same image.
func TestMkosiRunsResolvedBuilder(t *testing.T) {
	repo := repoRoot(t)
	const containerfile = "FROM scratch\n"
	fixture := builderFixture(t, "99", containerfile)
	writeBuilderLock(t, fixture, "tag="+contentTag("99", containerfile)+"\nimage="+builderRepository+"@"+builderDigest+"\n")
	containers := newFakeContainers(t, "podman")

	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "builder-version")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), containers.env("KATL_REPO_ROOT="+fixture, "KATL_CONTAINER_RUNTIME=podman", "KATL_MKOSI_IMAGE=", "GITHUB_STEP_SUMMARY=")...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("builder-version: %v\n%s", err, output)
	}

	runs := containers.callsStartingWith(t, "run")
	if len(runs) != 1 || !strings.Contains(runs[0], " "+builderRepository+"@"+builderDigest+" mkosi --version") {
		t.Fatalf("runs = %q", runs)
	}
	if got := containers.image(t, "localhost/katl-mkosi-builder:fedora-99-go-squashfs"); got != "pulled "+builderRepository+"@"+builderDigest {
		t.Fatalf("default alias resolves to %q", got)
	}
}

// In CI a current pin must be the builder every job runs, so a pull that keeps
// failing stops the job after bounded retries instead of building locally,
// while one that recovers uses the pin. Wrong implementations this separates:
// the local fallback leaking into CI, retrying without a bound, and giving up
// after the first transient failure.
func TestBuilderPrepareInCI(t *testing.T) {
	const containerfile = "FROM scratch\n"
	pinned := builderRepository + "@" + builderDigest
	for _, tc := range []struct {
		name, failures string
		exit           int
		pulls          int
		alias          string
	}{
		{name: "recovers", failures: "2", pulls: 3, alias: "pulled " + pinned},
		{name: "keeps failing", failures: "99", exit: 1, pulls: 3, alias: "old image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := builderFixture(t, "99", containerfile)
			writeBuilderLock(t, fixture, "tag="+contentTag("99", containerfile)+"\nimage="+pinned+"\n")
			containers := newFakeContainers(t, "docker")
			alias := "localhost/katl-mkosi-builder:alias"
			if err := os.WriteFile(filepath.Join(containers.store, keyFor(alias)), []byte("old image\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			record, exit := runBuilder(t, fixture, containers.env("GITHUB_ACTIONS=true", "KATL_FAKE_PULL_FAILURES="+tc.failures, "KATL_MKOSI_BUILDER_PULL_BACKOFF_SECONDS=0"), "prepare", "docker", alias)
			if (exit == 0) != (tc.exit == 0) {
				t.Fatalf("prepare exit %d, want %d: %s", exit, tc.exit, record)
			}
			if got := len(containers.callsStartingWith(t, "pull")); got != tc.pulls {
				t.Fatalf("pull attempts = %d, want %d", got, tc.pulls)
			}
			if builds := containers.callsStartingWith(t, "build"); len(builds) != 0 {
				t.Fatalf("CI built the builder locally: %q", builds)
			}
			if got := containers.image(t, alias); got != tc.alias {
				t.Fatalf("alias resolves to %q, want %q", got, tc.alias)
			}
			if tc.exit == 0 && recordValue(record, "source") != "pinned" {
				t.Fatalf("record = %s", record)
			}
		})
	}
}

// publish never overwrites a tag: a push reuses the content tag it finds, and
// a refresh takes the first free dated tag. Wrong implementations this
// separates: rebuilding a push over an existing content tag, moving the
// content tag on refresh, overwriting the day's dated tag, and treating a
// registry error as a missing tag.
func TestBuilderPublish(t *testing.T) {
	const containerfile = "FROM scratch\n"
	content := builderRepository + ":" + contentTag("99", containerfile)
	dated := content + "-20261127"
	for _, tc := range []struct {
		name, event  string
		existing     []string
		registryDown bool
		target       string
		published    bool
	}{
		{name: "first push", event: "push", target: content, published: true},
		{name: "push rerun", event: "push", existing: []string{content}, target: content},
		{name: "refresh without content tag", event: "refresh", target: content, published: true},
		{name: "refresh", event: "refresh", existing: []string{content}, target: dated, published: true},
		{name: "second refresh", event: "refresh", existing: []string{content, dated}, target: dated + "-2", published: true},
		{name: "third refresh", event: "refresh", existing: []string{content, dated, dated + "-2"}, target: dated + "-3", published: true},
		{name: "registry error", event: "push", registryDown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := builderFixture(t, "99", containerfile)
			containers := newFakeContainers(t, "docker")
			before := map[string]string{}
			for i, ref := range tc.existing {
				digest := fmt.Sprintf("sha256:%064d", i+1)
				before[ref] = digest
				if err := os.WriteFile(filepath.Join(containers.remote, keyFor(ref)), []byte(digest), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			env := containers.env("KATL_MKOSI_BUILDER_DATE=20261127")
			if tc.registryDown {
				env = append(env, "KATL_FAKE_REGISTRY_ERROR=1")
			}

			record, exit := runBuilder(t, fixture, env, "publish", tc.event)
			if tc.registryDown {
				if exit == 0 || len(containers.callsStartingWith(t, "build"))+len(containers.callsStartingWith(t, "push")) != 0 {
					t.Fatalf("registry error published anyway: exit %d", exit)
				}
				return
			}
			if exit != 0 {
				t.Fatalf("publish exit %d", exit)
			}
			if got := recordValue(record, "target"); got != tc.target {
				t.Fatalf("target = %q, want %q", got, tc.target)
			}
			if got := recordValue(record, "published"); got != fmt.Sprint(tc.published) {
				t.Fatalf("published = %q, want %t", got, tc.published)
			}
			remote, err := os.ReadFile(filepath.Join(containers.remote, keyFor(tc.target)))
			if err != nil {
				t.Fatal(err)
			}
			if got := recordValue(record, "digest"); got != string(remote) {
				t.Fatalf("digest = %q, registry has %q", got, remote)
			}
			for ref, digest := range before {
				if got := string(mustReadFile(t, filepath.Join(containers.remote, keyFor(ref)))); got != digest {
					t.Fatalf("%s was overwritten", ref)
				}
			}
			if pushes := containers.callsStartingWith(t, "push"); tc.published != (len(pushes) == 1) {
				t.Fatalf("pushes = %q", pushes)
			}
		})
	}
}
