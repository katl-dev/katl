package manifest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/disk"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "install.katl.dev/v1alpha1"
	Kind       = "InstallManifest"
)

var localRefRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+(/[A-Za-z0-9._+-]+)*$`)
var (
	labelDNSPattern       = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	labelNamePattern      = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	bootstrapTokenPattern = regexp.MustCompile(`\b[a-z0-9]{6}\.[a-z0-9]{16}\b`)
)

type Manifest struct {
	APIVersion  string        `json:"apiVersion" yaml:"apiVersion"`
	Kind        string        `json:"kind" yaml:"kind"`
	Node        NodeConfig    `json:"node" yaml:"node"`
	Install     InstallConfig `json:"install" yaml:"install"`
	KatlosImage KatlosImage   `json:"katlosImage" yaml:"katlosImage"`
}

type NodeConfig struct {
	Identity             NodeIdentity                 `json:"identity" yaml:"identity"`
	SystemRole           string                       `json:"systemRole" yaml:"systemRole"`
	Networkd             NetworkdConfig               `json:"networkd,omitempty" yaml:"networkd,omitempty"`
	HostConfiguration    HostConfiguration            `json:"hostConfiguration,omitempty,omitzero" yaml:"hostConfiguration,omitempty"`
	Kubernetes           KubernetesConfig             `json:"kubernetes,omitempty" yaml:"kubernetes,omitempty"`
	ControlPlaneEndpoint *controlplaneendpoint.Config `json:"controlPlaneEndpoint,omitempty" yaml:"controlPlaneEndpoint,omitempty"`
	Bootstrap            *BootstrapIntent             `json:"bootstrap,omitempty" yaml:"bootstrap,omitempty"`
}

type NodeIdentity struct {
	Hostname string      `json:"hostname" yaml:"hostname"`
	SSH      SSHIdentity `json:"ssh" yaml:"ssh"`
}

type SSHIdentity struct {
	AuthorizedKeys []string `json:"authorizedKeys" yaml:"authorizedKeys"`
}

type NetworkdConfig struct {
	Files []NetworkdFile `json:"files,omitempty" yaml:"files,omitempty"`
}

type NetworkdFile struct {
	Name    string `json:"name" yaml:"name"`
	Content string `json:"content" yaml:"content"`
}

const (
	HostConfigurationPresent = "present"
	HostConfigurationAbsent  = "absent"
)

type HostConfiguration struct {
	Sets map[string]HostConfigurationSet `json:"sets,omitempty" yaml:"sets,omitempty"`
}

func (config HostConfiguration) IsZero() bool {
	return len(config.Sets) == 0
}

type HostConfigurationSet struct {
	State  string                         `json:"state,omitempty" yaml:"state,omitempty"`
	Files  []HostConfigurationFile        `json:"files,omitempty" yaml:"files,omitempty"`
	Notify HostConfigurationNotifications `json:"notify,omitempty,omitzero" yaml:"notify,omitempty"`
}

type HostConfigurationFile struct {
	Path    string  `json:"path" yaml:"path"`
	Content *string `json:"content,omitempty" yaml:"content,omitempty"`
	Source  string  `json:"source,omitempty" yaml:"source,omitempty"`
	Mode    uint32  `json:"mode,omitempty" yaml:"mode,omitempty"`
}

type HostConfigurationNotifications struct {
	Systemd []HostConfigurationSystemdNotification `json:"systemd,omitempty" yaml:"systemd,omitempty"`
}

func (notifications HostConfigurationNotifications) IsZero() bool {
	return len(notifications.Systemd) == 0
}

type HostConfigurationSystemdNotification struct {
	Unit   string `json:"unit" yaml:"unit"`
	Action string `json:"action" yaml:"action"`
}

type KubernetesConfig struct {
	Kubeadm KubeadmReference `json:"kubeadm,omitempty" yaml:"kubeadm,omitempty"`
}

type KubeadmReference struct {
	ConfigRef string `json:"configRef,omitempty" yaml:"configRef,omitempty"`
}

type BootstrapIntent struct {
	ClusterName          string            `json:"clusterName,omitempty" yaml:"clusterName,omitempty"`
	InventoryNodeName    string            `json:"inventoryNodeName,omitempty" yaml:"inventoryNodeName,omitempty"`
	NodeAddress          string            `json:"nodeAddress,omitempty" yaml:"nodeAddress,omitempty"`
	ControlPlaneEndpoint string            `json:"controlPlaneEndpoint,omitempty" yaml:"controlPlaneEndpoint,omitempty"`
	BootstrapProfileRef  string            `json:"bootstrapProfileRef,omitempty" yaml:"bootstrapProfileRef,omitempty"`
	ProfileResolvedID    string            `json:"profileResolvedID,omitempty" yaml:"profileResolvedID,omitempty"`
	KubernetesCatalogRef string            `json:"kubernetesCatalogRef,omitempty" yaml:"kubernetesCatalogRef,omitempty"`
	KubernetesBundle     string            `json:"kubernetesBundle,omitempty" yaml:"kubernetesBundle,omitempty"`
	Access               BootstrapAccess   `json:"access,omitempty" yaml:"access,omitempty"`
	Labels               map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Taints               []NodeTaint       `json:"taints,omitempty" yaml:"taints,omitempty"`
}

type BootstrapAccess struct {
	Method        string `json:"method,omitempty" yaml:"method,omitempty"`
	User          string `json:"user,omitempty" yaml:"user,omitempty"`
	CredentialRef string `json:"credentialRef,omitempty" yaml:"credentialRef,omitempty"`
}

type NodeTaint struct {
	Key    string `json:"key" yaml:"key"`
	Value  string `json:"value,omitempty" yaml:"value,omitempty"`
	Effect string `json:"effect" yaml:"effect"`
}

type InstallConfig struct {
	WipeTarget bool         `json:"wipeTarget" yaml:"wipeTarget"`
	TargetDisk DiskSelector `json:"targetDisk" yaml:"targetDisk"`
	ExtraDisks []ExtraDisk  `json:"extraDisks,omitempty" yaml:"extraDisks,omitempty"`
}

type DiskSelector struct {
	ByID       string `json:"byID,omitempty" yaml:"byID,omitempty"`
	WWN        string `json:"wwn,omitempty" yaml:"wwn,omitempty"`
	Serial     string `json:"serial,omitempty" yaml:"serial,omitempty"`
	MinSizeMiB uint64 `json:"minSizeMiB,omitempty" yaml:"minSizeMiB,omitempty"`
}

type ExtraDisk struct {
	Name       string       `json:"name" yaml:"name"`
	Selector   DiskSelector `json:"selector" yaml:"selector"`
	Filesystem string       `json:"filesystem" yaml:"filesystem"`
	Mount      ExtraMount   `json:"mount" yaml:"mount"`
	Wipe       bool         `json:"wipe,omitempty" yaml:"wipe,omitempty"`
}

type ExtraMount struct {
	Path string `json:"path" yaml:"path"`
}

type KatlosImage struct {
	URL              string `json:"url,omitempty" yaml:"url,omitempty"`
	LocalRef         string `json:"localRef,omitempty" yaml:"localRef,omitempty"`
	SHA256           string `json:"sha256" yaml:"sha256"`
	SizeBytes        uint64 `json:"sizeBytes" yaml:"sizeBytes"`
	Version          string `json:"version" yaml:"version"`
	Architecture     string `json:"architecture" yaml:"architecture"`
	RuntimeInterface string `json:"runtimeInterface,omitempty" yaml:"runtimeInterface,omitempty"`
	Role             string `json:"role" yaml:"role"`
}

type RootDiskProfile struct {
	ESPSizeMiB      uint64
	XBOOTLDRSizeMiB uint64
	RootSlotSizeMiB uint64
	StateFilesystem string
	StateMinSizeMiB uint64
	InitialRootSlot disk.RootSlot
}

func Decode(reader io.Reader) (Manifest, error) {
	manifest, _, err := DecodeWithDefaultImage(reader, KatlosImage{})
	return manifest, err
}

func DecodeWithDefaultImage(reader io.Reader, defaultImage KatlosImage) (Manifest, bool, error) {
	return DecodeWithOptions(reader, DecodeOptions{DefaultKatlosImage: defaultImage})
}

type DecodeOptions struct {
	DefaultKatlosImage      KatlosImage
	AllowMissingKatlosImage bool
}

func DecodeWithOptions(reader io.Reader, options DecodeOptions) (Manifest, bool, error) {
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)

	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, false, fmt.Errorf("decode install manifest: %w", normalizeDecodeError(err))
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Manifest{}, false, fmt.Errorf("decode install manifest: multiple YAML documents")
		}
		return Manifest{}, false, fmt.Errorf("decode install manifest: %w", normalizeDecodeError(err))
	}
	if manifest.APIVersion != APIVersion {
		return Manifest{}, false, fmt.Errorf("apiVersion must be %s", APIVersion)
	}
	if manifest.Kind != Kind {
		return Manifest{}, false, fmt.Errorf("kind must be %s", Kind)
	}
	defaulted := false
	if KatlosImageEmpty(manifest.KatlosImage) && !KatlosImageEmpty(options.DefaultKatlosImage) {
		manifest.KatlosImage = options.DefaultKatlosImage
		defaulted = true
	}
	if err := ValidateWithOptions(manifest, ValidateOptions{AllowMissingKatlosImage: options.AllowMissingKatlosImage}); err != nil {
		return Manifest{}, false, err
	}
	return manifest, defaulted, nil
}

func KatlosImageEmpty(image KatlosImage) bool {
	return image == (KatlosImage{})
}

func normalizeDecodeError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "field ") && strings.Contains(err.Error(), " not found in type ") {
		return fmt.Errorf("unknown field: %w", err)
	}
	return err
}

func Validate(manifest Manifest) error {
	return ValidateWithOptions(manifest, ValidateOptions{})
}

type ValidateOptions struct {
	AllowMissingKatlosImage bool
}

func ValidateWithOptions(manifest Manifest, options ValidateOptions) error {
	if !manifest.Install.WipeTarget {
		return fmt.Errorf("install.wipeTarget must be true")
	}
	if strings.TrimSpace(manifest.Node.Identity.Hostname) == "" {
		return fmt.Errorf("node.identity.hostname is required")
	}
	if !ValidHostname(manifest.Node.Identity.Hostname) {
		return fmt.Errorf("node.identity.hostname %q is invalid", manifest.Node.Identity.Hostname)
	}
	if err := validateSystemRole(manifest.Node.SystemRole); err != nil {
		return err
	}
	if endpoint := manifest.Node.ControlPlaneEndpoint; endpoint != nil {
		plan, err := controlplaneendpoint.Normalize(*endpoint)
		if err != nil {
			return fmt.Errorf("node.controlPlaneEndpoint: %w", err)
		}
		if plan.Config.Advertisement == nil {
			return fmt.Errorf("node.controlPlaneEndpoint must contain managed advertisement intent")
		}
		if manifest.Node.SystemRole != "control-plane" {
			return fmt.Errorf("node.controlPlaneEndpoint is supported only on control-plane nodes")
		}
	}
	if len(manifest.Node.Identity.SSH.AuthorizedKeys) == 0 {
		return fmt.Errorf("node.identity.ssh.authorizedKeys must not be empty")
	}
	for i, key := range manifest.Node.Identity.SSH.AuthorizedKeys {
		if !ValidAuthorizedKey(key) {
			return fmt.Errorf("node.identity.ssh.authorizedKeys[%d] must be an SSH public key", i)
		}
	}
	if err := validateNetworkd(manifest.Node.Networkd); err != nil {
		return err
	}
	if err := ValidateHostConfiguration(manifest.Node.HostConfiguration, false); err != nil {
		return fmt.Errorf("node.hostConfiguration: %w", err)
	}
	if err := validateNameRef("node.kubernetes.kubeadm.configRef", manifest.Node.Kubernetes.Kubeadm.ConfigRef); err != nil {
		return err
	}
	if manifest.Node.Bootstrap != nil {
		if err := validateBootstrapIntent(*manifest.Node.Bootstrap); err != nil {
			return err
		}
	}
	if err := validateDiskSelector("install.targetDisk", manifest.Install.TargetDisk); err != nil {
		return err
	}
	if !(options.AllowMissingKatlosImage && KatlosImageEmpty(manifest.KatlosImage)) {
		if err := validateKatlosImage(manifest.KatlosImage); err != nil {
			return err
		}
	}
	for i, extra := range manifest.Install.ExtraDisks {
		if strings.TrimSpace(extra.Name) == "" {
			return fmt.Errorf("install.extraDisks[%d].name is required", i)
		}
		if err := validateDiskSelector(fmt.Sprintf("install.extraDisks[%d].selector", i), extra.Selector); err != nil {
			return err
		}
		if strings.TrimSpace(extra.Filesystem) == "" {
			return fmt.Errorf("install.extraDisks[%d].filesystem is required", i)
		}
		if strings.TrimSpace(extra.Mount.Path) == "" {
			return fmt.Errorf("install.extraDisks[%d].mount.path is required", i)
		}
	}
	return nil
}

func validateBootstrapIntent(intent BootstrapIntent) error {
	for field, value := range map[string]string{
		"node.bootstrap.clusterName":          intent.ClusterName,
		"node.bootstrap.inventoryNodeName":    intent.InventoryNodeName,
		"node.bootstrap.nodeAddress":          intent.NodeAddress,
		"node.bootstrap.controlPlaneEndpoint": intent.ControlPlaneEndpoint,
		"node.bootstrap.bootstrapProfileRef":  intent.BootstrapProfileRef,
		"node.bootstrap.profileResolvedID":    intent.ProfileResolvedID,
		"node.bootstrap.kubernetesCatalogRef": intent.KubernetesCatalogRef,
		"node.bootstrap.kubernetesBundle":     intent.KubernetesBundle,
		"node.bootstrap.access.method":        intent.Access.Method,
		"node.bootstrap.access.user":          intent.Access.User,
		"node.bootstrap.access.credentialRef": intent.Access.CredentialRef,
	} {
		if strings.TrimSpace(value) != value {
			return fmt.Errorf("%s %q must not contain leading or trailing whitespace", field, value)
		}
	}
	if err := validateBootstrapAccess(intent.Access); err != nil {
		return err
	}
	for key, value := range intent.Labels {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("node.bootstrap.labels contains an empty key")
		}
		if strings.TrimSpace(key) != key {
			return fmt.Errorf("node.bootstrap.labels key %q must not contain leading or trailing whitespace", key)
		}
		if !validLabelKey(key) {
			return fmt.Errorf("node.bootstrap.labels key %q is invalid", key)
		}
		if strings.TrimSpace(value) != value {
			return fmt.Errorf("node.bootstrap.labels[%q] value must not contain leading or trailing whitespace", key)
		}
		if value != "" && !validLabelName(value) {
			return fmt.Errorf("node.bootstrap.labels[%q] value %q is invalid", key, value)
		}
	}
	for i, taint := range intent.Taints {
		field := fmt.Sprintf("node.bootstrap.taints[%d]", i)
		if strings.TrimSpace(taint.Key) == "" {
			return fmt.Errorf("%s.key is required", field)
		}
		if strings.TrimSpace(taint.Key) != taint.Key {
			return fmt.Errorf("%s.key %q must not contain leading or trailing whitespace", field, taint.Key)
		}
		if !validLabelKey(taint.Key) {
			return fmt.Errorf("%s.key %q is invalid", field, taint.Key)
		}
		if strings.TrimSpace(taint.Value) != taint.Value {
			return fmt.Errorf("%s.value must not contain leading or trailing whitespace", field)
		}
		if taint.Value != "" && !validLabelName(taint.Value) {
			return fmt.Errorf("%s.value %q is invalid", field, taint.Value)
		}
		switch taint.Effect {
		case "NoSchedule", "PreferNoSchedule", "NoExecute":
		default:
			return fmt.Errorf("%s.effect %q is unsupported", field, taint.Effect)
		}
	}
	return nil
}

func validateBootstrapAccess(access BootstrapAccess) error {
	if access.Method == "" && access.User == "" && access.CredentialRef == "" {
		return nil
	}
	switch access.Method {
	case "ssh", "vsock", "agent":
	case "":
		return fmt.Errorf("node.bootstrap.access.method is required")
	default:
		return fmt.Errorf("node.bootstrap.access.method %q is unsupported", access.Method)
	}
	if access.Method != "agent" && access.CredentialRef == "" {
		return fmt.Errorf("node.bootstrap.access.credentialRef is required")
	}
	if strings.ContainsAny(access.CredentialRef, "\n\r") {
		return fmt.Errorf("node.bootstrap.access.credentialRef must be a single line")
	}
	if strings.Contains(access.CredentialRef, "-----BEGIN ") || bootstrapTokenPattern.MatchString(access.CredentialRef) {
		return fmt.Errorf("node.bootstrap.access.credentialRef must reference credentials, not inline secret material")
	}
	return nil
}

func validLabelKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	prefix, name, hasPrefix := strings.Cut(key, "/")
	if hasPrefix {
		if !validDNSSubdomain(prefix) {
			return false
		}
		key = name
	}
	return validLabelName(key)
}

func validDNSSubdomain(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if !validDNSLabel(part) {
			return false
		}
	}
	return true
}

func validDNSLabel(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	return labelDNSPattern.MatchString(value)
}

func ValidHostname(value string) bool {
	return validDNSLabel(strings.TrimSpace(value))
}

func ValidAuthorizedKey(key string) bool {
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return false
	}
	keyType := fields[0]
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return false
	}
	return validAuthorizedKeyBlob(keyType, blob)
}

func validAuthorizedKeyBlob(keyType string, blob []byte) bool {
	embedded, rest, ok := sshWireString(blob)
	if !ok || embedded != keyType {
		return false
	}
	switch keyType {
	case "ssh-ed25519":
		key, rest, ok := sshWireString(rest)
		return ok && len(key) == 32 && len(rest) == 0
	default:
		return false
	}
}

func sshWireString(blob []byte) (string, []byte, bool) {
	data, rest, ok := sshWireBytes(blob)
	if !ok {
		return "", nil, false
	}
	return string(data), rest, true
}

func sshWireBytes(blob []byte) ([]byte, []byte, bool) {
	if len(blob) < 4 {
		return nil, nil, false
	}
	size := binary.BigEndian.Uint32(blob[:4])
	if size > uint32(len(blob)-4) {
		return nil, nil, false
	}
	end := 4 + int(size)
	return blob[4:end], blob[end:], true
}

func validLabelName(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	return labelNamePattern.MatchString(value)
}

func validateSystemRole(value string) error {
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("node.systemRole %q must not contain leading or trailing whitespace", value)
	}
	switch value {
	case "":
		return fmt.Errorf("node.systemRole is required")
	case "control-plane", "worker":
		return nil
	default:
		return fmt.Errorf("node.systemRole %q is unsupported", value)
	}
}

func validateNetworkd(config NetworkdConfig) error {
	seen := make(map[string]struct{}, len(config.Files))
	for i, file := range config.Files {
		field := fmt.Sprintf("node.networkd.files[%d]", i)
		if strings.TrimSpace(file.Name) == "" {
			return fmt.Errorf("%s.name is required", field)
		}
		if file.Name != filepath.Base(file.Name) || file.Name == "." || file.Name == ".." {
			return fmt.Errorf("%s.name %q must be a single path segment", field, file.Name)
		}
		ext := filepath.Ext(file.Name)
		if ext != ".network" && ext != ".netdev" && ext != ".link" {
			return fmt.Errorf("%s.name %q must end with .network, .netdev, or .link", field, file.Name)
		}
		for _, r := range file.Name {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._@+-", r)
			if !ok {
				return fmt.Errorf("%s.name %q contains unsupported character %q", field, file.Name, r)
			}
		}
		if _, ok := seen[file.Name]; ok {
			return fmt.Errorf("%s.name %q duplicates another networkd file", field, file.Name)
		}
		seen[file.Name] = struct{}{}
		if strings.TrimSpace(file.Content) == "" {
			return fmt.Errorf("%s.content is required", field)
		}
	}
	return nil
}

const (
	MaxHostConfigurationFileBytes  = 1 << 20
	MaxHostConfigurationTotalBytes = 4 << 20
)

var protectedHostConfigurationPaths = []string{
	"/etc/extension-release.d",
	"/etc/katl",
	"/etc/kubernetes",
	"/etc/pam.d",
	"/etc/security",
	"/etc/ssh/authorized_keys",
	"/etc/ssh/sshd_config.d",
	"/etc/sudoers.d",
	"/etc/sysusers.d",
}

var protectedHostConfigurationExactPaths = map[string]struct{}{
	"/etc/fstab":           {},
	"/etc/group":           {},
	"/etc/gshadow":         {},
	"/etc/hostname":        {},
	"/etc/passwd":          {},
	"/etc/shadow":          {},
	"/etc/ssh/sshd_config": {},
	"/etc/subgid":          {},
	"/etc/subuid":          {},
	"/etc/sudoers":         {},
}

// ValidateHostConfiguration validates the native-file ownership boundary.
// allowSource is used only while compiling ClusterConfig; installed manifests
// must contain embedded content and never retain authoring-time file paths.
func ValidateHostConfiguration(config HostConfiguration, allowSource bool) error {
	setNames := make([]string, 0, len(config.Sets))
	for name := range config.Sets {
		setNames = append(setNames, name)
	}
	sort.Strings(setNames)
	paths := make(map[string]string)
	systemdNotifications := make(map[string]string)
	totalBytes := 0
	for _, name := range setNames {
		if err := validateHostConfigurationSetName(name); err != nil {
			return err
		}
		set := config.Sets[name]
		state := strings.TrimSpace(set.State)
		if state == "" {
			state = HostConfigurationPresent
		}
		switch state {
		case HostConfigurationPresent:
			if len(set.Files) == 0 {
				return fmt.Errorf("sets[%q].files must not be empty", name)
			}
		case HostConfigurationAbsent:
			if len(set.Files) != 0 || len(set.Notify.Systemd) != 0 {
				return fmt.Errorf("sets[%q] with state absent must not declare files or notifications", name)
			}
			continue
		default:
			return fmt.Errorf("sets[%q].state %q is unsupported", name, set.State)
		}
		for i, file := range set.Files {
			field := fmt.Sprintf("sets[%q].files[%d]", name, i)
			cleaned, err := validateHostConfigurationPath(file.Path)
			if err != nil {
				return fmt.Errorf("%s.path: %w", field, err)
			}
			if owner, exists := paths[cleaned]; exists {
				return fmt.Errorf("%s.path %q is already owned by set %q", field, cleaned, owner)
			}
			paths[cleaned] = name
			hasContent := file.Content != nil
			hasSource := strings.TrimSpace(file.Source) != ""
			if hasContent == hasSource {
				return fmt.Errorf("%s must declare exactly one of content or source", field)
			}
			if hasSource && !allowSource {
				return fmt.Errorf("%s.source must be resolved before installation", field)
			}
			if hasContent {
				if !utf8.ValidString(*file.Content) {
					return fmt.Errorf("%s.content must be valid UTF-8", field)
				}
				if strings.ContainsRune(*file.Content, '\x00') {
					return fmt.Errorf("%s.content must not contain NUL bytes", field)
				}
				if len(*file.Content) > MaxHostConfigurationFileBytes {
					return fmt.Errorf("%s.content exceeds %d bytes", field, MaxHostConfigurationFileBytes)
				}
				totalBytes += len(*file.Content)
			}
			switch file.Mode {
			case 0, 0o600, 0o640, 0o644:
			default:
				return fmt.Errorf("%s.mode %#o is unsupported; use 0600, 0640, or 0644", field, file.Mode)
			}
		}
		if err := validateHostConfigurationNotifications(name, set.Notify); err != nil {
			return err
		}
		for _, notification := range set.Notify.Systemd {
			if action, exists := systemdNotifications[notification.Unit]; exists && action != notification.Action {
				return fmt.Errorf("sets[%q].notify.systemd conflicts with action %q declared by another set for %q", name, action, notification.Unit)
			}
			systemdNotifications[notification.Unit] = notification.Action
		}
	}
	if totalBytes > MaxHostConfigurationTotalBytes {
		return fmt.Errorf("file content exceeds the %d-byte host configuration limit", MaxHostConfigurationTotalBytes)
	}
	return nil
}

func validateHostConfigurationSetName(name string) error {
	if name == "" || name != strings.TrimSpace(name) {
		return fmt.Errorf("host configuration set name %q must not be empty or contain surrounding whitespace", name)
	}
	if len(name) > 63 || !labelDNSPattern.MatchString(name) {
		return fmt.Errorf("host configuration set name %q must be a lowercase DNS label of 63 characters or fewer", name)
	}
	return nil
}

func validateHostConfigurationPath(value string) (string, error) {
	if strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("must not contain NUL bytes")
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%q must be absolute", value)
	}
	cleaned := path.Clean(value)
	if value != cleaned {
		return "", fmt.Errorf("%q must be normalized as %q", value, cleaned)
	}
	if cleaned == "/etc" || !strings.HasPrefix(cleaned, "/etc/") {
		return "", fmt.Errorf("%q must name a regular file below /etc", value)
	}
	if _, protected := protectedHostConfigurationExactPaths[cleaned]; protected {
		return "", fmt.Errorf("%q is owned by KatlOS", value)
	}
	for _, prefix := range protectedHostConfigurationPaths {
		if cleaned == prefix || strings.HasPrefix(cleaned, prefix+"/") {
			return "", fmt.Errorf("%q is below KatlOS-owned prefix %q", value, prefix)
		}
	}
	if protectedSystemdPath(cleaned) {
		return "", fmt.Errorf("%q is protected systemd configuration", value)
	}
	return cleaned, nil
}

func protectedSystemdPath(value string) bool {
	const prefix = "/etc/systemd/system/"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	relative := strings.TrimPrefix(value, prefix)
	unit := strings.Split(relative, "/")[0]
	if strings.HasSuffix(unit, ".wants") || strings.HasSuffix(unit, ".requires") {
		return true
	}
	unit = strings.TrimSuffix(unit, ".d")
	return protectedSystemdUnit(unit)
}

func protectedSystemdUnit(unit string) bool {
	if strings.HasPrefix(unit, "katl") {
		return true
	}
	switch unit {
	case "containerd.service", "efi.mount", "etc-kubernetes.mount", "kubelet.service",
		"sshd.service", "systemd-boot-check-no-failures.service",
		"systemd-boot-system-token.service", "systemd-confext.service",
		"systemd-hostnamed.service", "systemd-networkd.service",
		"systemd-resolved.service", "systemd-sysext.service", "var.mount":
		return true
	default:
		return false
	}
}

func validateHostConfigurationNotifications(setName string, notifications HostConfigurationNotifications) error {
	seen := make(map[string]string)
	for i, notification := range notifications.Systemd {
		field := fmt.Sprintf("sets[%q].notify.systemd[%d]", setName, i)
		unit := strings.TrimSpace(notification.Unit)
		if unit == "" || len(unit) > 255 || unit != notification.Unit || !systemdNotificationUnitPattern.MatchString(unit) {
			return fmt.Errorf("%s.unit %q must be a single systemd unit name", field, notification.Unit)
		}
		if protectedSystemdUnit(unit) {
			return fmt.Errorf("%s.unit %q is release-critical and cannot be notified", field, unit)
		}
		switch notification.Action {
		case "reload", "try-reload-or-restart", "try-restart":
		default:
			return fmt.Errorf("%s.action %q is unsupported", field, notification.Action)
		}
		if action, exists := seen[unit]; exists && action != notification.Action {
			return fmt.Errorf("%s conflicts with action %q already declared for %q", field, action, unit)
		}
		seen[unit] = notification.Action
	}
	return nil
}

var systemdNotificationUnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.(?:service|socket|target|timer|path|mount|automount|slice|scope|device|swap)$`)

