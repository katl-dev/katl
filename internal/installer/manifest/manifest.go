package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zariel/katl/internal/installer/disk"
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
	Identity   NodeIdentity     `json:"identity" yaml:"identity"`
	SystemRole string           `json:"systemRole" yaml:"systemRole"`
	Networkd   NetworkdConfig   `json:"networkd,omitempty" yaml:"networkd,omitempty"`
	Kubernetes KubernetesConfig `json:"kubernetes,omitempty" yaml:"kubernetes,omitempty"`
	Bootstrap  *BootstrapIntent `json:"bootstrap,omitempty" yaml:"bootstrap,omitempty"`
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
	AllowDestructiveInstall bool         `json:"allowDestructiveInstall" yaml:"allowDestructiveInstall"`
	TargetDisk              DiskSelector `json:"targetDisk" yaml:"targetDisk"`
	ExtraDisks              []ExtraDisk  `json:"extraDisks,omitempty" yaml:"extraDisks,omitempty"`
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
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()

	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode install manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Manifest{}, fmt.Errorf("decode install manifest: multiple JSON values")
	}
	if manifest.APIVersion != APIVersion {
		return Manifest{}, fmt.Errorf("apiVersion must be %s", APIVersion)
	}
	if manifest.Kind != Kind {
		return Manifest{}, fmt.Errorf("kind must be %s", Kind)
	}
	if err := Validate(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func Validate(manifest Manifest) error {
	if !manifest.Install.AllowDestructiveInstall {
		return fmt.Errorf("install.allowDestructiveInstall must be true")
	}
	if strings.TrimSpace(manifest.Node.Identity.Hostname) == "" {
		return fmt.Errorf("node.identity.hostname is required")
	}
	if err := validateSystemRole(manifest.Node.SystemRole); err != nil {
		return err
	}
	if len(manifest.Node.Identity.SSH.AuthorizedKeys) == 0 {
		return fmt.Errorf("node.identity.ssh.authorizedKeys must not be empty")
	}
	if err := validateNetworkd(manifest.Node.Networkd); err != nil {
		return err
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
	if err := validateKatlosImage(manifest.KatlosImage); err != nil {
		return err
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
	if access.CredentialRef == "" {
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
