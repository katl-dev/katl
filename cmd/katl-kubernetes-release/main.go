package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/installer/kubernetescompat"
)

const (
	defaultManifest             = "mkosi.profiles/kubernetes-sysext/kubernetes.env"
	defaultCompatibilityCatalog = "internal/installer/kubernetescompat/catalog.json"
)

var payloadPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

type releaseIdentity struct {
	PayloadVersion  string `json:"payloadVersion"`
	ArtifactVersion string `json:"artifactVersion"`
	Image           string `json:"image"`
}

type packageQuery func(name, selector, baseURL, command string) (string, error)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, queryPackage); err != nil {
		fmt.Fprintf(os.Stderr, "katl-kubernetes-release: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, query packageQuery) error {
	if len(args) == 0 {
		return errors.New("command is required: identity, prepare, or record-compatibility")
	}
	switch args[0] {
	case "identity":
		return runIdentity(args[1:], stdout, stderr)
	case "prepare":
		return runPrepare(args[1:], stdout, stderr, query)
	case "record-compatibility":
		return runRecordCompatibility(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unsupported command %q", args[0])
	}
}

func runRecordCompatibility(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-kubernetes-release record-compatibility", flag.ContinueOnError)
	flags.SetOutput(stderr)
	catalogPath := flags.String("catalog", defaultCompatibilityCatalog, "release compatibility catalog to update")
	payload := flags.String("payload-version", "", "exact Kubernetes payload version")
	artifact := flags.String("artifact-version", "", "immutable Katl bundle build version")
	manifestDigest := flags.String("manifest-digest", "", "published OCI manifest digest")
	architecture := flags.String("architecture", "x86_64", "supported architecture")
	runtimeInterfaces := flags.String("runtime-interfaces", "katl-runtime-1", "comma-separated supported KatlOS runtime interfaces")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if payloadPattern.FindStringSubmatch(strings.TrimSpace(*payload)) == nil {
		return fmt.Errorf("--payload-version must look like v1.36.1")
	}
	wantArtifactPrefix := strings.TrimSpace(*payload) + "-katl."
	if !strings.HasPrefix(strings.TrimSpace(*artifact), wantArtifactPrefix) {
		return fmt.Errorf("--artifact-version must look like %s1", wantArtifactPrefix)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(strings.TrimSpace(*manifestDigest)) {
		return fmt.Errorf("--manifest-digest must be a sha256 OCI manifest digest")
	}
	interfaces := splitNonEmpty(*runtimeInterfaces)
	if len(interfaces) == 0 {
		return fmt.Errorf("--runtime-interfaces must contain at least one value")
	}
	data, err := os.ReadFile(*catalogPath)
	if err != nil {
		return fmt.Errorf("read compatibility catalog: %w", err)
	}
	catalog, err := kubernetescompat.Decode(data)
	if err != nil {
		return err
	}
	entry := kubernetescompat.Entry{
		KubernetesVersion: strings.TrimSpace(*payload),
		Bundle:            "ghcr.io/katl-dev/kubernetes:" + strings.TrimSpace(*artifact) + "@" + strings.TrimSpace(*manifestDigest),
		Architectures:     []string{strings.TrimSpace(*architecture)},
		RuntimeInterfaces: interfaces,
	}
	catalog, err = kubernetescompat.Upsert(catalog, entry)
	if err != nil {
		return err
	}
	updated, err := kubernetescompat.Marshal(catalog)
	if err != nil {
		return err
	}
	if bytes.Equal(data, updated) {
		fmt.Fprintf(stdout, "compatibility already records %s as %s\n", entry.KubernetesVersion, entry.Bundle)
		return nil
	}
	info, err := os.Stat(*catalogPath)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(*catalogPath), ".kubernetes-compatibility-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(updated); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, *catalogPath); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "recorded %s as %s\n", entry.KubernetesVersion, entry.Bundle)
	return nil
}

func splitNonEmpty(value string) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func runIdentity(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-kubernetes-release identity", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifest := flags.String("manifest", defaultManifest, "Kubernetes release manifest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	state, _, err := readState(*manifest)
	if err != nil {
		return err
	}
	identity := releaseIdentity{
		PayloadVersion:  state.payload,
		ArtifactVersion: fmt.Sprintf("%s-katl.%d", state.payload, state.revision),
	}
	identity.Image = "ghcr.io/katl-dev/kubernetes:" + identity.ArtifactVersion
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, string(data))
	return nil
}

func runPrepare(args []string, stdout, stderr io.Writer, query packageQuery) error {
	flags := flag.NewFlagSet("katl-kubernetes-release prepare", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifest := flags.String("manifest", defaultManifest, "Kubernetes release manifest to update")
	payload := flags.String("payload-version", "", "exact Kubernetes payload version, for example v1.36.1")
	repoquery := flags.String("repoquery", "dnf", "dnf-compatible repository query command")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	match := payloadPattern.FindStringSubmatch(strings.TrimSpace(*payload))
	if match == nil {
		return fmt.Errorf("--payload-version must look like v1.36.1")
	}
	state, data, err := readState(*manifest)
	if err != nil {
		return err
	}
	minor := "v" + match[1] + "." + match[2]
	if minor != state.minor {
		return fmt.Errorf("payload minor %s does not match selected release line %s", minor, state.minor)
	}
	baseURL := "https://pkgs.k8s.io/core:/stable:/" + minor + "/rpm/"
	upstream := strings.TrimPrefix(*payload, "v")
	versions := make(map[string]string, 4)
	for _, name := range []string{"kubeadm", "kubelet", "kubectl"} {
		version, err := query(name, name+"-"+upstream, baseURL, *repoquery)
		if err != nil {
			return err
		}
		if packageUpstream(version) != upstream {
			return fmt.Errorf("%s resolved to %s, want Kubernetes %s", name, version, upstream)
		}
		versions[name] = version
	}
	cri, err := query("cri-tools", "cri-tools-"+match[1]+"."+match[2]+".*", baseURL, *repoquery)
	if err != nil {
		return err
	}
	if err := validateCRITools(cri, match); err != nil {
		return err
	}
	versions["cri-tools"] = cri

	replacements := map[string]string{
		"KATL_KUBERNETES_PAYLOAD_DEFAULT":           *payload,
		"KATL_KUBERNETES_ARTIFACT_REVISION_DEFAULT": "1",
		"KATL_KUBERNETES_KUBEADM_VERSION":           versions["kubeadm"],
		"KATL_KUBERNETES_KUBELET_VERSION":           versions["kubelet"],
		"KATL_KUBERNETES_KUBECTL_VERSION":           versions["kubectl"],
		"KATL_KUBERNETES_CRITOOLS_VERSION":          versions["cri-tools"],
	}
	updated, err := replaceManifestValues(data, replacements)
	if err != nil {
		return err
	}
	info, err := os.Stat(*manifest)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(*manifest), ".kubernetes-release-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(updated); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, *manifest); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "prepared %s-katl.1\n", *payload)
	for _, name := range []string{"kubeadm", "kubelet", "kubectl", "cri-tools"} {
		fmt.Fprintf(stdout, "%s=%s\n", name, versions[name])
	}
	return nil
}

type releaseState struct {
	minor    string
	payload  string
	revision int
}

func readState(path string) (releaseState, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return releaseState{}, nil, fmt.Errorf("read release manifest: %w", err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := manifestAssignment(line); ok {
			values[key] = value
		}
	}
	state := releaseState{minor: values["KATL_KUBERNETES_MINOR"], payload: values["KATL_KUBERNETES_PAYLOAD_DEFAULT"]}
	state.revision, err = strconv.Atoi(values["KATL_KUBERNETES_ARTIFACT_REVISION_DEFAULT"])
	if err != nil || state.revision < 1 {
		return releaseState{}, nil, errors.New("release manifest has invalid artifact revision")
	}
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+$`).MatchString(state.minor) {
		return releaseState{}, nil, errors.New("release manifest has invalid Kubernetes minor")
	}
	if match := payloadPattern.FindStringSubmatch(state.payload); match == nil || "v"+match[1]+"."+match[2] != state.minor {
		return releaseState{}, nil, errors.New("release manifest payload does not match its Kubernetes minor")
	}
	return state, data, nil
}

func manifestAssignment(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, `: "${`) && strings.HasSuffix(line, `}"`) {
		line = strings.TrimSuffix(strings.TrimPrefix(line, `: "${`), `}"`)
		parts := strings.SplitN(line, ":=", 2)
		if len(parts) == 2 {
			return parts[0], parts[1], true
		}
	}
	parts := strings.SplitN(line, "=", 2)
	if len(parts) == 2 && strings.HasPrefix(parts[0], "KATL_KUBERNETES_") && !strings.Contains(parts[1], "${") {
		return parts[0], strings.Trim(parts[1], `"`), true
	}
	return "", "", false
}

func replaceManifestValues(data []byte, replacements map[string]string) ([]byte, error) {
	lines := strings.Split(string(data), "\n")
	seen := make(map[string]bool, len(replacements))
	for i, line := range lines {
		key, _, ok := manifestAssignment(line)
		value, wanted := replacements[key]
		if !ok || !wanted {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), `: "${`) {
			lines[i] = `: "${` + key + `:=` + value + `}"`
		} else {
			lines[i] = key + "=" + value
		}
		seen[key] = true
	}
	for key := range replacements {
		if !seen[key] {
			return nil, fmt.Errorf("release manifest is missing %s", key)
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func queryPackage(name, selector, baseURL, command string) (string, error) {
	cmd := exec.Command(command,
		"repoquery",
		"--repofrompath=kubernetes,"+baseURL,
		"--repo=kubernetes",
		"--arch=x86_64",
		"--latest-limit=1",
		"--qf", "%{epoch}:%{version}-%{release}",
		selector,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("query %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Fields(stdout.String())
	if len(lines) != 1 || !regexp.MustCompile(`^[0-9]+:[0-9]+\.[0-9]+\.[0-9]+-[A-Za-z0-9._+~-]+$`).MatchString(lines[0]) {
		return "", fmt.Errorf("query %s returned invalid version %q", name, strings.TrimSpace(stdout.String()))
	}
	return lines[0], nil
}

func packageUpstream(version string) string {
	version = strings.TrimPrefix(version, "0:")
	return strings.SplitN(version, "-", 2)[0]
}

func validateCRITools(version string, payload []string) error {
	match := payloadPattern.FindStringSubmatch("v" + packageUpstream(version))
	if match == nil || match[1] != payload[1] || match[2] != payload[2] {
		return fmt.Errorf("cri-tools resolved to incompatible version %s", version)
	}
	criPatch, _ := strconv.Atoi(match[3])
	payloadPatch, _ := strconv.Atoi(payload[3])
	if criPatch > payloadPatch {
		return fmt.Errorf("cri-tools %s is newer than payload v%s.%s.%s", version, payload[1], payload[2], payload[3])
	}
	return nil
}
