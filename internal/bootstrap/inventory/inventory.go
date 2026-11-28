package inventory

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig/inputpath"
	"github.com/katl-dev/katl/internal/redact"
)

type SystemRole string

const (
	RoleControlPlane SystemRole = "control-plane"
	RoleWorker       SystemRole = "worker"
)

type BootstrapAction string

const (
	ActionInit             BootstrapAction = "kubeadm-init"
	ActionWorkerJoin       BootstrapAction = "kubeadm-worker-join"
	ActionControlPlaneJoin BootstrapAction = "kubeadm-control-plane-join"
)

type KubeadmIntent string

const (
	IntentControlPlane KubeadmIntent = "control-plane"
	IntentWorker       KubeadmIntent = "worker"
)

type Inventory struct {
	ExternalControlPlane        bool       `json:"externalControlPlane,omitempty"`
	ControlPlaneEndpoint        HostPort   `json:"controlPlaneEndpoint" yaml:"controlPlaneEndpoint"`
	ControlPlaneEndpointManaged bool       `json:"controlPlaneEndpointManaged,omitempty"`
	ControlPlaneEndpointDerived bool       `json:"controlPlaneEndpointDerived,omitempty" yaml:"controlPlaneEndpointDerived,omitempty"`
	KubernetesVersion           string     `json:"kubernetesVersion"`
	KubernetesBundleSource      string     `json:"kubernetesBundleSource,omitempty"`
	KubernetesBundleRef         string     `json:"kubernetesBundleRef,omitempty"`
	Bootstrap                   *Bootstrap `json:"bootstrap,omitempty" yaml:"bootstrap"`
	Nodes                       []Node     `json:"nodes"`
}

type Bootstrap struct {
	Manifests                     []BootstrapManifest `json:"manifests,omitempty" yaml:"manifests"`
	Waits                         []BootstrapWait     `json:"waits,omitempty" yaml:"waits"`
	StableEndpoint                string              `json:"stableEndpoint,omitempty" yaml:"stableEndpoint"`
	StableEndpointBeforeManifests bool                `json:"stableEndpointBeforeManifests,omitempty" yaml:"stableEndpointBeforeManifests"`
}

type BootstrapManifest struct {
	Path string `json:"path" yaml:"path"`
}

type BootstrapWait struct {
	Kind      string `json:"kind" yaml:"kind"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace"`
	Name      string `json:"name,omitempty" yaml:"name"`
	Condition string `json:"condition,omitempty" yaml:"condition"`
	Selector  string `json:"selector,omitempty" yaml:"selector"`
}

// Node is one inventory member. Address is where the workstation reaches the
// node and may be translated by NAT. KubernetesAddress, when set, is assigned
// to the node itself; see kubernetesHost for which one node-side consumers use.
type Node struct {
	Name              string            `json:"name"`
	Address           ManagementAddress `json:"address,omitzero"`
	KubernetesAddress netip.Addr        `json:"kubernetesAddress,omitzero" yaml:"kubernetesAddress"`
	SystemRole        SystemRole        `json:"systemRole"`
	Access            Access            `json:"access"`
	KubeadmConfig     KubeadmConfig     `json:"kubeadmConfig"`
	KubernetesVersion string            `json:"kubernetesVersion"`
	EnrollmentID      string            `json:"enrollmentID,omitempty" yaml:"enrollmentID,omitempty"`
	MachineID         string            `json:"machineID,omitempty" yaml:"machineID,omitempty"`
}

type Access struct {
	Method        string `json:"method" yaml:"method"`
	User          string `json:"user,omitempty" yaml:"user"`
	CredentialRef string `json:"credentialRef,omitempty" yaml:"credentialRef,omitempty"`
}

type KubeadmConfig struct {
	Ref              string        `json:"ref"`
	Path             string        `json:"path"`
	Intent           KubeadmIntent `json:"intent"`
	NodeLocalKubelet bool          `json:"nodeLocalKubelet,omitempty"`
}

type PlanRequest struct {
	Inventory       Inventory
	InitNode        string
	AddressOverride map[string]ManagementAddress
}

