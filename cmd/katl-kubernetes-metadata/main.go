package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/artifact"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "katl-kubernetes-metadata: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, environ []string) error {
	if len(args) == 0 {
		return fmt.Errorf("command is required: write-from-log")
	}
	switch args[0] {
	case "write-from-log":
		return runWriteFromLog(args[1:], stdout, stderr, environ)
	case "-h", "--help":
		fmt.Fprintln(stdout, "Usage: katl-kubernetes-metadata write-from-log --artifact PATH --log PATH --repo-id ID --repo-base-url URL --repo-minor MINOR")
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runWriteFromLog(args []string, stdout, stderr io.Writer, environ []string) error {
	flags := flag.NewFlagSet("katl-kubernetes-metadata write-from-log", flag.ContinueOnError)
	flags.SetOutput(stderr)
	artifactPath := flags.String("artifact", filepath.Join("_build", "mkosi", "katl-kubernetes.raw"), "Kubernetes sysext artifact")
	logPath := flags.String("log", "", "mkosi output log containing resolved package lines")
	runtimeArtifact := flags.String("runtime-artifact", filepath.Join("_build", "mkosi", "katl-runtime-root.squashfs"), "compatible runtime root artifact")
	runtimeMetadata := flags.String("runtime-metadata", filepath.Join("_build", "mkosi", "katl-runtime-root.squashfs.json"), "compatible runtime root metadata")
	repoID := flags.String("repo-id", "", "Kubernetes package repository ID")
	repoBaseURL := flags.String("repo-base-url", "", "Kubernetes package repository base URL")
	repoMinor := flags.String("repo-minor", "", "Kubernetes package minor")
	expectedPayload := flags.String("expected-payload-version", "", "optional expected Kubernetes payload version")
	expectedKubeadm := flags.String("expected-kubeadm-version", "", "optional expected kubeadm package version")
	expectedKubelet := flags.String("expected-kubelet-version", "", "optional expected kubelet package version")
	expectedKubectl := flags.String("expected-kubectl-version", "", "optional expected kubectl package version")
	expectedCRITools := flags.String("expected-cri-tools-version", "", "optional expected cri-tools package version")
	expectedEthtool := flags.String("expected-ethtool-version", "", "optional expected ethtool package version")
	expectedSocat := flags.String("expected-socat-version", "", "optional expected socat package version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	for name, value := range map[string]string{
		"--log":           *logPath,
		"--repo-id":       *repoID,
		"--repo-base-url": *repoBaseURL,
		"--repo-minor":    *repoMinor,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}
	packages, err := resolvePackages(absPath(repoRoot, *logPath), *repoID)
	if err != nil {
		return err
	}
	expected := map[string]string{
		"kubeadm":   *expectedKubeadm,
		"kubelet":   *expectedKubelet,
		"kubectl":   *expectedKubectl,
		"cri-tools": *expectedCRITools,
		"ethtool":   *expectedEthtool,
		"socat":     *expectedSocat,
	}
	for name, want := range expected {
		if strings.TrimSpace(want) != "" && packages[name] != want {
			return fmt.Errorf("%s resolved as %s, want %s", name, packages[name], want)
		}
	}
	payloadVersion, err := payloadVersionFromPackage(packages["kubeadm"])
	if err != nil {
		return err
	}
	if strings.TrimSpace(*expectedPayload) != "" && payloadVersion != *expectedPayload {
		return fmt.Errorf("Kubernetes payload version %s resolved from kubeadm, want %s", payloadVersion, *expectedPayload)
	}
	if !strings.HasPrefix(payloadVersion, *repoMinor+".") {
		return fmt.Errorf("Kubernetes payload version %s does not match selected minor %s", payloadVersion, *repoMinor)
	}

	env := envMap(environ)
	created, err := buildTimestamp(env)
	if err != nil {
		return err
	}
	path := absPath(repoRoot, *artifactPath)
	size, digest, err := fileInfo(path)
	if err != nil {
		return err
	}
	if err := writeChecksum(path, digest); err != nil {
		return err
	}
	runtimeMetadataPath := absPath(repoRoot, *runtimeMetadata)
	runtimeSHA, err := runtimeDigest(runtimeMetadataPath)
	if err != nil {
		return err
	}
	metadata := artifact.LocalMeta{
		Name:             "kubernetes",
		Kind:             artifact.ArtifactSysext,
		Format:           "sysext",
		Path:             filepath.Base(path),
		SizeBytes:        size,
		SHA256:           digest,
		Version:          envDefaultFunc(env, "KATL_BUILD_COMMIT", func() string { return gitDescribe(repoRoot) }),
		PayloadVersion:   payloadVersion,
		Architecture:     envDefault(env, "KATL_ARCHITECTURE", hostArchitecture()),
		SourceRepo:       &artifact.SourceRepo{ID: *repoID, BaseURL: *repoBaseURL, Minor: *repoMinor},
		PackageVersions:  packages,
		RuntimeInterface: "katl-runtime-1",
		CompatibleRuntime: &artifact.Compat{
			Interface:      "katl-runtime-1",
			ArtifactPath:   filepath.Base(absPath(repoRoot, *runtimeArtifact)),
			ArtifactSHA256: runtimeSHA,
		},
		Created: created,
	}
	if err := writeJSON(path+".json", metadata); err != nil {
		return err
	}
	fmt.Fprintln(stdout, digest)
	return nil
}

func resolvePackages(logPath, repoID string) (map[string]string, error) {
	file, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("open mkosi log %s: %w", logPath, err)
	}
	defer file.Close()

	packages := map[string]string{
		"kubeadm":   "",
		"kubelet":   "",
		"kubectl":   "",
		"cri-tools": "",
		"ethtool":   "",
		"socat":     "",
	}
	repositories := make(map[string]string, len(packages))
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		if _, ok := packages[fields[0]]; !ok {
			continue
		}
		packages[fields[0]] = fields[2]
		repositories[fields[0]] = fields[3]
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mkosi log %s: %w", logPath, err)
	}
	for name, version := range packages {
		if version == "" {
			return nil, fmt.Errorf("could not determine resolved version for %s", name)
		}
		if isKubernetesPackage(name) && repositories[name] != repoID {
			return nil, fmt.Errorf("%s resolved from %s, want %s", name, repositories[name], repoID)
		}
	}
	return packages, nil
}

