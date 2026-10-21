package main

import (
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
)

const repository = "katl-dev/katl"
const workflow = ".github/workflows/release-artifacts.yml"

type command func(string, ...string) ([]byte, error)

func main() {
	call := func(name string, args ...string) ([]byte, error) {
		cmd := exec.Command(name, args...)
		cmd.Stderr = os.Stderr
		return cmd.Output()
	}
	if err := run(os.Args[1:], os.Stdout, os.Stderr, call); err != nil {
		fmt.Fprintln(os.Stderr, "katl-release:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, call command) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, "usage: scripts/katl-release prepare [--ref main] VERSION\n       scripts/katl-release inspect RUN_ID\n       scripts/katl-release download RUN_ID DIRECTORY\n\nPreparation is read-only. Publication requires approval in GitHub Actions.")
		return nil
	}
	if args[0] == "prepare" {
		return prepare(args[1:], stdout, stderr, call)
	}
	if (args[0] != "inspect" || len(args) != 2) && (args[0] != "download" || len(args) != 3) {
		return errors.New("expected prepare, inspect RUN_ID, or download RUN_ID DIRECTORY; use --help")
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

func api(call command, endpoint string, value any) error {
	data, err := call("gh", "api", "repos/"+repository+"/"+endpoint)
	if err != nil {
		return fmt.Errorf("read GitHub %s: %w (check gh auth status and repository access)", endpoint, err)
	}
	return json.Unmarshal(data, value)
}

func validateVersion(version string, call command) error {
	if strings.HasPrefix(version, "v") {
		return errors.New("use the embedded version without the v prefix")
	}
	_, err := call("scripts/katl-release-artifacts", "version", "workflow_dispatch", "branch", "main", version)
	return err
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
	if err := validateVersion(version, call); err != nil {
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
	var environment struct {
		Rules []struct {
			Type      string            `json:"type"`
			Reviewers []json.RawMessage `json:"reviewers"`
		} `json:"protection_rules"`
	}
	if err := api(call, "environments/release", &environment); err != nil {
		return err
	}
	protected := false
	for _, rule := range environment.Rules {
		protected = protected || rule.Type == "required_reviewers" && len(rule.Reviewers) > 0
	}
	if !protected {
		return errors.New("configure required reviewers on the GitHub release environment before pushing a tag")
	}
	fmt.Fprintf(stdout, "Release: %s\nSource:  %s (%s)\nApproval: release environment has required reviewers\n\nReview release guidance in docs/releases/%s.md if needed, and record qualification in a release Bead.\nAfter source review, start the candidate build with:\n\n", tag, source.SHA, *ref, version)
	if _, err := os.Stat(".jj"); err == nil {
		fmt.Fprintf(stdout, "jj git fetch --remote origin\njj tag set %s --revision %s\njj git push --remote origin --tag %s\n", tag, source.SHA, tag)
	} else {
		fmt.Fprintf(stdout, "git fetch origin\ngit tag %s %s\ngit push origin refs/tags/%s\n", tag, source.SHA, tag)
	}
	fmt.Fprintln(stdout, "\nThen find the tag run in GitHub Actions and use:\nscripts/katl-release inspect RUN_ID\nscripts/katl-release download RUN_ID DIRECTORY\n\nFollow docs/development/releases.md before approving publication.")
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
	if err := validateVersion(strings.TrimPrefix(run.Tag, "v"), call); err != nil {
		return result, fmt.Errorf("run is not a KatlOS release: %w", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(run.Commit) {
		return result, errors.New("run has no full source commit SHA")
	}
	name := "katl-" + run.Tag[1:] + "-x86_64-" + run.Commit[:12]
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
	names, err := verifyChecksums(dir)
	if err != nil {
		return err
	}
	for _, name := range append(names, "SHA256SUMS") {
		fmt.Fprintln(stderr, "Verifying provenance:", name)
		if _, err := call("gh", "attestation", "verify", filepath.Join(dir, name), "--repo", repository,
			"--signer-workflow", repository+"/"+workflow, "--source-ref", "refs/tags/"+release.Run.Tag,
			"--source-digest", release.Run.Commit); err != nil {
			return fmt.Errorf("provenance verification failed for %s; check gh attestation verify, network access, and a writable gh cache: %w", name, err)
		}
	}
	// Reruns can replace artifacts under the same name. Qualification belongs to one attempt and artifact ID.
	current, err := inspect(release.Run.ID, call)
	if err != nil {
		return err
	}
	if current.Run.Attempt != release.Run.Attempt || current.Run.Commit != release.Run.Commit || current.Artifact != release.Artifact {
		return errors.New("candidate changed during verification; download into a fresh directory and qualify again")
	}
	fmt.Fprintln(stderr, "Integrity and provenance verified. VM and hands-on qualification are still required before approval.")
	return nil
}

func verifyChecksums(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return nil, err
	}
	expected := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if len(line) < 67 || line[64:66] != "  " {
			return nil, errors.New("invalid SHA256SUMS entry")
		}
		digest, name := line[:64], line[66:]
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, errors.New("invalid SHA256SUMS digest")
		}
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\r\n") || name == "SHA256SUMS" || expected[name] != "" {
			return nil, fmt.Errorf("unsafe or duplicate SHA256SUMS filename %q", name)
		}
		expected[name] = digest
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