type Plan struct {
	ExternalControlPlane        bool              `json:"externalControlPlane,omitempty"`
	InitNode                    string            `json:"initNode"`
	ControlPlaneEndpoint        HostPort          `json:"controlPlaneEndpoint"`
	ControlPlaneEndpointManaged bool              `json:"controlPlaneEndpointManaged,omitempty"`
	ControlPlaneEndpointDerived bool              `json:"controlPlaneEndpointDerived,omitempty" yaml:"controlPlaneEndpointDerived,omitempty"`
	KubernetesVersion           string            `json:"kubernetesVersion"`
	KubernetesBundleSource      string            `json:"kubernetesBundleSource,omitempty"`
	KubernetesBundleRef         string            `json:"kubernetesBundleRef,omitempty"`
	Bootstrap                   *Bootstrap        `json:"bootstrap,omitempty"`
	Nodes                       []PlannedNode     `json:"nodes"`
	AddressOverrides            []AddressOverride `json:"addressOverrides,omitempty"`
}

type PlannedNode struct {
	Name              string            `json:"name"`
	Address           ManagementAddress `json:"address"`
	KubernetesAddress netip.Addr        `json:"kubernetesAddress,omitzero"`
	SystemRole        SystemRole        `json:"systemRole"`
	Action            BootstrapAction   `json:"action"`
	Access            Access            `json:"access"`
	KubeadmConfig     KubeadmConfig     `json:"kubeadmConfig"`
	KubernetesVersion string            `json:"kubernetesVersion"`
	EnrollmentID      string            `json:"enrollmentID,omitempty" yaml:"enrollmentID,omitempty"`
	MachineID         string            `json:"machineID,omitempty" yaml:"machineID,omitempty"`
}

type AddressOverride struct {
	Node    string            `json:"node"`
	Before  ManagementAddress `json:"before,omitzero"`
	Address ManagementAddress `json:"address"`
}

// ValidateInventory validates durable cluster inventory without requiring an
// operator-scoped kubeadm init choice. The first declared control-plane node is
// used only to exercise the planner's complete validation path; the resulting
// action ordering is discarded.
func ValidateInventory(inv Inventory) error {
	initNode := ""
	for _, node := range inv.Nodes {
		if SystemRole(strings.TrimSpace(string(node.SystemRole))) == RoleControlPlane {
			initNode = strings.TrimSpace(node.Name)
			break
		}
	}
	_, err := PlanInventory(PlanRequest{Inventory: inv, InitNode: initNode})
	return err
}