func validateNameRef(field string, value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s %q must not contain leading or trailing whitespace", field, value)
	}
	if value != filepath.Base(value) || value == "." || value == ".." {
		return fmt.Errorf("%s %q must be a single path segment", field, value)
	}
	if len(value) > 63 {
		return fmt.Errorf("%s %q must be 63 characters or fewer", field, value)
	}
	if !isLowercaseLetterOrDigit(rune(value[0])) || !isLowercaseLetterOrDigit(rune(value[len(value)-1])) {
		return fmt.Errorf("%s %q must start and end with a lowercase letter or digit", field, value)
	}
	for _, r := range value {
		ok := isLowercaseLetterOrDigit(r) || r == '-'
		if !ok {
			return fmt.Errorf("%s %q must contain only lowercase letters, digits, and dashes", field, value)
		}
	}
	return nil
}

func isLowercaseLetterOrDigit(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

func validateDiskSelector(field string, selector DiskSelector) error {
	selected := 0
	for _, value := range []string{selector.ByID, selector.WWN, selector.Serial} {
		if strings.TrimSpace(value) != "" {
			selected++
		}
	}
	if selected != 1 {
		return fmt.Errorf("%s must set exactly one of byID, wwn, or serial", field)
	}
	return nil
}

func validateKatlosImage(image KatlosImage) error {
	const field = "katlosImage"
	urlValue := strings.TrimSpace(image.URL)
	localRef := strings.TrimSpace(image.LocalRef)
	if image.URL != urlValue {
		return fmt.Errorf("%s.url must not contain leading or trailing whitespace", field)
	}
	if image.LocalRef != localRef {
		return fmt.Errorf("%s.localRef must not contain leading or trailing whitespace", field)
	}
	switch {
	case urlValue == "" && localRef == "":
		return fmt.Errorf("%s must set url or localRef", field)
	case urlValue != "" && localRef != "":
		return fmt.Errorf("%s must not set both url and localRef", field)
	}
	if urlValue != "" {
		parsed, err := url.Parse(urlValue)
		if err != nil {
			return fmt.Errorf("%s.url is invalid: %w", field, err)
		}
		if parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("%s.url must be absolute", field)
		}
	}
	if localRef != "" {
		if filepath.IsAbs(localRef) || filepath.Clean(localRef) != localRef || !localRefRE.MatchString(localRef) {
			return fmt.Errorf("%s.localRef %q must be a clean relative path", field, localRef)
		}
		for _, segment := range strings.Split(localRef, "/") {
			if segment == "." || segment == ".." {
				return fmt.Errorf("%s.localRef %q must not contain dot segments", field, localRef)
			}
		}
	}
	if strings.TrimSpace(image.SHA256) == "" {
		return fmt.Errorf("%s.sha256 is required", field)
	}
	if err := validateSHA256(image.SHA256); err != nil {
		return fmt.Errorf("%s.sha256 is invalid: %w", field, err)
	}
	if image.SizeBytes == 0 {
		return fmt.Errorf("%s.sizeBytes is required", field)
	}
	if strings.TrimSpace(image.Version) == "" {
		return fmt.Errorf("%s.version is required", field)
	}
	if strings.TrimSpace(image.Architecture) == "" {
		return fmt.Errorf("%s.architecture is required", field)
	}
	if image.Role != "install" {
		return fmt.Errorf("%s.role must be install", field)
	}
	return nil
}