func isKubernetesPackage(name string) bool {
	switch name {
	case "kubeadm", "kubelet", "kubectl", "cri-tools":
		return true
	default:
		return false
	}
}

func payloadVersionFromPackage(version string) (string, error) {
	trimmed := strings.TrimSpace(version)
	if index := strings.Index(trimmed, ":"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	if index := strings.Index(trimmed, "-"); index >= 0 {
		trimmed = trimmed[:index]
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("could not derive Kubernetes payload version from package version %s", version)
	}
	for _, part := range parts {
		if part == "" {
			return "", fmt.Errorf("could not derive Kubernetes payload version from package version %s", version)
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return "", fmt.Errorf("could not derive Kubernetes payload version from package version %s", version)
			}
		}
	}
	return "v" + trimmed, nil
}

func runtimeDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read runtime metadata %s: %w", path, err)
	}
	var metadata struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("decode runtime metadata %s: %w", path, err)
	}
	if strings.TrimSpace(metadata.SHA256) == "" {
		return "", fmt.Errorf("runtime metadata missing sha256: %s", path)
	}
	return metadata.SHA256, nil
}

func buildTimestamp(env map[string]string) (string, error) {
	value := strings.TrimSpace(env["SOURCE_DATE_EPOCH"])
	if value == "" {
		return time.Now().UTC().Format(time.RFC3339), nil
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds < 0 {
		return "", fmt.Errorf("SOURCE_DATE_EPOCH must be a non-negative Unix timestamp")
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339), nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create metadata directory: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write metadata %s: %w", path, err)
	}
	return nil
}

func writeChecksum(path, digest string) error {
	content := fmt.Sprintf("%s  %s\n", digest, filepath.Base(path))
	if err := os.WriteFile(path+".sha256", []byte(content), 0o644); err != nil {
		return fmt.Errorf("write checksum %s: %w", path+".sha256", err)
	}
	return nil
}

func fileInfo(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("open artifact %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, "", fmt.Errorf("stat artifact %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, "", fmt.Errorf("artifact is not a regular file: %s", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return 0, "", fmt.Errorf("hash artifact %s: %w", path, err)
	}
	return info.Size(), hex.EncodeToString(hash.Sum(nil)), nil
}

func findRepoRoot() (string, error) {
	command := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := command.Output()
	if err == nil {
		return filepath.Clean(strings.TrimSpace(string(output))), nil
	}
	workingDirectory, workingErr := os.Getwd()
	if workingErr != nil {
		return "", fmt.Errorf("find repository root: %w", err)
	}
	return workingDirectory, nil
}

func gitDescribe(repoRoot string) string {
	command := exec.Command("git", "-C", repoRoot, "describe", "--always", "--dirty", "--abbrev=12")
	output, err := command.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

func hostArchitecture() string {
	command := exec.Command("uname", "-m")
	output, err := command.Output()
	if err == nil && strings.TrimSpace(string(output)) != "" {
		return strings.TrimSpace(string(output))
	}
	return runtime.GOARCH
}

func absPath(repoRoot, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(repoRoot, filepath.Clean(path))
}

func envMap(environ []string) map[string]string {
	env := make(map[string]string, len(environ))
	for _, item := range environ {
		name, value, ok := strings.Cut(item, "=")
		if ok {
			env[name] = value
		}
	}
	return env
}

func envDefault(env map[string]string, name, fallback string) string {
	if value := strings.TrimSpace(env[name]); value != "" {
		return value
	}
	return fallback
}

func envDefaultFunc(env map[string]string, name string, fallback func() string) string {
	if value := strings.TrimSpace(env[name]); value != "" {
		return value
	}
	return fallback()
}