func PlanInventory(request PlanRequest) (Plan, error) {
	if len(request.Inventory.Nodes) == 0 {
		return Plan{}, fmt.Errorf("inventory must contain at least one node")
	}
	nodes := make([]PlannedNode, 0, len(request.Inventory.Nodes))
	seen := make(map[string]struct{}, len(request.Inventory.Nodes))
	originalAddress := make(map[string]ManagementAddress, len(request.Inventory.Nodes))
	var controlPlanes []string
	version := strings.TrimSpace(request.Inventory.KubernetesVersion)
	for _, node := range request.Inventory.Nodes {
		name := strings.TrimSpace(node.Name)
		if err := validateName(name); err != nil {
			return Plan{}, err
		}
		originalAddress[name] = node.Address
		if override, ok := request.AddressOverride[name]; ok {
			if override.IsZero() {
				return Plan{}, fmt.Errorf("address override for node %q is empty", name)
			}
			node.Address = override
		}
		planned, err := normalizeNode(node, version)
		if err != nil {
			return Plan{}, err
		}
		if _, ok := seen[planned.Name]; ok {
			return Plan{}, fmt.Errorf("duplicate node name %q", planned.Name)
		}
		seen[planned.Name] = struct{}{}
		if version == "" {
			version = planned.KubernetesVersion
		} else if planned.KubernetesVersion != version {
			return Plan{}, fmt.Errorf("node %q Kubernetes version %q does not match inventory version %q", planned.Name, planned.KubernetesVersion, version)
		}
		nodes = append(nodes, planned)
	}
	for name := range request.AddressOverride {
		if _, ok := seen[name]; !ok {
			return Plan{}, fmt.Errorf("address override references unknown node %q", name)
		}
	}

	for _, node := range nodes {
		if node.SystemRole == RoleControlPlane {
			controlPlanes = append(controlPlanes, node.Name)
		}
	}
	if request.Inventory.ExternalControlPlane && (len(controlPlanes) != 0 || request.Inventory.ControlPlaneEndpointManaged || request.InitNode != "") {
		return Plan{}, fmt.Errorf("external cluster inventory must contain only workers and cannot select an init node or managed endpoint")
	}
	if len(controlPlanes) == 0 && !request.Inventory.ExternalControlPlane {
		return Plan{}, fmt.Errorf("at least one control-plane node is required")
	}
	initNode := ""
	if !request.Inventory.ExternalControlPlane {
		var err error
		initNode, err = selectInitNode(strings.TrimSpace(request.InitNode), controlPlanes, nodes)
		if err != nil {
			return Plan{}, err
		}
	}
	controlPlaneEndpoint := request.Inventory.ControlPlaneEndpoint
	if (len(nodes) > 1 || request.Inventory.ExternalControlPlane) && controlPlaneEndpoint.IsZero() {
		return Plan{}, fmt.Errorf("control-plane endpoint is required for multi-node bootstrap")
	}

	overrides := make([]AddressOverride, 0, len(request.AddressOverride))
	for index, node := range nodes {
		if override, ok := request.AddressOverride[node.Name]; ok {
			overrides = append(overrides, AddressOverride{
				Node:    node.Name,
				Before:  originalAddress[node.Name],
				Address: override,
			})
		}
		switch node.SystemRole {
		case RoleControlPlane:
			if node.Name == initNode {
				nodes[index].Action = ActionInit
			} else {
				nodes[index].Action = ActionControlPlaneJoin
			}
		case RoleWorker:
			nodes[index].Action = ActionWorkerJoin
		}
	}
	sort.Slice(overrides, func(i, j int) bool {
		return overrides[i].Node < overrides[j].Node
	})
	bundleSource := strings.TrimSpace(request.Inventory.KubernetesBundleSource)
	bundleRef := strings.TrimSpace(request.Inventory.KubernetesBundleRef)
	if (bundleSource == "") != (bundleRef == "") {
		return Plan{}, fmt.Errorf("kubernetesBundleSource and kubernetesBundleRef must be set together")
	}
	if bundleSource != "" {
		if err := validateKubernetesBundleSelection(bundleSource, bundleRef, version); err != nil {
			return Plan{}, err
		}
	}
	return Plan{
		ExternalControlPlane:        request.Inventory.ExternalControlPlane,
		InitNode:                    initNode,
		ControlPlaneEndpoint:        controlPlaneEndpoint,
		ControlPlaneEndpointManaged: request.Inventory.ControlPlaneEndpointManaged,
		ControlPlaneEndpointDerived: request.Inventory.ControlPlaneEndpointDerived,
		KubernetesVersion:           version,
		KubernetesBundleSource:      bundleSource,
		KubernetesBundleRef:         bundleRef,
		Bootstrap:                   request.Inventory.Bootstrap,
		Nodes:                       nodes,
		AddressOverrides:            overrides,
	}, nil
}

func validateKubernetesBundleSelection(source string, ref string, kubernetesVersion string) error {
	sourceURL, err := url.Parse(source)
	if err != nil || !sourceURL.IsAbs() || sourceURL.Scheme != "https" || sourceURL.Host == "" {
		return fmt.Errorf("kubernetesBundleSource must be an absolute HTTPS URL")
	}
	if strings.Contains(ref, "/") {
		image, err := reference.ParseNamed(ref)
		pinned, ok := image.(reference.Digested)
		if err != nil || !ok || !sha256DigestPattern.MatchString(pinned.Digest().String()) {
			return fmt.Errorf("kubernetesBundleRef requires an immutable OCI manifest digest")
		}
		return nil
	}
	payloadVersion, ok := payloadVersionFromBundleRef(ref)
	if !ok {
		return fmt.Errorf("kubernetesBundleRef is invalid: expected vMAJOR.MINOR.PATCH@sha256:<digest>")
	}
	if payloadVersion != strings.TrimSpace(kubernetesVersion) {
		return fmt.Errorf("kubernetesBundleRef payload version %q does not match Kubernetes version %q", payloadVersion, kubernetesVersion)
	}
	return nil
}

func payloadVersionFromBundleRef(ref string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(ref), "@")
	if len(parts) > 2 || strings.TrimSpace(parts[0]) == "" || (len(parts) == 2 && !sha256DigestPattern.MatchString(parts[1])) {
		return "", false
	}
	if match := imageBundleTagPattern.FindStringSubmatch(parts[0]); match != nil {
		return "v" + match[1], true
	}
	if len(parts) != 2 {
		return "", false
	}
	return strings.TrimSpace(parts[0]), true
}

