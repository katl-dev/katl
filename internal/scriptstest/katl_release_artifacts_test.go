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
		{name: "beta branch", args: []string{"version", "push", "branch", "release/2026.7.0-beta.2"}, want: "2026.7.0-beta.2"},
		{name: "release candidate branch", args: []string{"version", "push", "branch", "release/2026.7.0-rc.1"}, want: "2026.7.0-rc.1"},
		{name: "versioned branch", args: []string{"version", "push", "branch", "release/v2026.7.0"}, want: "2026.7.0"},
		{name: "manual", args: []string{"version", "workflow_dispatch", "branch", "main", "2026.7.0-dev.1"}, want: "2026.7.0-dev.1"},
		{name: "wrong branch", args: []string{"version", "push", "branch", "main"}, wantErr: "must start with release/"},
		{name: "nested branch", args: []string{"version", "push", "branch", "release/team/2026.7.0"}, wantErr: "not canonical"},
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

func TestKatlReleaseArtifactNotesFollowReleaseHistory(t *testing.T) {
	repo := repoRoot(t)
	gitDir := t.TempDir()
	runGit(t, gitDir, "init", "--quiet")
	runGit(t, gitDir, "config", "user.name", "Katl Test")
	runGit(t, gitDir, "config", "user.email", "test@katl.dev")

	for index, release := range []struct {
		tag     string
		subject string
	}{
		{tag: "v2026.7.0-dev.4", subject: "release: finish development train"},
		{tag: "v2026.7.0-alpha.1", subject: "release: publish first alpha"},
		{tag: "v2026.7.0-beta.1", subject: "release: publish first beta"},
	} {
		path := filepath.Join(gitDir, fmt.Sprintf("release-%d", index))
		if err := os.WriteFile(path, []byte(release.subject+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, gitDir, "add", filepath.Base(path))
		runGit(t, gitDir, "commit", "--quiet", "-m", release.subject)
		runGit(t, gitDir, "tag", release.tag)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "notes", "2026.7.0-beta.1")
	cmd.Dir = gitDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("notes failed: %v\n%s", err, output)
	}
	notes := string(output)
	for _, value := range []string{
		"experimental beta release for home-lab evaluation",
		"publish first beta",
		"v2026.7.0-alpha.1...v2026.7.0-beta.1",
	} {
		if !strings.Contains(notes, value) {
			t.Fatalf("release notes missing %q:\n%s", value, notes)
		}
	}
	for _, value := range []string{"publish first alpha", "finish development train"} {
		if strings.Contains(notes, value) {
			t.Fatalf("release notes unexpectedly contain %q:\n%s", value, notes)
		}
	}
}

func TestKatlReleaseArtifactNotes(t *testing.T) {
	repo := repoRoot(t)
	gitDir := t.TempDir()
	runGit(t, gitDir, "init", "--quiet")
	runGit(t, gitDir, "config", "user.name", "Katl Test")
	runGit(t, gitDir, "config", "user.email", "test@katl.dev")

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
			args = append(args, "-m", "Release-Note: Rotate node credentials")
		}
		runGit(t, gitDir, args...)
		hashes = append(hashes, strings.TrimSpace(runGit(t, gitDir, "rev-parse", "HEAD")))
		if index == 1 {
			runGit(t, gitDir, "tag", "v2026.7.0-dev.3")
			runGit(t, gitDir, "tag", "v1.36.0-katl.99")
		}
	}
	runGit(t, gitDir, "tag", "v2026.7.0-dev.4")

	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "notes", "2026.7.0-dev.4")
	cmd.Dir = gitDir
	cmd.Env = append(
		environmentWithout("KATL_RELEASE_TARGET"),
		"GITHUB_REF_TYPE=branch",
		"GITHUB_SHA=not-a-commit-in-the-test-repository",
		"KATL_RELEASE_REPOSITORY_URL=https://github.example/katl-dev/katl",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("notes failed: %v\n%s", err, output)
	}
	notes := string(output)
	for _, value := range []string{
		"## Support",
		"SUPPORT.md",
		"## Changes",
		"- Rotate node credentials",
		"- **release:** attest KatlOS artifacts",
		"- **release:** automate Kubernetes bundles",
		"https://github.example/katl-dev/katl/commit/" + hashes[3],
		"## Verify",
		"[PROVENANCE.md](https://github.example/katl-dev/katl/releases/download/v2026.7.0-dev.4/PROVENANCE.md)",
		"[SUPPORT.md](https://github.example/katl-dev/katl/releases/download/v2026.7.0-dev.4/SUPPORT.md)",
		"v2026.7.0-dev.3...v2026.7.0-dev.4",
		"https://github.example/katl-dev/katl/compare/v2026.7.0-dev.3...v2026.7.0-dev.4",
	} {
		if !strings.Contains(notes, value) {
			t.Fatalf("release notes missing %q:\n%s", value, notes)
		}
	}
	for _, value := range []string{commits[0], commits[1], commits[4], commits[5], "v1.36.0-katl.99"} {
		if strings.Contains(notes, value) {
			t.Fatalf("release notes unexpectedly contain %q:\n%s", value, notes)
		}
	}
}

func TestKatlReleaseArtifactNotesIncludeVersionedUpgradeGuide(t *testing.T) {
	repo := repoRoot(t)
	gitDir := t.TempDir()
	runGit(t, gitDir, "init", "--quiet")
	runGit(t, gitDir, "config", "user.name", "Katl Test")
	runGit(t, gitDir, "config", "user.email", "test@katl.dev")
	if err := os.WriteFile(filepath.Join(gitDir, "change"), []byte("release candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, gitDir, "add", "change")
	runGit(t, gitDir, "commit", "--quiet", "-m", "release: prepare candidate")

	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "notes", "2026.9.0-beta.18")
	cmd.Dir = gitDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("notes failed: %v\n%s", err, output)
	}
	notes := string(output)
	for _, want := range []string{"## Upgrade paths", "beta.17", "beta.14", "## Verify"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("release notes missing %q:\n%s", want, notes)
		}
	}
}

func TestKatlReleaseArtifactBuildKatlctl(t *testing.T) {
	repo := repoRoot(t)
	buildDir := t.TempDir()
	version := "2026.7.0-rc.1"
	commit := strings.Repeat("a", 40)
	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "build-katlctl", version)
	cmd.Dir = repo
	cmd.Env = append(
		os.Environ(),
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

	cmd := exec.Command(filepath.Join(repo, "scripts", "katl-release-artifacts"), "stage", version, output)
	cmd.Dir = repo
	cmd.Env = append(
		os.Environ(),
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
	for _, value := range []string{"## Packages", "| Kernel | `6.19.1-1.fc44.x86_64` |", "| systemd | `259.9-1.fc44.x86_64` |", "| drbd9 | `9.3.4` |", "## Support", "SUPPORT.md", "## Changes", "## Verify", "[PROVENANCE.md]("} {
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

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
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
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "KATL_FLAVOUR=lts", "KATL_MKOSI_BUILD_DIR="+buildDir, "KATL_ARCHITECTURE=x86_64")
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