func validateSHA256(value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("must be %d lowercase hex characters", sha256.Size*2)
	}
	if value != strings.ToLower(value) {
		return fmt.Errorf("must be lowercase hex")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return err
	}
	return nil
}

func DefaultRootDiskProfile() RootDiskProfile {
	return RootDiskProfile{
		ESPSizeMiB:      disk.DefaultESPSizeMiB,
		RootSlotSizeMiB: 8192,
		StateFilesystem: "ext4",
		StateMinSizeMiB: 8192,
		InitialRootSlot: disk.RootSlotA,
	}
}

func BuildDiskLayoutRequest(manifest Manifest, profile RootDiskProfile, runtimeRootSizeMiB uint64) (disk.DiskLayoutRequest, error) {
	if profile.ESPSizeMiB == 0 {
		profile.ESPSizeMiB = disk.DefaultESPSizeMiB
	}
	if profile.RootSlotSizeMiB == 0 {
		profile.RootSlotSizeMiB = 8192
	}
	if profile.StateFilesystem == "" {
		profile.StateFilesystem = "ext4"
	}
	if profile.StateMinSizeMiB == 0 {
		profile.StateMinSizeMiB = 8192
	}
	if profile.InitialRootSlot == "" {
		profile.InitialRootSlot = disk.RootSlotA
	}

	extraDisks := make([]disk.ExtraDiskRequest, 0, len(manifest.Install.ExtraDisks))
	for _, extra := range manifest.Install.ExtraDisks {
		extraDisks = append(extraDisks, disk.ExtraDiskRequest{
			Name:       extra.Name,
			Selector:   diskSelector(extra.Selector),
			Filesystem: extra.Filesystem,
			MountPath:  extra.Mount.Path,
			Wipe:       extra.Wipe,
		})
	}

	return disk.DiskLayoutRequest{
		TargetDisk:         diskSelector(manifest.Install.TargetDisk),
		ESPSizeMiB:         profile.ESPSizeMiB,
		XBOOTLDRSizeMiB:    profile.XBOOTLDRSizeMiB,
		RootA:              disk.RootSlotRequest{SizeMiB: profile.RootSlotSizeMiB},
		RootB:              disk.RootSlotRequest{SizeMiB: profile.RootSlotSizeMiB},
		State:              disk.StatePartitionRequest{Filesystem: profile.StateFilesystem, MinSizeMiB: profile.StateMinSizeMiB},
		ExtraDisks:         extraDisks,
		InitialRootSlot:    profile.InitialRootSlot,
		RuntimeRootSizeMiB: runtimeRootSizeMiB,
	}, nil
}

func diskSelector(selector DiskSelector) disk.TargetDiskSelector {
	return disk.TargetDiskSelector{
		ByID:       selector.ByID,
		WWN:        selector.WWN,
		Serial:     selector.Serial,
		MinSizeMiB: selector.MinSizeMiB,
	}
}