func normalizeNode(node Node, inventoryVersion string) (PlannedNode, error) {
	name := strings.TrimSpace(node.Name)
	if err := validateName(name); err != nil {
		return PlannedNode{}, err
	}
	address := node.Address
	if address.IsZero() {
		return PlannedNode{}, fmt.Errorf("node %q address is required", name)
	}
	role := SystemRole(strings.TrimSpace(string(node.SystemRole)))
	switch role {
	case RoleControlPlane, RoleWorker:
	default:
		return PlannedNode{}, fmt.Errorf("node %q systemRole %q is unsupported", name, node.SystemRole)
	}
	access, err := normalizeAccess(name, node.Access)
	if err != nil {
		return PlannedNode{}, err
	}
	config, err := normalizeKubeadmConfig(name, role, node.KubeadmConfig)
	if err != nil {
		return PlannedNode{}, err
	}
	version := strings.TrimSpace(node.KubernetesVersion)
	if version == "" {
		version = inventoryVersion
	}
	if version == "" {
		return PlannedNode{}, fmt.Errorf("node %q Kubernetes version is required", name)
	}
	if inventoryVersion != "" && version != inventoryVersion {
		return PlannedNode{}, fmt.Errorf("node %q Kubernetes version %q does not match inventory version %q", name, version, inventoryVersion)
	}
	kubernetesAddress := node.KubernetesAddress
	if kubernetesAddress.IsValid() {
		if err := validKubernetesAddr(kubernetesAddress); err != nil {
			return PlannedNode{}, fmt.Errorf("node %q Kubernetes address %w", name, err)
		}
		kubernetesAddress = kubernetesAddress.Unmap()
	}
	return PlannedNode{
		Name:              name,
		Address:           address,
		KubernetesAddress: kubernetesAddress,
		SystemRole:        role,
		Access:            access,
		KubeadmConfig:     config,
		KubernetesVersion: version,
		EnrollmentID:      strings.TrimSpace(node.EnrollmentID),
		MachineID:         strings.TrimSpace(node.MachineID),
	}, nil
}

func normalizeAccess(node string, access Access) (Access, error) {
	access.Method = strings.TrimSpace(access.Method)
	switch access.Method {
	case "ssh", "vsock", "agent":
	case "":
		return Access{}, fmt.Errorf("node %q access method is required", node)
	default:
		return Access{}, fmt.Errorf("node %q access method %q is unsupported", node, access.Method)
	}
	access.User = strings.TrimSpace(access.User)
	access.CredentialRef = strings.TrimSpace(access.CredentialRef)
	if access.Method != "agent" && access.CredentialRef == "" {
		return Access{}, fmt.Errorf("node %q access credentialRef is required", node)
	}
	if access.Method == "agent" && !strings.HasPrefix(access.CredentialRef, "vsock:") {
		access.CredentialRef = ""
	}
	if strings.ContainsAny(access.CredentialRef, "\n\r") {
		return Access{}, fmt.Errorf("node %q access credentialRef must be a single line", node)
	}
	if strings.Contains(access.CredentialRef, "-----BEGIN ") || bootstrapTokenPattern.MatchString(access.CredentialRef) {
		return Access{}, fmt.Errorf("node %q access credentialRef must reference credentials, not inline secret material", node)
	}
	return access, nil
}

