package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/flavour"
	"github.com/katl-dev/katl/internal/releasestate"
	"github.com/katl-dev/katl/internal/releaseversion"
)

const repository = "katl-dev/katl"
const workflow = ".github/workflows/release-artifacts.yml"

// command runs a program and returns its stdout. A program that exits
// non-zero fails with an *exec.ExitError that carries its stderr.
type command func(string, ...string) ([]byte, error)

func main() {
	call := func(name string, args ...string) ([]byte, error) {
		cmd := exec.Command(name, args...)
		var stderr bytes.Buffer
		cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
		out, err := cmd.Output()
		if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
			exit.Stderr = stderr.Bytes()
		}
		return out, err
	}
	if err := run(os.Args[1:], os.Stdout, os.Stderr, call); err != nil {
		fmt.Fprintln(os.Stderr, "katl-release:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, call command) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, "usage: scripts/katl-release prepare [--ref main] VERSION\n       scripts/katl-release inspect RUN_ID\n       scripts/katl-release download RUN_ID DIRECTORY\n       scripts/katl-release verify TAG COMMIT DIRECTORY\n       scripts/katl-release checksums DIRECTORY\n       scripts/katl-release equivalent BUNDLE FLAVOUR=SHA256SUMS...\n       scripts/katl-release regression VERSION COMMIT\n       scripts/katl-release next [--ref REF] [--dispatch COMMIT] [--retry REASON]\n       scripts/katl-release status VERSION\n       scripts/katl-release version VERSION\n       scripts/katl-release stable-tags [TAG...]\n       scripts/katl-release bundle-name VERSION COMMIT\n       scripts/katl-release record VERSION building|failed|qualified [DIGEST]\n\nPreparation is read-only. Publication follows successful VM qualification in GitHub Actions.")
		return nil
	}
	if args[0] == "prepare" {
		return prepare(args[1:], stdout, stderr, call)
	}
	if args[0] == "regression" {
		return regression(args[1:], stdout, stderr, call)
	}
	if args[0] == "record" {
		return record(args[1:], stderr, call)
	}
	if args[0] == "version" || args[0] == "stable-tags" || args[0] == "bundle-name" {
		return versionCommand(args, stdout)
	}
	if args[0] == "next" {
		return next(context.Background(), args[1:], stdout, newGitHubReader(call))
	}
	if args[0] == "status" {
		return status(context.Background(), args[1:], stdout, newGitHubReader(call))
	}
	if args[0] == "verify" {
		return verify(args[1:], stderr, call)
	}
	if args[0] == "checksums" && len(args) == 2 {
		_, err := verifyChecksums(args[1])
		return err
	}
	if args[0] == "equivalent" && len(args) >= 3 {
		return requireQualified(args[1], args[2:])
	}
	if (args[0] != "inspect" || len(args) != 2) && (args[0] != "download" || len(args) != 3) {
		return errors.New("expected prepare, inspect RUN_ID, download RUN_ID DIRECTORY, verify TAG COMMIT DIRECTORY, checksums DIRECTORY, or equivalent BUNDLE FLAVOUR=SHA256SUMS...; use --help")
	}
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || id <= 0 {
		return errors.New("RUN_ID must be a positive integer from the tag's Actions run URL")
	}
	candidate, err := inspect(id, call)
	if err != nil {
		return err
	}
	if args[0] == "download" {
		if err := download(candidate, args[2], stderr, call); err != nil {
			return err
		}
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(candidate)
}

// verify checks a release bundle that is already on disk, such as the bundle
// the tag workflow is about to publish.
func verify(args []string, stderr io.Writer, call command) error {
	if len(args) != 3 {
		return errors.New("usage: scripts/katl-release verify TAG COMMIT DIRECTORY")
	}
	tag, commit, dir := args[0], args[1], args[2]
	if !strings.HasPrefix(tag, "v") {
		return errors.New("TAG must be a KatlOS release tag such as v2026.10.2")
	}
	if _, err := validateVersion(tag[1:]); err != nil {
		return fmt.Errorf("TAG is not a KatlOS release: %w", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		return errors.New("COMMIT must be the tag's full source commit SHA")
	}

	if err := verifyBundle(dir, tag, commit, stderr, call); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "Integrity and provenance verified.")
	return nil
}

func api(call command, endpoint string, value any) error {
	data, err := call("gh", "api", "repos/"+repository+"/"+endpoint)
	if err != nil {
		return fmt.Errorf("read GitHub %s: %w (check gh auth status and repository access)", endpoint, err)
	}
	return json.Unmarshal(data, value)
}

func validateVersion(version string) (releaseversion.Version, error) {
	if strings.HasPrefix(version, "v") {
		return releaseversion.Version{}, errors.New("use the embedded version without the v prefix")
	}
	return releaseversion.Parse(version)
}

func prepare(args []string, stdout, stderr io.Writer, call command) error {
	flags := flag.NewFlagSet("prepare", flag.ContinueOnError)
	flags.SetOutput(stderr)
	ref := flags.String("ref", "main", "published source ref or full commit SHA")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || *ref == "" {
		return errors.New("usage: scripts/katl-release prepare [--ref main] VERSION")
	}
	version := flags.Arg(0)
	if _, err := validateVersion(version); err != nil {
		return fmt.Errorf("invalid release version: %w", err)
	}
	var source struct{ SHA string }
	if err := api(call, "commits/"+url.PathEscape(*ref), &source); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(source.SHA) {
		return errors.New("GitHub did not return a full source commit SHA")
	}
	var refs []struct{ Ref string }
	tag := "v" + version
	if err := api(call, "git/matching-refs/tags/"+tag, &refs); err != nil {
		return err
	}
	for _, ref := range refs {
		if ref.Ref == "refs/tags/"+tag {
			return fmt.Errorf("tag %s already exists; inspect its run or choose a new version", tag)
		}
	}
	fmt.Fprintf(stdout, "Release: %s\nSource:  %s (%s)\nPublication: automatic after regression and production VM qualification\n\nReview release guidance in docs/releases/%s.md or, until it is published, docs/releases/next.md.\nAfter source review, start the candidate build with:\n\n", tag, source.SHA, *ref, version)
	if _, err := os.Stat(".jj"); err == nil {
		fmt.Fprintf(stdout, "jj git fetch --remote origin\njj tag set %s --revision %s\njj git push --remote origin --tag %s\n", tag, source.SHA, tag)
	} else {
		fmt.Fprintf(stdout, "git fetch origin\ngit tag %s %s\ngit push origin refs/tags/%s\n", tag, source.SHA, tag)
	}
	fmt.Fprintln(stdout, "\nThen find the tag run in GitHub Actions and use:\nscripts/katl-release inspect RUN_ID\nscripts/katl-release download RUN_ID DIRECTORY\n\nFollow docs/development/releases.md and monitor Release validation through publication.")
	return nil
}

type workflowRun struct {
	ID         int64  `json:"id"`
	Attempt    int    `json:"run_attempt"`
	Tag        string `json:"head_branch"`
	Commit     string `json:"head_sha"`
	Event      string `json:"event"`
	Path       string `json:"path"`
	URL        string `json:"html_url"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type artifact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Digest  string `json:"digest"`
	Expired bool   `json:"expired"`
}

type candidate struct {
	Run      workflowRun `json:"run"`
	Artifact artifact    `json:"artifact"`
}

func inspect(id int64, call command) (candidate, error) {
	var result candidate
	if err := api(call, fmt.Sprintf("actions/runs/%d", id), &result.Run); err != nil {
		return result, err
	}
	run := result.Run
	if run.ID != id || run.Attempt < 1 || run.Event != "push" || run.Path != workflow || !strings.HasPrefix(run.Tag, "v") {
		return result, errors.New("select a pushed KatlOS tag run from release-artifacts.yml, not a branch or manual build")
	}
	version, err := validateVersion(strings.TrimPrefix(run.Tag, "v"))
	if err != nil {
		return result, fmt.Errorf("run is not a KatlOS release: %w", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(run.Commit) {
		return result, errors.New("run has no full source commit SHA")
	}
	name := releasestate.BundleName(version, run.Commit)
	// A run can contain hundreds of intermediate artifacts; select only the final bundle.
	for page := 1; ; page++ {
		var response struct{ Artifacts []artifact }
		if err := api(call, fmt.Sprintf("actions/runs/%d/artifacts?per_page=100&page=%d", id, page), &response); err != nil {
			return result, err
		}
		for _, artifact := range response.Artifacts {
			if artifact.Name != name {
				continue
			}
			if result.Artifact.ID != 0 {
				return result, errors.New("run has multiple final bundles; resolve the ambiguity before qualification")
			}
			result.Artifact = artifact
		}
		if len(response.Artifacts) < 100 {
			break
		}
	}
	if result.Artifact.ID == 0 {
		return result, fmt.Errorf("final bundle %s is unavailable (run %s/%s); inspect %s", name, run.Status, run.Conclusion, run.URL)
	}
	if result.Artifact.Expired {
		return result, errors.New("final bundle expired; a rebuilt candidate requires fresh qualification")
	}
	return result, nil
}

func download(release candidate, dir string, stderr io.Writer, call command) error {
	// Reserve a fresh directory so interrupted downloads cannot mix candidate bytes.
	if err := os.Mkdir(dir, 0o755); err != nil {
		return fmt.Errorf("create fresh download directory: %w", err)
	}
	fmt.Fprintf(stderr, "Downloading artifact %d from run %d attempt %d into %s\n", release.Artifact.ID, release.Run.ID, release.Run.Attempt, dir)
	if _, err := call("gh", "run", "download", strconv.FormatInt(release.Run.ID, 10), "--repo", repository, "--name", release.Artifact.Name, "--dir", dir); err != nil {
		return fmt.Errorf("download failed; retained %s for inspection: %w", dir, err)
	}
	if err := verifyBundle(dir, release.Run.Tag, release.Run.Commit, stderr, call); err != nil {
		return err
	}
	// Reruns can replace artifacts under the same name. Qualification belongs to one attempt and artifact ID.
	current, err := inspect(release.Run.ID, call)
	if err != nil {
		return err
	}
	if current.Run.Attempt != release.Run.Attempt || current.Run.Commit != release.Run.Commit || current.Artifact != release.Artifact {
		return errors.New("candidate changed during verification; download into a fresh directory and qualify again")
	}
	fmt.Fprintln(stderr, "Integrity and provenance verified. Release validation must pass before publication.")
	return nil
}

// verifyBundle checks every file in dir against SHA256SUMS and against the
// release workflow's attestation for tag and commit.
func verifyBundle(dir, tag, commit string, stderr io.Writer, call command) error {
	names, err := verifyChecksums(dir)
	if err != nil {
		return err
	}

	p := &provenance{call: call, repository: repository}
	defer p.close()
	for _, name := range append(names, "SHA256SUMS") {
		fmt.Fprintln(stderr, "Verifying provenance:", name)
		if _, err := p.verify(filepath.Join(dir, name), "refs/tags/"+tag, commit); err != nil {
			return err
		}
	}
	return nil
}

// provenance verifies files against their attestations from
// release-artifacts.yml with gh attestation verify.
type provenance struct {
	call       command
	repository string
	// scratch holds the trusted root once it is fetched.
	scratch, root string
}

// errUnattested marks a verification that gh attestation verify completed
// and refused, as opposed to one it could not complete.
var errUnattested = errors.New("gh attestation verify found no valid attestation")

// verify checks that file is attested by release-artifacts.yml at commit on
// sourceRef, and returns the verified attestations as gh's JSON. A failure
// is classified as ghFailure classifies it, and wraps errUnattested when gh
// ran and refused the file rather than failing to reach GitHub or Sigstore.
func (p *provenance) verify(file, sourceRef, commit string) ([]byte, error) {
	if p.root == "" {
		// Without a trusted root, every gh attestation verify initializes the
		// Sigstore TUF trust root on its own, and any one of those
		// initializations can fail. Fetch the TUF-verified root once and verify
		// every file against it. The file stays outside any bundle directory so
		// it cannot join the published bundle.
		root, err := p.call("gh", "attestation", "trusted-root")
		if err != nil {
			return nil, fmt.Errorf("fetch Sigstore trusted root; check network access to tuf-repo-cdn.sigstore.dev and tuf-repo.github.com: %w",
				ghFailure("gh attestation trusted-root", err, stderrOf(err)))
		}
		scratch, err := os.MkdirTemp("", "katl-release-")
		if err != nil {
			return nil, err
		}
		p.scratch = scratch
		if err := os.WriteFile(filepath.Join(scratch, "trusted_root.jsonl"), root, 0o600); err != nil {
			return nil, err
		}
		p.root = filepath.Join(scratch, "trusted_root.jsonl")
	}
	out, err := p.call("gh", "attestation", "verify", file, "--repo", p.repository,
		"--signer-workflow", p.repository+"/"+workflow, "--source-ref", sourceRef,
		"--source-digest", commit, "--custom-trusted-root", p.root, "--format", "json")
	if err != nil {
		failure := ghFailure("gh attestation verify", err, stderrOf(err))
		var api *apiError
		var exit *exec.ExitError
		if errors.As(err, &exit) && errors.As(failure, &api) &&
			!api.auth && !api.network && (api.status == 0 || api.status == 404) {
			failure = fmt.Errorf("%w: %w", errUnattested, failure)
		}
		return nil, fmt.Errorf("provenance verification failed for %s; check gh attestation verify and network access: %w", filepath.Base(file), failure)
	}
	return out, nil
}

// stderrOf is what a command that exited non-zero printed on stderr.
func stderrOf(err error) string {
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		return string(exit.Stderr)
	}
	return ""
}

// close removes the trusted root; the next verification fetches it again.
func (p *provenance) close() {
	if p == nil || p.scratch == "" {
		return
	}
	os.RemoveAll(p.scratch)
	p.scratch, p.root = "", ""
}

// parseChecksums reads the SHA256SUMS file at path into a name-to-digest map.
func parseChecksums(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeChecksums(data)
}

// decodeChecksums reads SHA256SUMS content into a name-to-digest map.
func decodeChecksums(data []byte) (map[string]string, error) {
	sums := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if len(line) < 67 || line[64:66] != "  " {
			return nil, errors.New("invalid SHA256SUMS entry")
		}
		digest, name := line[:64], line[66:]
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, errors.New("invalid SHA256SUMS digest")
		}
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\r\n") || name == "SHA256SUMS" || sums[name] != "" {
			return nil, fmt.Errorf("unsafe or duplicate SHA256SUMS filename %q", name)
		}
		sums[name] = digest
	}
	return sums, nil
}

func verifyChecksums(dir string) ([]string, error) {
	expected, err := parseChecksums(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("release bundle contains a nonregular file: %s", entry.Name())
		}
		if entry.Name() == "SHA256SUMS" {
			continue
		}
		if expected[entry.Name()] == "" {
			return nil, fmt.Errorf("release file is not checksummed: %s", entry.Name())
		}
		file, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return nil, err
		}
		if hex.EncodeToString(hash.Sum(nil)) != expected[entry.Name()] {
			return nil, fmt.Errorf("checksum mismatch: %s", entry.Name())
		}
		delete(expected, entry.Name())
		names = append(names, entry.Name())
	}
	if len(expected) != 0 {
		return nil, errors.New("release bundle is missing files listed in SHA256SUMS")
	}
	if len(names) == 0 {
		return nil, errors.New("release bundle is empty")
	}
	sort.Strings(names)
	return names, nil
}

// renderedNotes is the one release file that differs between the assembled
// flavours and the bundle, because the bundle renders it.
const renderedNotes = "RELEASE_NOTES.md"

// requireQualified checks that the bundle in dir consists of the bytes that
// production VM qualification verified. Each flavour is a NAME=SHA256SUMS
// argument naming the checksums the qualification run verified.
func requireQualified(dir string, flavours []string) error {
	if _, err := verifyChecksums(dir); err != nil {
		return err
	}
	bundle, err := parseChecksums(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	qualified := make(map[string]bool)
	seen := make(map[string]bool)
	for _, argument := range flavours {
		name, path, ok := strings.Cut(argument, "=")
		if !ok || name == "" || path == "" {
			return fmt.Errorf("expected FLAVOUR=SHA256SUMS, got %q", argument)
		}
		if _, err := flavour.Normalize(name); err != nil {
			return fmt.Errorf("unknown flavour %q; choose %s or %s", name, flavour.Standard, flavour.LTS)
		}
		if seen[name] {
			return fmt.Errorf("duplicate qualified checksums for %s", name)
		}
		seen[name] = true
		evidence, err := parseChecksums(path)
		if err != nil {
			return fmt.Errorf("qualified checksums for %s: %w", name, err)
		}
		for file, digest := range evidence {
			if file == renderedNotes {
				continue
			}
			published, ok := bundle[file]
			if !ok {
				return fmt.Errorf("%s (%s) was qualified but is missing from the bundle", file, name)
			}
			if published != digest {
				return fmt.Errorf("%s (%s) differs from qualified bytes", file, name)
			}
			qualified[file] = true
		}
	}
	for _, name := range flavour.Supported() {
		if !seen[name] {
			return fmt.Errorf("no qualified checksums for %s", name)
		}
	}
	for file := range bundle {
		if file != renderedNotes && !qualified[file] {
			return fmt.Errorf("published file was not qualified: %s", file)
		}
	}
	return nil
}
