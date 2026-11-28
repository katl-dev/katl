package scriptstest

import (
	"crypto/sha256"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

func TestKatlReleaseArtifactVersion(t *testing.T) {
	repo := repoRoot(t)
	script := filepath.Join(repo, "scripts", "katl-release-artifacts")
	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{name: "stable tag", args: []string{"version", "push", "tag", "v2026.7.0"}, want: "2026.7.0"},
		{name: "development tag", args: []string{"version", "push", "tag", "v2026.7.0-dev.0"}, want: "2026.7.0-dev.0"},
		{name: "alpha tag", args: []string{"version", "push", "tag", "v2026.7.0-alpha.1"}, want: "2026.7.0-alpha.1"},
		{name: "manual", args: []string{"version", "workflow_dispatch", "branch", "main", "2026.7.0-dev.1"}, want: "2026.7.0-dev.1"},
		{name: "scheduled", args: []string{"version", "schedule", "branch", "main", "2026.7.1-dev.0"}, want: "2026.7.1-dev.0"},
		{name: "called candidate", args: []string{"version", "workflow_call", "branch", "release/2026.7.0", "2026.7.0-rc.2"}, want: "2026.7.0-rc.2"},
		{name: "called final", args: []string{"version", "workflow_call", "branch", "release/2026.7.0", "2026.7.0"}, want: "2026.7.0"},
		{name: "called without version", args: []string{"version", "workflow_call", "branch", "release/2026.7.0", ""}, wantErr: "version is empty"},
		{name: "called leading-zero candidate", args: []string{"version", "workflow_call", "branch", "release/2026.7.0", "2026.7.0-rc.01"}, wantErr: "not canonical"},
		{name: "scheduled without version", args: []string{"version", "schedule", "branch", "main", ""}, wantErr: "version is empty"},
		{name: "series branch", args: []string{"version", "push", "branch", "release/2026.12.x"}, wantErr: "unsupported push ref type"},
		{name: "versioned branch", args: []string{"version", "push", "branch", "release/2026.7.0-rc.1"}, wantErr: "unsupported push ref type"},
		{name: "semantic version", args: []string{"version", "push", "tag", "v0.1.0"}, wantErr: "not canonical"},
		{name: "nightly label", args: []string{"version", "push", "tag", "nightly"}, wantErr: "not canonical"},
		{name: "zero-padded month", args: []string{"version", "push", "tag", "v2026.07.0"}, wantErr: "not canonical"},
		{name: "invalid month", args: []string{"version", "push", "tag", "v2026.13.0"}, wantErr: "not canonical"},
		{name: "leading-zero sequence", args: []string{"version", "push", "tag", "v2026.7.0-rc.01"}, wantErr: "not canonical"},
		{name: "unsupported prerelease", args: []string{"version", "workflow_dispatch", "branch", "main", "2026.7.0-test.1"}, wantErr: "not canonical"},
		{name: "unsafe manual", args: []string{"version", "workflow_dispatch", "branch", "main", ".hidden"}, wantErr: "not canonical"},
		{name: "empty manual", args: []string{"version", "workflow_dispatch", "branch", "main", ""}, wantErr: "version is empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(script, tt.args...)
			cmd.Dir = repo
			output, err := cmd.CombinedOutput()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(string(output), tt.wantErr) {
					t.Fatalf("error = %v, output = %q, want %q", err, output, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("version failed: %v\n%s", err, output)
			}
			if got := strings.TrimSpace(string(output)); got != tt.want {
				t.Fatalf("version = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestKatlReleaseArtifactDevVersion(t *testing.T) {
	repo := repoRoot(t)
	script := filepath.Join(repo, "scripts", "katl-release-artifacts")
	tests := []struct{ name, tag, want, wantErr string }{
		{name: "stable tag", tag: "v2026.11.4", want: "2026.11.5-dev.0"},
		{name: "first release of a month", tag: "v2026.12.0", want: "2026.12.1-dev.0"},
		{name: "two-digit patch", tag: "v2026.11.9", want: "2026.11.10-dev.0"},
		{name: "prerelease", tag: "v2026.11.4-rc.1", wantErr: "not a stable"},
		{name: "not CalVer", tag: "v1.2.3", wantErr: "not a stable"},
		{name: "development", tag: "v2026.11.5-dev.0", wantErr: "not a stable"},
		{name: "unprefixed", tag: "2026.11.4", wantErr: "not a stable"},
		{name: "empty", tag: "", wantErr: "not a stable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(script, "dev-version", tt.tag)
			cmd.Dir = repo
			output, err := cmd.CombinedOutput()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(string(output), tt.wantErr) {
					t.Fatalf("error = %v, output = %q, want %q", err, output, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("dev-version failed: %v\n%s", err, output)
			}
			if got := strings.TrimSpace(string(output)); got != tt.want {
				t.Fatalf("dev-version = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestKatlReleaseArtifactNotes(t *testing.T) {
	repo := repoRoot(t)

	const commit = "0123456789abcdef0123456789abcdef01234567"
	const repositoryURL = "https://github.example/katl-dev/katl"
	wantChanges := []string{
		"**release:** attest KatlOS artifacts",
		"**release:** automate Kubernetes bundles",
		"Rotate node credentials",
	}
	versions := []struct {
		name        string
		versionArgs []string
		wantVersion string
		wantBundle  string
	}{
		{
			name:        "dev",
			versionArgs: []string{"schedule", "branch", "main", "2026.7.0-dev.4"},
			wantVersion: "2026.7.0-dev.4",
			wantBundle:  "katl-2026.7.0-dev.4-x86_64-0123456789ab",
		},
		{
			name:        "rc",
			versionArgs: []string{"workflow_call", "branch", "release/2026.6.3", "2026.6.3-rc.1"},
			wantVersion: "2026.6.3-rc.1",
			wantBundle:  "katl-2026.6.3-rc.1-x86_64-0123456789ab",
		},
		{
			name:        "existing series final",
			versionArgs: []string{"workflow_call", "branch", "release/2026.6.3", "2026.6.3"},
			wantVersion: "2026.6.3",
			wantBundle:  "katl-2026.6.3-x86_64-0123456789ab",
		},
		{
			name:        "new series final",
			versionArgs: []string{"workflow_call", "branch", "release/2026.7.0", "2026.7.0"},
			wantVersion: "2026.7.0",
			wantBundle:  "katl-2026.7.0-x86_64-0123456789ab",
		},
	}
	guidance := []struct {
		name         string
		next         string // docs/releases/next.md in the fixture; empty means absent
		wantGuidance []string
	}{
		{name: "no guidance"},
		{
			name:         "next.md with bullets",
			next:         "## Pending guidance\n\n- Drain workloads before upgrading.\n- Upgrade control-plane nodes first.\n",
			wantGuidance: []string{"- Drain workloads before upgrading.", "- Upgrade control-plane nodes first."},
		},
	}

	for _, g := range guidance {
		t.Run(g.name, func(t *testing.T) {
			gitDir := t.TempDir()
			runGit(t, gitDir, "init", "--quiet")
			if g.next != "" {
				releases := filepath.Join(gitDir, "docs", "releases")
				if err := os.MkdirAll(releases, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(releases, "next.md"), []byte(g.next), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			commits := []string{
				"runtime: establish KatlOS root",
				"release: publish first artifacts",
				"release: automate Kubernetes bundles",
				"release: attest KatlOS artifacts",
				"test: exercise release note suppression",
				"management: rotate credentials internally",
			}
			var hashes []string
			for index, subject := range commits {
				path := filepath.Join(gitDir, fmt.Sprintf("change-%d", index))
				if err := os.WriteFile(path, []byte(subject+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGit(t, gitDir, "add", filepath.Base(path))
				args := []string{"commit", "--quiet", "-m", subject}
				if index == 4 {
					args = append(args, "-m", "Release-Note: skip")
				} else if index == 5 {
					args = append(args, "-m", "Release-Note: Rotate node credentials\n\n* Follow-up cleanup\n\nRelease-Note: skip")
				}
				runGit(t, gitDir, args...)
				hashes = append(hashes, strings.TrimSpace(runGit(t, gitDir, "rev-parse", "HEAD")))
				if index == 1 {
					runGit(t, gitDir, "tag", "v2026.6.2")
					runGit(t, gitDir, "tag", "v1.36.0-katl.99")
				}
			}
			// The existing-series final is tagged locally but has no published release.
			for _, tag := range []string{"v2026.7.0-dev.4", "v2026.6.3-rc.1", "v2026.6.3", "v2026.7.0"} {
				runGit(t, gitDir, "tag", tag)
			}
			published := fakeReleaseListing(t, `[{"tag_name":"v2026.6.2","draft":false,"prerelease":false}]`)

			for _, tc := range versions {
				t.Run(tc.name, func(t *testing.T) {
					run := func(script string, args ...string) string {
						t.Helper()
						cmd := exec.Command(filepath.Join(repo, "scripts", script), args...)
						cmd.Dir = gitDir
						// The fixture is the repository root, so guidance comes
						// from its docs/releases and never from this checkout.
						cmd.Env = append(
							gittest.Env(environmentWithout("KATL_RELEASE_TARGET"), gitDir),
							published,
							"GITHUB_REF_TYPE="+tc.versionArgs[1],
							"GITHUB_SHA=not-a-commit-in-the-test-repository",
							"KATL_RELEASE_REPOSITORY_URL="+repositoryURL,
							"KATL_REPO_ROOT="+gitDir,
						)
						output, err := cmd.CombinedOutput()
						if err != nil {
							t.Fatalf("%s %v: %v\n%s", script, args, err, output)
						}
						return string(output)
					}

					version := strings.TrimSpace(run("katl-release-artifacts", append([]string{"version"}, tc.versionArgs...)...))
					if version != tc.wantVersion {
						t.Fatalf("version = %q, want %q", version, tc.wantVersion)
					}
					if bundle := strings.TrimSpace(run("katl-release", "bundle-name", version, commit)); bundle != tc.wantBundle {
						t.Fatalf("bundle name = %q, want %q", bundle, tc.wantBundle)
					}

					notes := run("katl-release-artifacts", "notes", version)
					changes := releaseNoteSubjects(notes)
					slices.Sort(changes)
					if !slices.Equal(changes, wantChanges) {
						t.Fatalf("changes = %q, want %q\n%s", changes, wantChanges, notes)
					}

					// Guidance follows the Changes section and precedes Verify.
					body, _, _ := strings.Cut(notes, "\n## Verify\n")
					_, guidanceText, _ := strings.Cut(strings.TrimPrefix(body, "## Changes\n"), "\n## ")
					var gotGuidance []string
					for _, line := range strings.Split(guidanceText, "\n") {
						if strings.HasPrefix(line, "- ") {
							gotGuidance = append(gotGuidance, line)
						}
					}
					if !slices.Equal(gotGuidance, g.wantGuidance) {
						t.Fatalf("guidance bullets = %q, want %q\n%s", gotGuidance, g.wantGuidance, notes)
					}

					tag := "v" + tc.wantVersion
					for _, value := range []string{
						"## Changes", "## Verify", "## Support",
						repositoryURL + "/commit/" + hashes[3],
						"[PROVENANCE.md](" + repositoryURL + "/releases/download/" + tag + "/PROVENANCE.md)",
						"[SUPPORT.md](" + repositoryURL + "/releases/download/" + tag + "/SUPPORT.md)",
						repositoryURL + "/compare/v2026.6.2..." + tag,
					} {
						if !strings.Contains(notes, value) {
							t.Errorf("release notes missing %q:\n%s", value, notes)
						}
					}
				})
			}
		})
	}
}

func TestKatlReleaseArtifactNotesFromSquashHistory(t *testing.T) {
	repo := repoRoot(t)
	fixtures := filepath.Join(repo, "internal", "scriptstest", "testdata", "release-notes")
	gitDir := t.TempDir()
	runGit(t, gitDir, "init", "--quiet")
	golden, err := os.ReadFile(filepath.Join(fixtures, "changes.golden"))
	if err != nil {
		t.Fatal(err)
	}
	want := string(golden)

	for _, ref := range []string{"47561a44", "3bc58a63", "9110881c", "e47afb10"} {
		message := filepath.Join(fixtures, ref+".txt")
		runGit(t, gitDir, "commit", "--quiet", "--allow-empty", "--cleanup=verbatim", "-F", message)
		hash := strings.TrimSpace(runGit(t, gitDir, "rev-parse", "HEAD"))
		want = strings.NewReplacer("{{"+ref+".full}}", hash, "{{"+ref+".short}}", hash[:7]).Replace(want)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "notes", "2026.11.5-rc.1")
	cmd.Dir = gitDir
	cmd.Env = append(
		gittest.Env(environmentWithout("KATL_RELEASE_TARGET", "KATL_RELEASE_REPOSITORY_URL"), gitDir),
		"KATL_REPO_ROOT="+gitDir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("notes failed: %v\n%s", err, output)
	}
	changes, _, found := strings.Cut(string(output), "\n## Verify\n")
	if !found || changes != want {
		t.Fatalf("Changes section:\n%s\nwant:\n%s", changes, want)
	}
}

func TestKatlReleaseArtifactNotesIncludeVersionedUpgradeGuide(t *testing.T) {
	repo := repoRoot(t)
	gitDir := t.TempDir()
	runGit(t, gitDir, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(gitDir, "change"), []byte("release candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, gitDir, "add", "change")
	runGit(t, gitDir, "commit", "--quiet", "-m", "release: prepare candidate")

	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "notes", "2026.9.0-beta.18")
	cmd.Dir = gitDir
	cmd.Env = gittest.Env(os.Environ(), gitDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("notes failed: %v\n%s", err, output)
	}
	notes := string(output)
	for _, want := range []string{
		"## Upgrade paths", "beta.17", "beta.14", "## Verify",
		"This is an experimental beta release for home-lab evaluation, not a production-supported release.",
	} {
		if !strings.Contains(notes, want) {
			t.Fatalf("release notes missing %q:\n%s", want, notes)
		}
	}
}

func TestKatlReleaseArtifactNotesGuidanceSelection(t *testing.T) {
	repo := repoRoot(t)

	tests := []struct {
		name    string
		version string
		files   map[string]string
		want    string
		absent  []string
	}{
		{
			name:    "next for rc",
			version: "2026.12.0-rc.1",
			files:   map[string]string{"next.md": "## Pending guidance\n"},
			want:    "## Pending guidance",
		},
		{
			name:    "next for final",
			version: "2026.12.0",
			files:   map[string]string{"next.md": "## Pending guidance\n"},
			want:    "## Pending guidance",
		},
		{
			name:    "versioned over next",
			version: "2026.12.0",
			files: map[string]string{
				"next.md":      "## Pending guidance\n",
				"2026.12.0.md": "## Published guidance\n",
			},
			want:   "## Published guidance",
			absent: []string{"## Pending guidance"},
		},
		{
			name:    "exact rc over final and next",
			version: "2026.12.0-rc.1",
			files: map[string]string{
				"next.md":           "## Pending guidance\n",
				"2026.12.0.md":      "## Final guidance\n",
				"2026.12.0-rc.1.md": "## RC guidance\n",
			},
			want:   "## RC guidance",
			absent: []string{"## Pending guidance", "## Final guidance"},
		},
		{
			name:    "final over next for rc",
			version: "2026.12.0-rc.2",
			files: map[string]string{
				"next.md":      "## Pending guidance\n",
				"2026.12.0.md": "## Final guidance\n",
			},
			want:   "## Final guidance",
			absent: []string{"## Pending guidance"},
		},
		{
			name:    "no guidance",
			version: "2026.12.0-rc.1",
			files:   map[string]string{},
			want:    "## Changes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gitDir := t.TempDir()
			runGit(t, gitDir, "init", "--quiet")
			if err := os.WriteFile(filepath.Join(gitDir, "change"), []byte("change\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, gitDir, "add", "change")
			runGit(t, gitDir, "commit", "--quiet", "-m", "release: prepare candidate")

			guidanceRoot := t.TempDir()
			guidanceDir := filepath.Join(guidanceRoot, "docs", "releases")
			if err := os.MkdirAll(guidanceDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, content := range test.files {
				if err := os.WriteFile(filepath.Join(guidanceDir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "notes", test.version)
			cmd.Dir = gitDir
			cmd.Env = append(gittest.Env(os.Environ(), gitDir), "KATL_REPO_ROOT="+guidanceRoot)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("notes failed: %v\n%s", err, output)
			}
			notes := string(output)
			if !strings.Contains(notes, test.want) {
				t.Fatalf("release notes missing %q:\n%s", test.want, notes)
			}
			for _, value := range test.absent {
				if strings.Contains(notes, value) {
					t.Fatalf("release notes unexpectedly contain %q:\n%s", value, notes)
				}
			}
		})
	}
}

func TestKatlReleaseArtifactBuildKatlctl(t *testing.T) {
	repo := repoRoot(t)
	buildDir := t.TempDir()
	version := "2026.7.0-rc.1"
	commit := strings.Repeat("a", 40)
	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "build-katlctl", version)
	cmd.Dir = repo
	// Release builds are stamped from a clone, which
	// TestKatlReleaseArtifactVerifyKatlctl covers with a fixture repository.
	// This build is from the developer's checkout, where a linked worktree
	// would be stamped from whatever .git directory lies above it.
	cmd.Env = append(
		os.Environ(),
		"GOFLAGS=-buildvcs=false",
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"KATL_ARCHITECTURE=x86_64",
		"KATL_BUILD_COMMIT="+commit,
		"KATL_BUILD_DATE=2026-07-11T00:00:00Z",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build katlctl failed: %v\n%s", err, output)
	}

	linuxPath := filepath.Join(buildDir, "katlctl-"+version+"-linux-amd64")
	output, err := exec.Command(linuxPath, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("run released katlctl: %v\n%s", err, output)
	}
	for _, value := range []string{version, commit, "2026-07-11T00:00:00Z"} {
		if !strings.Contains(string(output), value) {
			t.Fatalf("katlctl version output missing %q: %q", value, output)
		}
	}
	macPath := filepath.Join(buildDir, "katlctl-"+version+"-darwin-arm64")
	macBinary, err := macho.Open(macPath)
	if err != nil {
		t.Fatalf("read macOS katlctl: %v", err)
	}
	defer macBinary.Close()
	if macBinary.Cpu != macho.CpuArm64 {
		t.Fatalf("macOS katlctl CPU = %v, want arm64", macBinary.Cpu)
	}

	for _, target := range []struct{ os, arch string }{{"linux", "amd64"}, {"darwin", "arm64"}} {
		name := fmt.Sprintf("katlctl-%s-%s-%s", version, target.os, target.arch)
		path := filepath.Join(buildDir, name)
		var metadata struct {
			ArtifactKind string `json:"artifactKind"`
			Version      string `json:"version"`
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			SHA256       string `json:"sha256"`
			SizeBytes    int64  `json:"sizeBytes"`
		}
		if err := json.Unmarshal(mustReadFile(t, path+".json"), &metadata); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(mustReadFile(t, path))
		if metadata.ArtifactKind != "katl.operator-cli.v1" || metadata.Version != version || metadata.OS != target.os || metadata.Architecture != target.arch || metadata.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("%s metadata = %#v", name, metadata)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if metadata.SizeBytes != info.Size() {
			t.Fatalf("%s size = %d, metadata = %d", name, info.Size(), metadata.SizeBytes)
		}
		checksum := string(mustReadFile(t, path+".sha256"))
		if !strings.Contains(checksum, metadata.SHA256+"  "+name) {
			t.Fatalf("%s checksum = %q", name, checksum)
		}
	}
}

func environmentWithout(names ...string) []string {
	excluded := make(map[string]struct{}, len(names))
	for _, name := range names {
		excluded[name] = struct{}{}
	}

	environment := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if _, found := excluded[name]; !found {
			environment = append(environment, value)
		}
	}
	return environment
}

func TestKatlReleaseArtifactStage(t *testing.T) {
	repo := repoRoot(t)
	buildDir := t.TempDir()
	output := filepath.Join(t.TempDir(), "dist")
	version := "2026.7.0-rc.0"
	names := writeRequiredReleaseArtifacts(t, buildDir)
	for _, name := range []string{"katl-installer.packages.tsv", "katl-runtime.packages.tsv"} {
		if err := os.WriteFile(filepath.Join(buildDir, name), []byte("kernel-core\t0:6.19.1-1.fc44.x86_64\nsystemd\t0:259.9-1.fc44.x86_64\ncontainerd\t0:2.2.0-1.fc44.x86_64\ncrun\t0:1.26-1.fc44.x86_64\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeReleaseExtensionInventory(t, buildDir, "standard")
	if err := os.WriteFile(filepath.Join(buildDir, "katl-release-build-inputs.json"), []byte("{\"kind\":\"ResourceTestManifest\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeReleaseArtifact(t, buildDir, "katl-runtime.efi")
	if err := os.WriteFile(filepath.Join(buildDir, "artifacts.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	history := stageHistory(t)
	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "stage", version, output)
	cmd.Dir = history
	cmd.Env = append(
		gittest.Env(environmentWithout("KATL_RELEASE_TARGET"), history),
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"KATL_ARCHITECTURE=x86_64",
	)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stage failed: %v\n%s", err, result)
	}

	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	sort.Strings(got)
	evidence := []string{"katl-installer.packages.tsv", "katl-runtime.packages.tsv", "katl-runtime.extensions.json", "katl-release-build-inputs.json"}
	want := append([]string{"PROVENANCE.md", "RELEASE_NOTES.md", "SHA256SUMS", "SUPPORT.md"}, evidence...)
	for _, name := range names {
		want = append(want, name, name+".json", name+".sha256")
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("staged files = %#v, want %#v", got, want)
	}
	provenance, err := os.ReadFile(filepath.Join(output, "PROVENANCE.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"gh attestation verify",
		"katl-dev/katl/.github/workflows/release-artifacts.yml",
		"Secure Boot signature",
		"future node-side trust policy",
	} {
		if !strings.Contains(string(provenance), value) {
			t.Fatalf("provenance notice missing %q: %q", value, provenance)
		}
	}
	releaseNotes := string(mustReadFile(t, filepath.Join(output, "RELEASE_NOTES.md")))
	for _, value := range []string{"## Packages", "| Kernel | `6.19.1-1.fc44.x86_64` |", "| systemd | `259.9-1.fc44.x86_64` |", "| drbd9 | `9.3.4` |", "## Support", "SUPPORT.md", "## Changes", "**runtime:** prepare 2026.7.0", "## Verify", "[PROVENANCE.md](", "v2026.6.2...v2026.7.0-rc.0"} {
		if !strings.Contains(releaseNotes, value) {
			t.Fatalf("release notes missing %q: %q", value, releaseNotes)
		}
	}

	checksums := mustReadFile(t, filepath.Join(output, "SHA256SUMS"))
	for _, name := range append(append([]string{"PROVENANCE.md", "RELEASE_NOTES.md", "SUPPORT.md"}, evidence...), names...) {
		if !strings.Contains(string(checksums), "  "+name+"\n") {
			t.Fatalf("SHA256SUMS missing %q: %q", name, checksums)
		}
		for _, suffix := range []string{".json", ".sha256"} {
			if name == "PROVENANCE.md" || name == "RELEASE_NOTES.md" || name == "SUPPORT.md" || slices.Contains(evidence, name) {
				continue
			}
			if !strings.Contains(string(checksums), "  "+name+suffix+"\n") {
				t.Fatalf("SHA256SUMS missing %q: %q", name+suffix, checksums)
			}
		}
	}
	if strings.Contains(string(checksums), "  SHA256SUMS\n") {
		t.Fatalf("SHA256SUMS must not contain its own digest: %q", checksums)
	}
}

func TestKatlReleaseArtifactStageRejectsDigestMismatch(t *testing.T) {
	repo := repoRoot(t)
	buildDir := t.TempDir()
	writeRequiredReleaseArtifacts(t, buildDir)
	if err := os.WriteFile(filepath.Join(buildDir, "katl-installer.efi"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "stage", "2026.7.0-rc.0", filepath.Join(t.TempDir(), "dist"))
	cmd.Dir = repo
	cmd.Env = append(
		os.Environ(),
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"KATL_ARCHITECTURE=x86_64",
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "FAILED") {
		t.Fatalf("error = %v, output = %q, want checksum failure", err, output)
	}
}

func TestKatlReleaseArtifactStageRejectsMetadataMismatch(t *testing.T) {
	repo := repoRoot(t)
	buildDir := t.TempDir()
	writeRequiredReleaseArtifacts(t, buildDir)
	metadata, err := json.Marshal(struct {
		SHA256    string `json:"sha256"`
		SizeBytes int    `json:"sizeBytes"`
	}{
		SHA256:    strings.Repeat("0", sha256.Size*2),
		SizeBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "katl-installer.efi.json"), append(metadata, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "stage", "2026.7.0-rc.0", filepath.Join(t.TempDir(), "dist"))
	cmd.Dir = repo
	cmd.Env = append(
		os.Environ(),
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"KATL_ARCHITECTURE=x86_64",
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "metadata does not match artifact") {
		t.Fatalf("error = %v, output = %q, want metadata failure", err, output)
	}
}

func TestKatlReleaseArtifactStageRejectsVersionMismatch(t *testing.T) {
	repo := repoRoot(t)
	buildDir := t.TempDir()
	writeRequiredReleaseArtifacts(t, buildDir)
	path := filepath.Join(buildDir, "katl-installer.efi.json")
	var metadata map[string]any
	if err := json.Unmarshal(mustReadFile(t, path), &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["version"] = "2026.7.0-dev.0"
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "stage", "2026.7.0-rc.0", filepath.Join(t.TempDir(), "dist"))
	cmd.Dir = repo
	cmd.Env = append(
		os.Environ(),
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"KATL_ARCHITECTURE=x86_64",
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "release identity") {
		t.Fatalf("error = %v, output = %q, want release identity failure", err, output)
	}
}

func writeReleaseArtifact(t *testing.T, dir, name string) {
	t.Helper()
	data := []byte("artifact " + name + "\n")
	digest := sha256.Sum256(data)
	digestHex := hex.EncodeToString(digest[:])
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(struct {
		Version      string `json:"version"`
		Architecture string `json:"architecture"`
		SHA256       string `json:"sha256"`
		SizeBytes    int    `json:"sizeBytes"`
	}{
		Version:      "2026.7.0-rc.0",
		Architecture: "x86_64",
		SHA256:       digestHex,
		SizeBytes:    len(data),
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata = append(metadata, '\n')
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	checksum := fmt.Sprintf("%s  %s\n", digestHex, name)
	if err := os.WriteFile(filepath.Join(dir, name+".sha256"), []byte(checksum), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRequiredReleaseArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	names := []string{
		"katl-installer.efi",
		"katl-installer.vmlinuz",
		"katl-installer.initrd",
		"katl-installer.iso",
		"katlos-install-2026.7.0-rc.0-x86_64.squashfs",
		"katlos-upgrade-2026.7.0-rc.0-x86_64.squashfs",
		"katlctl-2026.7.0-rc.0-linux-amd64",
		"katlctl-2026.7.0-rc.0-darwin-arm64",
	}
	for _, name := range names {
		writeReleaseArtifact(t, dir, name)
	}
	setReleaseArtifactTarget(t, dir, "katlctl-2026.7.0-rc.0-linux-amd64", "linux", "amd64")
	setReleaseArtifactTarget(t, dir, "katlctl-2026.7.0-rc.0-darwin-arm64", "darwin", "arm64")
	return names
}

func writeReleaseExtensionInventory(t *testing.T, dir, flavour string) {
	t.Helper()
	kernelRelease := "6.19.1-1.fc44.x86_64"
	if flavour == "lts" {
		kernelRelease = "6.18.1-1.fc44.x86_64"
	}
	data := fmt.Sprintf(`{"schemaVersion":1,"artifactKind":"katl.release-extension-inventory.v1","version":"2026.7.0-rc.0","architecture":"x86_64","flavour":%q,"runtimeInterface":"katl-runtime-1","kernelRelease":%q,"runtimeSHA256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","extensions":[{"name":"drbd9","payloadVersion":"9.3.4","repository":"ghcr.io/katl-dev/katl/extensions/drbd9","reference":"ghcr.io/katl-dev/katl/extensions/drbd9@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`, flavour, kernelRelease)
	if err := os.WriteFile(filepath.Join(dir, "katl-runtime.extensions.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setReleaseArtifactTarget(t *testing.T, dir, name, osName, architecture string) {
	t.Helper()
	path := filepath.Join(dir, name+".json")
	var metadata map[string]any
	if err := json.Unmarshal(mustReadFile(t, path), &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["architecture"] = architecture
	metadata["os"] = osName
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKatlReleaseLTSStage(t *testing.T) {
	repo, buildDir := repoRoot(t), t.TempDir()
	names := writeRequiredReleaseArtifacts(t, buildDir)
	for _, name := range names {
		if strings.HasPrefix(name, "katlctl-") {
			continue
		}
		path := filepath.Join(buildDir, name+".json")
		var metadata map[string]any
		if err := json.Unmarshal(mustReadFile(t, path), &metadata); err != nil {
			t.Fatal(err)
		}
		metadata["flavour"], metadata["path"], metadata["checksumPath"] = "lts", name, name+".sha256"
		data, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(buildDir, "katl-release-build-inputs.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"katl-installer.packages.tsv", "katl-runtime.packages.tsv"} {
		if err := os.WriteFile(filepath.Join(buildDir, name), []byte("kernel-longterm-core\t0:6.18.1-1.fc44.x86_64\nsystemd\t0:259.9-1.fc44.x86_64\ncontainerd\t0:2.2.0-1.fc44.x86_64\ncrun\t0:1.26-1.fc44.x86_64\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeReleaseExtensionInventory(t, buildDir, "lts")
	output := filepath.Join(t.TempDir(), "dist")
	cmd := exec.Command(filepath.Join(repo, "scripts/katl-release-artifacts"), "stage", "2026.7.0-rc.0", output)
	history := stageHistory(t)
	cmd.Dir = history
	cmd.Env = append(gittest.Env(environmentWithout("KATL_RELEASE_TARGET"), history), "KATL_FLAVOUR=lts", "KATL_MKOSI_BUILD_DIR="+buildDir, "KATL_ARCHITECTURE=x86_64")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stage: %v\n%s", err, out)
	}
	for _, name := range []string{"katl-installer-lts.iso", "katl-installer-lts.vmlinuz", "katl-runtime-lts.extensions.json", "katlos-lts-install-2026.7.0-rc.0-x86_64.squashfs", "katlos-lts-upgrade-2026.7.0-rc.0-x86_64.squashfs", "katlctl-2026.7.0-rc.0-linux-amd64", "katlctl-2026.7.0-rc.0-darwin-arm64"} {
		if _, err := os.Stat(filepath.Join(output, name)); err != nil {
			t.Fatal(err)
		}
		if name == "katl-runtime-lts.extensions.json" {
			continue
		}
		check := exec.Command("sha256sum", "-c", name+".sha256")
		check.Dir = output
		if out, err := check.CombinedOutput(); err != nil {
			t.Fatalf("%s checksum: %v\n%s", name, err, out)
		}
	}
	check := exec.Command("sha256sum", "-c", "SHA256SUMS")
	check.Dir = output
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("manifest: %v\n%s", err, out)
	}
}

func TestKatlReleaseArtifactNotesReleaseNoteTrailers(t *testing.T) {
	repo := repoRoot(t)
	gitDir := t.TempDir()
	runGit(t, gitDir, "init", "--quiet")

	// GitHub squash bodies list the PR's commits oldest first and end with
	// co-author lines after a separator.
	const coAuthors = "\n---------\n\nCo-authored-by: Someone <someone@example.com>\n"
	cases := []struct {
		name    string
		subject string
		body    string
		want    string // exact entry text before the commit link; empty means omitted
	}{
		{
			name:    "ordinary trailer",
			subject: "area: ordinary change",
			body:    "Why.\n\nRelease-Note: Ordinary note\n",
			want:    "- Ordinary note",
		},
		{
			name:    "squash mid-body note",
			subject: "area: mid-body change",
			body:    "* area: first\n\nDetails.\n\nRelease-Note: Reader summary\n\n* area: second\n\nMore details.\n" + coAuthors,
			want:    "- Reader summary",
		},
		{
			name:    "all skip",
			subject: "area: skipped change",
			body:    "* area: first\n\nRelease-Note: skip\n\nRelease-Note: skip\nCo-authored-by: Someone <someone@example.com>\n",
		},
		{
			name:    "squash skip then note",
			subject: "area: skip then note",
			body: "* vmtest: first\n\nRelease-Note: skip\n\n* install: second\n\nDetails.\n\n" +
				"Release-Note: install: wrapped summary\nacross lines\n" + coAuthors,
			want: "- install: wrapped summary across lines",
		},
		{
			name:    "body note survives later skips",
			subject: "area: note then skips",
			body: "* area: first\n\nRelease-Note: Keep this summary\n\n* area: second\n\nRelease-Note: skip\n\n" +
				"Release-Note: skip\nCo-authored-by: Someone <someone@example.com>\n",
			want: "- Keep this summary",
		},
		{
			name:    "last reader note in trailer block",
			subject: "area: multiple trailers",
			body: "Details.\n\nRelease-Note: Superseded summary\nRelease-Note: Use this\n" +
				" folded summary\nRelease-Note: skip\n",
			want: "- Use this folded summary",
		},
		{
			name:    "squash without trailer",
			subject: "area: plain subject",
			body:    "* area: first\n\nDetails.\n" + coAuthors,
			want:    "- **area:** plain subject",
		},
	}

	runGit(t, gitDir, "commit", "--quiet", "--allow-empty", "-m", "release: base")
	runGit(t, gitDir, "tag", "v2026.6.2")
	hashes := make([]string, len(cases))
	for index, tc := range cases {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("message-%d", index))
		if err := os.WriteFile(path, []byte(tc.subject+"\n\n"+tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, gitDir, "commit", "--quiet", "--allow-empty", "-F", path)
		hashes[index] = strings.TrimSpace(runGit(t, gitDir, "rev-parse", "HEAD"))
	}
	runGit(t, gitDir, "tag", "v2026.7.0-dev.4")

	published := fakeReleaseListing(t, `[{"tag_name":"v2026.6.2","draft":false,"prerelease":false}]`)
	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "notes", "2026.7.0-dev.4")
	cmd.Dir = gitDir
	cmd.Env = append(
		gittest.Env(environmentWithout("KATL_RELEASE_TARGET"), gitDir),
		published,
		"GITHUB_REF_TYPE=branch",
		"KATL_RELEASE_REPOSITORY_URL=https://github.example/katl-dev/katl",
		"KATL_REPO_ROOT="+gitDir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("notes failed: %v\n%s", err, output)
	}
	notes := string(output)

	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hash := hashes[index]
			if tc.want == "" {
				if strings.Contains(notes, "/commit/"+hash) {
					t.Fatalf("entry for %q should be omitted:\n%s", tc.subject, notes)
				}
				return
			}
			if !strings.Contains(notes, "\n"+tc.want+" ([`") {
				t.Fatalf("notes missing entry %q:\n%s", tc.want, notes)
			}
		})
	}
	if strings.Contains(notes, "Superseded summary") {
		t.Fatalf("notes contain a note superseded by a later reader note:\n%s", notes)
	}
}

func TestKatlReleaseArtifactNotesSiblingRelease(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "Old release")
	runGit(t, dir, "tag", "v2026.11.4")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "Already shipped")
	runGit(t, dir, "switch", "--quiet", "-c", "release/2026.11.5")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "Package the previous release")
	runGit(t, dir, "tag", "-a", "v2026.11.5", "-m", "Previous release")
	runGit(t, dir, "switch", "--quiet", "main")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "New feature")
	runGit(t, dir, "tag", "v2026.11.6")
	runGit(t, dir, "tag", "v2026.11.7")
	runGit(t, dir, "tag", "v2026.11.8")

	t.Run("without a pin", func(t *testing.T) {
		published := fakeReleaseListing(t, `[
  {"tag_name":"v2026.11.4","draft":false,"prerelease":false},
  {"tag_name":"v2026.11.7","draft":true,"prerelease":false},
  {"tag_name":"v2026.11.5","draft":false,"prerelease":false},
  {"tag_name":"v2026.11.8","draft":false,"prerelease":true},
  {"tag_name":"v2026.11.10","draft":false,"prerelease":false}
]`)
		// v2026.11.6 has no release, and v2026.11.10 has no local tag.
		assertReleaseNoteSubjects(t, dir, "HEAD", "2026.11.11", []string{"New feature"}, published)
	})

	t.Run("with a pin", func(t *testing.T) {
		tools := t.TempDir()
		writeFakeExecutable(t, tools, "gh", "echo pinned releases do not need the release API >&2\nexit 1\n")
		writeReleaseScriptFixture(t, dir, "v2026.11.5")
		runGit(t, dir, "add", productionBaselineFile)
		runGit(t, dir, "commit", "--quiet", "-m", "New fix")
		assertReleaseNoteSubjects(t, dir, "HEAD", "2026.11.6", []string{"New fix", "New feature"},
			"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	})
}

func TestKatlReleaseArtifactNotesAfterPromotion(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "Previous release")
	runGit(t, dir, "tag", "v2026.11.5")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "Candidate feature")
	runGit(t, dir, "tag", "v2026.11.6-rc.1")
	runGit(t, dir, "tag", "v2026.11.6")

	// A checkout that has moved on may now pin the final release.
	writeReleaseScriptFixture(t, dir, "v2026.11.6")
	runGit(t, dir, "add", productionBaselineFile)
	runGit(t, dir, "commit", "--quiet", "-m", "Pin the published final")
	runGit(t, dir, "tag", "v2026.11.7")

	published := fakeReleaseListing(t, `[
  {"tag_name":"v2026.11.7","draft":false,"prerelease":false},
  {"tag_name":"v2026.11.6","draft":false,"prerelease":false},
  {"tag_name":"v2026.11.5","draft":false,"prerelease":false}
]`)
	assertReleaseNoteSubjects(t, dir, "v2026.11.6-rc.1", "2026.11.6-rc.1", []string{"Candidate feature"}, published)
}

func fakeReleaseListing(t *testing.T, releases string) string {
	t.Helper()
	tools := t.TempDir()
	writeFakeExecutable(t, tools, "gh", "test \"$1\" = api\ncat <<'EOF'\n"+releases+"\nEOF\n")
	return "PATH=" + tools + string(os.PathListSeparator) + os.Getenv("PATH")
}

func assertReleaseNoteSubjects(t *testing.T, dir, target, version string, want []string, path string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(repoRoot(t), "scripts", "katl-release-artifacts"), "notes", version)
	cmd.Dir = dir
	cmd.Env = append(gittest.Env(os.Environ(), dir), "KATL_RELEASE_TARGET="+target, "KATL_REPO_ROOT="+dir, path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("notes: %v\n%s", err, output)
	}

	if subjects := releaseNoteSubjects(string(output)); !slices.Equal(subjects, want) {
		t.Fatalf("subjects = %q, want %q\n%s", subjects, want, output)
	}
}

// releaseNoteSubjects returns the change entries: the bullets of the Changes
// section, which ends at the next heading.
func releaseNoteSubjects(notes string) []string {
	_, changes, _ := strings.Cut(notes, "## Changes\n")
	changes, _, _ = strings.Cut(changes, "\n## ")
	var subjects []string
	for _, line := range strings.Split(changes, "\n") {
		if subject, ok := strings.CutPrefix(line, "- "); ok {
			subject, _, _ = strings.Cut(subject, " ([`")
			subjects = append(subjects, subject)
		}
	}
	return subjects
}

func TestKatlReleaseArtifactVerifyKatlctl(t *testing.T) {
	repo := repoRoot(t)
	source := t.TempDir()
	buildDir := t.TempDir()
	version := "2026.7.0-rc.1"
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...)
		cmd.Dir = source
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	write("go.mod", "module example.invalid/katl\n\ngo 1.21\n")
	write("cmd/katlctl/main.go", "package main\n\nfunc main() {}\n")
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "fixture")

	run := func(command string) ([]byte, error) {
		cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), command, version)
		cmd.Env = append(
			os.Environ(),
			"KATL_REPO_ROOT="+source,
			"KATL_MKOSI_BUILD_DIR="+buildDir,
			"KATL_ARCHITECTURE=x86_64",
		)
		return cmd.CombinedOutput()
	}

	for _, tc := range []struct {
		name  string
		dirty bool
		pass  bool
	}{
		{"clean tree", false, true},
		{"untracked file", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dirty {
				write("mkosi.conf.d/ci-downloads.conf", "[Build]\n")
			}
			if output, err := run("build-katlctl"); err != nil {
				t.Fatalf("build katlctl: %v\n%s", err, output)
			}
			output, err := run("verify-katlctl")
			if (err == nil) != tc.pass {
				t.Fatalf("verify katlctl = %v, want pass=%v\n%s", err, tc.pass, output)
			}
		})
	}
}

func TestMkosiBuilderActionOutputIsIgnored(t *testing.T) {
	repo := repoRoot(t)
	action := string(mustReadFile(t, filepath.Join(repo, ".github", "actions", "mkosi-builder", "action.yml")))
	if !strings.Contains(action, "mkosi.conf.d/ci-downloads.conf") {
		t.Fatal("mkosi-builder no longer writes mkosi.conf.d/ci-downloads.conf; update this test")
	}
	// An untracked file in the checkout stamps every later Go build vcs.modified=true.
	if output, err := exec.Command("git", "-C", repo, "check-ignore", "-q", "mkosi.conf.d/ci-downloads.conf").CombinedOutput(); err != nil {
		t.Fatalf("mkosi.conf.d/ci-downloads.conf is not ignored: %v\n%s", err, output)
	}
}

const productionBaselineFile = "scripts/production-upgrade-baseline.txt"

// stageHistory returns a repository whose history stands in for the checkout's
// when stage renders release notes, so the result does not depend on which tags
// the checkout fetched.
func stageHistory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, productionBaselineFile), []byte("v2026.6.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", productionBaselineFile)
	runGit(t, dir, "commit", "--quiet", "-m", "release: publish 2026.6.2")
	runGit(t, dir, "tag", "v2026.6.2")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "runtime: prepare 2026.7.0")
	return dir
}

func writeReleaseScriptFixture(t *testing.T, dir, baseline string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "scripts/katl-release-artifacts"
	if err := os.Symlink(filepath.Join(repoRoot(t), script), filepath.Join(dir, script)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, productionBaselineFile), []byte(baseline+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKatlReleaseArtifactBaselineRejectsPrerelease(t *testing.T) {
	dir := t.TempDir()
	writeReleaseScriptFixture(t, dir, "v2026.11.5-rc.1")
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "release candidate")
	runGit(t, dir, "tag", "v2026.11.5-rc.1")

	command := exec.Command(filepath.Join(dir, "scripts/katl-release-artifacts"), "upgrade-baseline")
	command.Dir = dir
	command.Env = append(gittest.Env(os.Environ(), dir), "KATL_REPO_ROOT="+dir)
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), productionBaselineFile+" must contain a stable KatlOS release tag") {
		t.Fatalf("prerelease pin should fail: %v: %s", err, output)
	}
}

func TestKatlReleaseArtifactBaselineAtRevision(t *testing.T) {
	dir := t.TempDir()
	writeReleaseScriptFixture(t, dir, "v2026.11.4")
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "add", productionBaselineFile)
	runGit(t, dir, "commit", "--quiet", "-m", "produce bundle")
	runGit(t, dir, "tag", "v2026.11.4")
	producingRevision := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(dir, productionBaselineFile), []byte("v2026.11.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "--quiet", "-am", "move baseline")
	runGit(t, dir, "tag", "v2026.11.5")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "checkout", want: "v2026.11.5\n"},
		{name: "producing revision", args: []string{producingRevision}, want: "v2026.11.4\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(dir, "scripts/katl-release-artifacts"), append([]string{"upgrade-baseline"}, tc.args...)...)
			cmd.Dir = dir
			cmd.Env = append(gittest.Env(os.Environ(), dir), "KATL_REPO_ROOT="+dir)
			if output, err := cmd.CombinedOutput(); err != nil || string(output) != tc.want {
				t.Fatalf("upgrade-baseline %v = %q, error %v, want %q", tc.args, output, err, tc.want)
			}
		})
	}
}

func TestKatlReleaseArtifactPreviousTagInSeries(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "katl-release-artifacts")
	gitDir := t.TempDir()
	runGit(t, gitDir, "init", "--quiet", "--initial-branch=main")
	commit := func(tag string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(gitDir, "file"), []byte(tag+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, gitDir, "add", "file")
		runGit(t, gitDir, "commit", "--quiet", "-m", tag)
		runGit(t, gitDir, "tag", tag)
	}

	// release/2026.10.x branches from 2026.10.2 before main reaches 2026.11.x, and
	// carries prerelease tags that sort above its stable tag.
	commit("v2026.10.2")
	runGit(t, gitDir, "branch", "release/2026.10.x")
	commit("v2026.11.4")
	commit("v2026.11.9")
	commit("v2026.11.10")
	commit("v2026.12.0-rc.1")
	runGit(t, gitDir, "switch", "--quiet", "release/2026.10.x")
	commit("v2026.10.3-rc.1")
	commit("v2026.10.3-dev.0")
	runGit(t, gitDir, "switch", "--quiet", "main")

	tools := t.TempDir()
	// A base lookup must not consult the latest release, which belongs to main.
	writeFakeExecutable(t, tools, "gh", "echo gh must not be called >&2\nexit 1\n")

	for _, tt := range []struct{ name, base, want, wantErr string }{
		{name: "main ignores later prerelease and sorts numerically", base: "main", want: "v2026.11.10"},
		{name: "release series gets its own stable tag", base: "release/2026.10.x", want: "v2026.10.2"},
		{name: "commit of a base", base: "v2026.11.4^{commit}", want: "v2026.11.4"},
		{name: "unknown base", base: "release/none", wantErr: "base ref does not exist"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(script, "previous-tag", tt.base)
			cmd.Dir = gitDir
			cmd.Env = append(gittest.Env(os.Environ(), gitDir), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := cmd.CombinedOutput()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(string(output), tt.wantErr) {
					t.Fatalf("error = %v, output = %q, want %q", err, output, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("previous-tag failed: %v\n%s", err, output)
			}
			if got := strings.TrimSpace(string(output)); got != tt.want {
				t.Fatalf("previous-tag = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("only prereleases", func(t *testing.T) {
		empty := t.TempDir()
		runGit(t, empty, "init", "--quiet")
		runGit(t, empty, "commit", "--quiet", "--allow-empty", "-m", "x")
		runGit(t, empty, "tag", "v2026.11.5-rc.1")
		cmd := exec.Command(script, "previous-tag", "HEAD")
		cmd.Dir = empty
		cmd.Env = gittest.Env(os.Environ(), empty)
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "no stable release tag") {
			t.Fatalf("error = %v, output = %q", err, output)
		}
	})
}