func normalizeKubeadmConfig(node string, role SystemRole, config KubeadmConfig) (KubeadmConfig, error) {
	config.Ref = strings.TrimSpace(config.Ref)
	config.Path = strings.TrimSpace(config.Path)
	if config.Ref == "" && config.Path == "" {
		return KubeadmConfig{}, fmt.Errorf("node %q kubeadm config ref or path is required", node)
	}
	if config.Ref != "" {
		if err := validateName(config.Ref); err != nil {
			return KubeadmConfig{}, fmt.Errorf("node %q kubeadm config ref: %w", node, err)
		}
	}
	if config.Path != "" {
		if err := validateKubeadmConfigPath(config.Path); err != nil {
			return KubeadmConfig{}, fmt.Errorf("node %q kubeadm config path: %w", node, err)
		}
	}
	config.Intent = KubeadmIntent(strings.TrimSpace(string(config.Intent)))
	if config.Intent == "" {
		config.Intent = KubeadmIntent(role)
	}
	switch config.Intent {
	case IntentControlPlane, IntentWorker:
	default:
		return KubeadmConfig{}, fmt.Errorf("node %q kubeadm config intent %q is unsupported", node, config.Intent)
	}
	if role == RoleControlPlane && config.Intent != IntentControlPlane {
		return KubeadmConfig{}, fmt.Errorf("node %q systemRole control-plane requires control-plane kubeadm config intent", node)
	}
	if role == RoleWorker && config.Intent != IntentWorker {
		return KubeadmConfig{}, fmt.Errorf("node %q systemRole worker requires worker kubeadm config intent", node)
	}
	return config, nil
}

func validateKubeadmConfigPath(value string) error {
	clean := path.Clean(value)
	if clean != value {
		return fmt.Errorf("%q must be a clean absolute path", value)
	}
	if !inputpath.Contains(clean) {
		return fmt.Errorf("%q must be under %s", value, inputpath.Root+"/")
	}
	if path.Base(clean) != "config.yaml" {
		return fmt.Errorf("%q must end with config.yaml", value)
	}
	return nil
}

func selectInitNode(requested string, controlPlanes []string, nodes []PlannedNode) (string, error) {
	if requested != "" {
		for _, node := range nodes {
			if node.Name == requested {
				if node.SystemRole != RoleControlPlane {
					return "", fmt.Errorf("init node %q must be a control-plane node", requested)
				}
				return requested, nil
			}
		}
		return "", fmt.Errorf("init node %q was not found", requested)
	}
	if len(controlPlanes) == 1 {
		return controlPlanes[0], nil
	}
	return "", fmt.Errorf("multiple control-plane nodes require explicit init node")
}

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("node name is required")
	}
	if len(name) > 63 || !dnsLabelPattern.MatchString(name) {
		return fmt.Errorf("node name %q must be a single DNS-style label", name)
	}
	return nil
}

type ReadinessChecker interface {
	CheckReadiness(ctx context.Context, node PlannedNode) (ReadinessSnapshot, error)
}

type ReadinessSnapshot struct {
	KatlKubeadmReadyTarget bool
	KubernetesSysextActive bool
	KubeadmConfigExists    bool
	ContainerdActive       bool
	CRIResponsive          bool
	KubeletInstalled       bool
	EtcKubernetesWritable  bool
	EtcKubernetesProjected bool
	KubernetesVersion      string
	SystemRole             SystemRole
	KubeadmConfigIntent    KubeadmIntent
	Diagnostics            []Diagnostic
}

type Diagnostic struct {
	Field   string
	Message string
}

type ReadinessReport struct {
	Ready bool
	Nodes []NodeReadiness
}

type NodeReadiness struct {
	Name        string
	Ready       bool
	Diagnostics []Diagnostic
}

func VerifyReadiness(ctx context.Context, plan Plan, checker ReadinessChecker) (ReadinessReport, error) {
	if checker == nil {
		return ReadinessReport{}, fmt.Errorf("readiness checker is required")
	}
	report := ReadinessReport{Ready: true, Nodes: make([]NodeReadiness, 0, len(plan.Nodes))}
	for _, node := range plan.Nodes {
		snapshot, err := checker.CheckReadiness(ctx, node)
		nodeReport := NodeReadiness{Name: node.Name, Ready: true}
		if err != nil {
			nodeReport.Ready = false
			nodeReport.Diagnostics = append(nodeReport.Diagnostics, Diagnostic{Field: "access", Message: Redact(err.Error())})
		} else {
			nodeReport.Diagnostics = readinessDiagnostics(node, snapshot)
			for _, diagnostic := range snapshot.Diagnostics {
				nodeReport.Diagnostics = append(nodeReport.Diagnostics, Diagnostic{
					Field:   diagnostic.Field,
					Message: Redact(diagnostic.Message),
				})
			}
			nodeReport.Ready = len(nodeReport.Diagnostics) == 0
		}
		if !nodeReport.Ready {
			report.Ready = false
		}
		report.Nodes = append(report.Nodes, nodeReport)
	}
	return report, nil
}

