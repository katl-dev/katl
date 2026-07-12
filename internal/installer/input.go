package installer

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

type InputSource string

const (
	InputSourceKernelCmdline InputSource = "kernel-cmdline"
	InputSourceRunKatl       InputSource = "/run/katl"
	InputSourceEtcKatl       InputSource = "/etc/katl"
	InputSourceEmbeddedMedia InputSource = "embedded-media"
	InputSourceLocalFile     InputSource = "local-file"
	InputSourceManifest      InputSource = "manifest"
)

type InstallAction string

const (
	InstallActionRun           InstallAction = "run"
	InstallActionWaitForConfig InstallAction = "wait-for-config"
	InstallActionHoldForDebug  InstallAction = "hold-for-debug"
)

type BootInputRequest struct {
	KernelCmdline string
	Files         []BootInputFile
	Manifest      []byte
}

type BootInputFile struct {
	Source  InputSource
	Path    string
	Content []byte
}

type BootInput struct {
	ManifestPath    string
	ManifestURL     string
	ManifestSHA256  string
	BundlePath      string
	BundleURL       string
	BundleSHA256    string
	BundleDigest    string
	NodeName        string
	InstallMode     string
	HoldForDebug    bool
	WaitForConfig   bool
	ArtifactBaseURL string
	Action          InstallAction
	SelectedSources map[string]InputSource
	Logs            []string
}

func (i BootInput) CanMutateDisks() bool {
	return i.Action == InstallActionRun && i.InstallMode == "auto" && (i.ManifestPath != "" || (i.ManifestURL != "" && i.ManifestSHA256 != "") || i.BundlePath != "" || i.BundleURL != "")
}

func DiscoverBootInput(request BootInputRequest) (BootInput, error) {
	resolver := newInputResolver()

	if len(request.Manifest) > 0 {
		if err := resolver.applyManifest(request.Manifest); err != nil {
			return BootInput{}, err
		}
	}

	for _, file := range request.Files {
		if err := resolver.applyFile(file); err != nil {
			return BootInput{}, err
		}
	}

	if err := resolver.applyKernelCmdline(request.KernelCmdline); err != nil {
		return BootInput{}, err
	}

	input := resolver.input
	if input.InstallMode == "" {
		input.InstallMode = "manual"
	}
	switch {
	case input.HoldForDebug:
		input.Action = InstallActionHoldForDebug
	case input.WaitForConfig || (input.ManifestPath == "" && input.ManifestURL == "" && input.BundlePath == "" && input.BundleURL == ""):
		input.Action = InstallActionWaitForConfig
		input.WaitForConfig = true
	default:
		input.Action = InstallActionRun
	}

	return input, nil
}

type inputResolver struct {
	input BootInput
	rank  map[string]int
}

func newInputResolver() *inputResolver {
	return &inputResolver{
		input: BootInput{
			SelectedSources: make(map[string]InputSource),
		},
		rank: make(map[string]int),
	}
}