func readinessDiagnostics(node PlannedNode, snapshot ReadinessSnapshot) []Diagnostic {
	var diagnostics []Diagnostic
	check := func(ok bool, field, message string) {
		if !ok {
			diagnostics = append(diagnostics, Diagnostic{Field: field, Message: message})
		}
	}
	check(snapshot.KatlKubeadmReadyTarget, "katl-kubeadm-ready.target", "target is not active")
	check(snapshot.KubernetesSysextActive, "kubernetes-sysext", "selected Kubernetes sysext is not active")
	check(snapshot.KubeadmConfigExists, "kubeadm-config", "rendered kubeadm config is missing")
	check(snapshot.ContainerdActive, "containerd", "containerd is not active")
	check(snapshot.CRIResponsive, "cri", "CRI socket is not responsive")
	check(snapshot.KubeletInstalled, "kubelet", "kubelet is not installed")
	check(snapshot.EtcKubernetesWritable, "etc-kubernetes", "/etc/kubernetes is not writable")
	check(snapshot.EtcKubernetesProjected, "etc-kubernetes", "/etc/kubernetes is not projected writable state")
	if snapshot.KubernetesVersion != "" && snapshot.KubernetesVersion != node.KubernetesVersion {
		diagnostics = append(diagnostics, Diagnostic{
			Field:   "kubernetesVersion",
			Message: fmt.Sprintf("node reports %s, plan requires %s", snapshot.KubernetesVersion, node.KubernetesVersion),
		})
	}
	if snapshot.SystemRole != "" && snapshot.SystemRole != node.SystemRole {
		diagnostics = append(diagnostics, Diagnostic{
			Field:   "systemRole",
			Message: fmt.Sprintf("node reports %s, plan requires %s", snapshot.SystemRole, node.SystemRole),
		})
	}
	if snapshot.KubeadmConfigIntent != "" && snapshot.KubeadmConfigIntent != node.KubeadmConfig.Intent {
		diagnostics = append(diagnostics, Diagnostic{
			Field:   "kubeadm-config",
			Message: fmt.Sprintf("node config intent is %s, plan requires %s", snapshot.KubeadmConfigIntent, node.KubeadmConfig.Intent),
		})
	}
	return diagnostics
}

var (
	urlPattern                = regexp.MustCompile(`https?://[^\s]+`)
	sha256DigestPattern       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	imageBundleTagPattern     = regexp.MustCompile(`:v([0-9]+\.[0-9]+\.[0-9]+)-katl\.[0-9]+$`)
	dnsLabelPattern           = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	bootstrapTokenPattern     = regexp.MustCompile(`\b[a-z0-9]{6}\.[a-z0-9]{16}\b`)
	discoveryTokenHashPattern = regexp.MustCompile(`(?i)\b(discovery-token-ca-cert-hash\s+)?sha256:[a-f0-9]{64}\b`)
	certificateKeyPattern     = regexp.MustCompile(`(?i)\b(certificate[-_ ]?key=?|certificateKey=?)\s*[a-f0-9]{64}\b`)
	bearerTokenPattern        = regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9._~+/=-]+`)
)

func Redact(value string) string {
	value = redact.PrivateKeys(value)
	value = bootstrapTokenPattern.ReplaceAllString(value, "[REDACTED BOOTSTRAP TOKEN]")
	value = discoveryTokenHashPattern.ReplaceAllString(value, "${1}[REDACTED DISCOVERY TOKEN HASH]")
	value = certificateKeyPattern.ReplaceAllString(value, "${1}[REDACTED]")
	value = redact.KubeconfigData(value)
	value = bearerTokenPattern.ReplaceAllString(value, "${1}[REDACTED]")
	return urlPattern.ReplaceAllStringFunc(value, redactURL)
}

func redactURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return value
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func Error(report ReadinessReport) error {
	if report.Ready {
		return nil
	}
	var parts []string
	for _, node := range report.Nodes {
		if node.Ready {
			continue
		}
		for _, diagnostic := range node.Diagnostics {
			parts = append(parts, fmt.Sprintf("%s %s: %s", node.Name, diagnostic.Field, diagnostic.Message))
		}
	}
	if len(parts) == 0 {
		return errors.New("bootstrap readiness failed")
	}
	return errors.New(strings.Join(parts, "; "))
}