func (r *inputResolver) applyManifest(data []byte) error {
	var manifest struct {
		Node struct {
			Identity struct {
				Hostname string `yaml:"hostname"`
			} `yaml:"identity"`
		} `yaml:"node"`
		KatlosImage struct {
			URL string `yaml:"url"`
		} `yaml:"katlosImage"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("decode manifest-derived input: %w", err)
	}

	r.setString("nodeName", InputSourceManifest, manifest.Node.Identity.Hostname)
	if manifest.KatlosImage.URL != "" {
		baseURL, err := artifactBaseURL(manifest.KatlosImage.URL)
		if err != nil {
			return err
		}
		r.setString("artifactBaseURL", InputSourceManifest, baseURL)
	}

	return nil
}

func (r *inputResolver) applyFile(file BootInputFile) error {
	sourceRank, ok := sourceRanks[file.Source]
	if !ok {
		return fmt.Errorf("unsupported boot input source %q", file.Source)
	}

	var values bootInputValues
	if err := json.Unmarshal(file.Content, &values); err != nil {
		return fmt.Errorf("decode %s input %s: %w", file.Source, file.Path, err)
	}

	r.applyValues(file.Source, sourceRank, values)
	return nil
}

func (r *inputResolver) applyKernelCmdline(cmdline string) error {
	values, err := parseKernelCmdline(cmdline)
	if err != nil {
		return err
	}
	r.applyValues(InputSourceKernelCmdline, sourceRanks[InputSourceKernelCmdline], values)
	return nil
}

func (r *inputResolver) applyValues(source InputSource, rank int, values bootInputValues) {
	r.setStringWithRank("manifestPath", source, rank, values.ManifestPath)
	r.setStringWithRank("manifestURL", source, rank, values.ManifestURL)
	r.setStringWithRank("manifestSHA256", source, rank, values.ManifestSHA256)
	r.setStringWithRank("bundlePath", source, rank, values.BundlePath)
	r.setStringWithRank("bundleURL", source, rank, values.BundleURL)
	r.setStringWithRank("bundleSHA256", source, rank, values.BundleSHA256)
	r.setStringWithRank("bundleDigest", source, rank, values.BundleDigest)
	r.setStringWithRank("nodeName", source, rank, values.NodeName)
	r.setStringWithRank("installMode", source, rank, values.InstallMode)
	r.setStringWithRank("artifactBaseURL", source, rank, values.ArtifactBaseURL)
	r.setBoolWithRank("holdForDebug", source, rank, values.HoldForDebug)
	r.setBoolWithRank("waitForConfig", source, rank, values.WaitForConfig)
}

func (r *inputResolver) setString(field string, source InputSource, value string) {
	r.setStringWithRank(field, source, sourceRanks[source], value)
}

func (r *inputResolver) setStringWithRank(field string, source InputSource, rank int, value string) {
	value = strings.TrimSpace(value)
	if value == "" || rank < r.rank[field] {
		return
	}
	if (field == "manifestPath" || field == "bundlePath") && rank == r.rank[field] {
		return
	}

	switch field {
	case "manifestPath":
		r.input.ManifestPath = value
	case "manifestURL":
		r.input.ManifestURL = value
	case "manifestSHA256":
		r.input.ManifestSHA256 = value
	case "bundlePath":
		r.input.BundlePath = value
	case "bundleURL":
		r.input.BundleURL = value
	case "bundleSHA256":
		r.input.BundleSHA256 = value
	case "bundleDigest":
		r.input.BundleDigest = value
	case "nodeName":
		r.input.NodeName = value
	case "installMode":
		r.input.InstallMode = value
	case "artifactBaseURL":
		r.input.ArtifactBaseURL = value
	default:
		return
	}
	r.recordSelection(field, source, rank)
}

func (r *inputResolver) setBoolWithRank(field string, source InputSource, rank int, value *bool) {
	if value == nil || rank < r.rank[field] {
		return
	}

	switch field {
	case "holdForDebug":
		r.input.HoldForDebug = *value
	case "waitForConfig":
		r.input.WaitForConfig = *value
	default:
		return
	}
	r.recordSelection(field, source, rank)
}

func (r *inputResolver) recordSelection(field string, source InputSource, rank int) {
	r.rank[field] = rank
	r.input.SelectedSources[field] = source
	r.input.Logs = append(r.input.Logs, fmt.Sprintf("selected %s from %s", field, source))
}

type bootInputValues struct {
	ManifestPath    string `json:"manifestPath"`
	ManifestURL     string `json:"manifestURL"`
	ManifestSHA256  string `json:"manifestSHA256"`
	BundlePath      string `json:"bundlePath"`
	BundleURL       string `json:"bundleURL"`
	BundleSHA256    string `json:"bundleSHA256"`
	BundleDigest    string `json:"bundleDigest"`
	NodeName        string `json:"nodeName"`
	InstallMode     string `json:"installMode"`
	ArtifactBaseURL string `json:"artifactBaseURL"`
	HoldForDebug    *bool  `json:"holdForDebug"`
	WaitForConfig   *bool  `json:"waitForConfig"`
}

var sourceRanks = map[InputSource]int{
	InputSourceManifest:      10,
	InputSourceLocalFile:     20,
	InputSourceEmbeddedMedia: 30,
	InputSourceEtcKatl:       40,
	InputSourceRunKatl:       50,
	InputSourceKernelCmdline: 60,
}

func parseKernelCmdline(cmdline string) (bootInputValues, error) {
	var values bootInputValues
	for _, token := range strings.Fields(cmdline) {
		key, value, hasValue := strings.Cut(token, "=")
		switch key {
		case "katl.manifest":
			if hasValue {
				values.ManifestPath = value
			}
		case "katl.manifest.url":
			if hasValue {
				values.ManifestURL = value
			}
		case "katl.manifest.sha256":
			if hasValue {
				values.ManifestSHA256 = value
			}
		case "katl.bundle":
			if hasValue {
				values.BundlePath = value
			}
		case "katl.bundle.url":
			if hasValue {
				values.BundleURL = value
			}
		case "katl.bundle.sha256":
			if hasValue {
				values.BundleSHA256 = value
			}
		case "katl.bundle.digest":
			if hasValue {
				values.BundleDigest = value
			}
		case "katl.node":
			if hasValue {
				values.NodeName = value
			}
		case "katl.install.mode":
			if hasValue {
				values.InstallMode = value
			}
		case "katl.artifact-base-url":
			if hasValue {
				values.ArtifactBaseURL = value
			}
		case "katl.hold-for-debug":
			parsed, err := parseKernelBool(hasValue, value)
			if err != nil {
				return bootInputValues{}, fmt.Errorf("parse %s: %w", key, err)
			}
			values.HoldForDebug = &parsed
		case "katl.wait-for-config":
			parsed, err := parseKernelBool(hasValue, value)
			if err != nil {
				return bootInputValues{}, fmt.Errorf("parse %s: %w", key, err)
			}
			values.WaitForConfig = &parsed
		}
	}
	return values, nil
}

func parseKernelBool(hasValue bool, value string) (bool, error) {
	if !hasValue {
		return true, nil
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("unsupported boolean value %q", value)
	}
}

func artifactBaseURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse artifact URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("artifact URL must be absolute")
	}
	lastSlash := strings.LastIndex(parsed.Path, "/")
	if lastSlash < 0 {
		parsed.Path = "/"
	} else {
		parsed.Path = parsed.Path[:lastSlash+1]
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
