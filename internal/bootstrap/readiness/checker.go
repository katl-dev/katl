package readiness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"gopkg.in/yaml.v3"
)

const (
	NodeMetadataPath          = "/etc/katl/node.json"
	ProjectedKubernetesSource = "/var/lib/katl/kubernetes/etc-kubernetes"
)

type NodeMetadata struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Identity   NodeIdentity         `json:"identity"`
	SystemRole inventory.SystemRole `json:"systemRole"`
	Kubeadm    NodeKubeadm          `json:"kubeadm,omitempty"`
	Kubernetes NodeKubernetes       `json:"kubernetes,omitempty"`
}

type NodeIdentity struct {
	Hostname string `json:"hostname"`
}

type NodeKubeadm struct {
	ConfigRef  string                  `json:"configRef,omitempty"`
	ConfigPath string                  `json:"configPath,omitempty"`
	Intent     inventory.KubeadmIntent `json:"intent,omitempty"`
}

type NodeKubernetes struct {
	PayloadVersion string `json:"payloadVersion,omitempty"`
	ActivationPath string `json:"activationPath,omitempty"`
}

type CommandTransport interface {
	RunCommand(ctx context.Context, node inventory.PlannedNode, req CommandRequest) (CommandResult, error)
	ReadFile(ctx context.Context, node inventory.PlannedNode, req FileRequest) (FileResult, error)
	WriteFile(ctx context.Context, node inventory.PlannedNode, req WriteFileRequest) (WriteFileResult, error)
}

type CommandRequest struct {
	Argv            []string
	Timeout         time.Duration
	StdoutLimit     uint32
	StderrLimit     uint32
	SensitiveOutput bool
}

type CommandResult struct {
	ExitStatus      int32
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
}

type FileRequest struct {
	Path      string
	Timeout   time.Duration
	MaxBytes  uint32
	Sensitive bool
}

type FileResult struct {
	Content   []byte
	Truncated bool
	Redaction string
}

type WriteFileRequest struct {
	Path      string
	Content   []byte
	Mode      uint32
	Timeout   time.Duration
	Sensitive bool
}

type WriteFileResult struct {
	SizeBytes uint32
	Redaction string
}

type Checker struct {
	Agent        CommandTransport
	Timeout      time.Duration
	OutputLimit  uint32
	FileLimit    uint32
	MetadataPath string
}

func (c Checker) CheckReadiness(ctx context.Context, node inventory.PlannedNode) (inventory.ReadinessSnapshot, error) {
	if node.Access.Method != "agent" {
		return inventory.ReadinessSnapshot{}, fmt.Errorf("node %q access method %q is not supported by bootstrap readiness checker", node.Name, node.Access.Method)
	}
	if c.Agent == nil {
		return inventory.ReadinessSnapshot{}, fmt.Errorf("node %q agent transport is required", node.Name)
	}
	check := checker{Checker: c}
	return check.check(ctx, node)
}

type checker struct {
	Checker
	diagnostics []inventory.Diagnostic
}

func (c *checker) check(ctx context.Context, node inventory.PlannedNode) (inventory.ReadinessSnapshot, error) {
	snapshot := inventory.ReadinessSnapshot{}
	var err error
	snapshot.KatlKubeadmReadyTarget, err = c.commandOK(ctx, node, "katl-kubeadm-ready.target", []string{"systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"}, false)
	if err != nil {
		return snapshot, err
	}
	metadata := c.nodeMetadata(ctx, node)
	snapshot.SystemRole = metadata.SystemRole
	kubernetesSysextPath := c.kubernetesSysextPath(metadata)
	if kubernetesSysextPath != "" {
		snapshot.KubernetesSysextActive, err = c.commandOK(ctx, node, "kubernetes-sysext", []string{"test", "-e", kubernetesSysextPath}, false)
		if err != nil {
			return snapshot, err
		}
	}
	kubeadmVersion, err := c.kubeadmVersion(ctx, node)
	if err != nil {
		return snapshot, err
	}
	snapshot.KubernetesVersion = kubeadmVersion
	configPath := kubeadmConfigPath(node)
	snapshot.KubeadmConfigExists, err = c.commandOK(ctx, node, "kubeadm-config", []string{"test", "-f", configPath}, false)
	if err != nil {
		return snapshot, err
	}
	if snapshot.KubeadmConfigExists {
		snapshot.KubeadmConfigIntent = c.kubeadmConfigIntent(ctx, node, configPath)
	}
	snapshot.ContainerdActive, err = c.commandOK(ctx, node, "containerd", []string{"systemctl", "is-active", "--quiet", "containerd.service"}, false)
	if err != nil {
		return snapshot, err
	}
	snapshot.CRIResponsive, err = c.commandOK(ctx, node, "cri", []string{"crictl", "info"}, true)
	if err != nil {
		return snapshot, err
	}
	kubeletBinary, err := c.commandOK(ctx, node, "kubelet", []string{"test", "-x", "/usr/bin/kubelet"}, false)
	if err != nil {
		return snapshot, err
	}
	kubeletService, err := c.commandOK(ctx, node, "kubelet", []string{"systemctl", "cat", "kubelet.service"}, false)
	if err != nil {
		return snapshot, err
	}
	snapshot.KubeletInstalled = kubeletBinary && kubeletService
	snapshot.EtcKubernetesWritable, err = c.commandOK(ctx, node, "etc-kubernetes", []string{"test", "-w", "/etc/kubernetes"}, false)
	if err != nil {
		return snapshot, err
	}
	snapshot.EtcKubernetesProjected, err = c.etcKubernetesProjected(ctx, node)
	if err != nil {
		return snapshot, err
	}
	snapshot.Diagnostics = c.diagnostics
	return snapshot, nil
}

func (c *checker) commandOK(ctx context.Context, node inventory.PlannedNode, field string, argv []string, sensitive bool) (bool, error) {
	result, err := c.Agent.RunCommand(ctx, node, CommandRequest{
		Argv:            argv,
		Timeout:         c.timeout(),
		StdoutLimit:     c.outputLimit(),
		StderrLimit:     c.outputLimit(),
		SensitiveOutput: sensitive,
	})
	if err != nil {
		return false, err
	}
	if result.ExitStatus == 0 {
		return true, nil
	}
	c.diagnostics = append(c.diagnostics, commandDiagnostic(field, argv, result))
	return false, nil
}

func (c *checker) kubeadmVersion(ctx context.Context, node inventory.PlannedNode) (string, error) {
	result, err := c.Agent.RunCommand(ctx, node, CommandRequest{
		Argv:        []string{"kubeadm", "version", "-o", "short"},
		Timeout:     c.timeout(),
		StdoutLimit: c.outputLimit(),
		StderrLimit: c.outputLimit(),
	})
	if err != nil {
		return "", err
	}
	if result.ExitStatus != 0 {
		c.diagnostics = append(c.diagnostics, commandDiagnostic("kubernetes-sysext", []string{"kubeadm", "version", "-o", "short"}, result))
		return "", nil
	}
	version := firstKubernetesVersion(result.Stdout)
	if version == "" {
		version = firstKubernetesVersion(result.Stderr)
	}
	if version == "" {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "kubernetes-sysext", Message: "kubeadm did not report a Kubernetes version"})
		return "", nil
	}
	return version, nil
}

func (c *checker) kubeadmConfigIntent(ctx context.Context, node inventory.PlannedNode, configPath string) inventory.KubeadmIntent {
	file, err := c.Agent.ReadFile(ctx, node, FileRequest{
		Path:      configPath,
		Timeout:   c.timeout(),
		MaxBytes:  c.fileLimit(),
		Sensitive: true,
	})
	if err != nil {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "kubeadm-config", Message: inventory.Redact(err.Error())})
		return ""
	}
	intent, err := detectKubeadmIntent(file.Content)
	if err != nil {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "kubeadm-config", Message: inventory.Redact(err.Error())})
		return ""
	}
	return intent
}

func (c *checker) etcKubernetesProjected(ctx context.Context, node inventory.PlannedNode) (bool, error) {
	result, err := c.Agent.RunCommand(ctx, node, CommandRequest{
		Argv:        []string{"findmnt", "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE"},
		Timeout:     c.timeout(),
		StdoutLimit: c.outputLimit(),
		StderrLimit: c.outputLimit(),
	})
	if err != nil {
		return false, err
	}
	if result.ExitStatus != 0 {
		c.diagnostics = append(c.diagnostics, commandDiagnostic("etc-kubernetes", []string{"findmnt", "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE"}, result))
		return false, nil
	}
	source := strings.TrimSpace(result.Stdout)
	if !projectedKubernetesSourceMatches(source, ProjectedKubernetesSource) {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{
			Field:   "etc-kubernetes",
			Message: fmt.Sprintf("/etc/kubernetes is backed by %q, want %q", inventory.Redact(source), ProjectedKubernetesSource),
		})
		return false, nil
	}
	return true, nil
}

func projectedKubernetesSourceMatches(source string, projected string) bool {
	source = strings.TrimSpace(source)
	projected = strings.TrimSpace(projected)
	if source == projected {
		return true
	}
	statePath, ok := strings.CutPrefix(projected, "/var")
	if !ok || statePath == "" {
		return false
	}
	return strings.HasSuffix(source, "["+statePath+"]")
}

func (c *checker) nodeMetadata(ctx context.Context, node inventory.PlannedNode) NodeMetadata {
	path := c.metadataPath()
	exists, err := c.commandOK(ctx, node, "node-metadata", []string{"test", "-f", path}, false)
	if err != nil {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "node-metadata", Message: inventory.Redact(err.Error())})
		return NodeMetadata{}
	}
	if !exists {
		return NodeMetadata{}
	}
	file, err := c.Agent.ReadFile(ctx, node, FileRequest{
		Path:      path,
		Timeout:   c.timeout(),
		MaxBytes:  c.fileLimit(),
		Sensitive: false,
	})
	if err != nil {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "node-metadata", Message: inventory.Redact(err.Error())})
		return NodeMetadata{}
	}
	var metadata NodeMetadata
	if err := json.Unmarshal(file.Content, &metadata); err != nil {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "node-metadata", Message: "decode node metadata: " + inventory.Redact(err.Error())})
		return NodeMetadata{}
	}
	if metadata.APIVersion != "katl.dev/v1alpha1" || metadata.Kind != "NodeMetadata" {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{
			Field:   "node-metadata",
			Message: fmt.Sprintf("metadata apiVersion/kind is %s/%s, want katl.dev/v1alpha1/NodeMetadata", metadata.APIVersion, metadata.Kind),
		})
	}
	if metadata.SystemRole == "" {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "node-metadata", Message: "metadata systemRole is required"})
	}
	if metadata.Kubernetes.PayloadVersion == "" {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "node-metadata", Message: "metadata kubernetes.payloadVersion is required"})
	} else if metadata.Kubernetes.PayloadVersion != node.KubernetesVersion {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{
			Field:   "node-metadata",
			Message: fmt.Sprintf("metadata Kubernetes version %s, plan requires %s", metadata.Kubernetes.PayloadVersion, node.KubernetesVersion),
		})
	}
	if metadata.Kubernetes.ActivationPath == "" {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "node-metadata", Message: "metadata kubernetes.activationPath is required"})
	} else if !validKubernetesActivationPath(metadata.Kubernetes.ActivationPath) {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{
			Field:   "node-metadata",
			Message: fmt.Sprintf("metadata kubernetes.activationPath %s must be under /run/extensions", metadata.Kubernetes.ActivationPath),
		})
	}
	if node.KubeadmConfig.Ref != "" && metadata.Kubeadm.ConfigRef == "" {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "node-metadata", Message: "metadata kubeadm.configRef is required"})
	} else if node.KubeadmConfig.Ref != "" && metadata.Kubeadm.ConfigRef != node.KubeadmConfig.Ref {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{
			Field:   "node-metadata",
			Message: fmt.Sprintf("metadata kubeadm config ref %s, plan requires %s", metadata.Kubeadm.ConfigRef, node.KubeadmConfig.Ref),
		})
	}
	configPath := kubeadmConfigPath(node)
	if configPath != "" && metadata.Kubeadm.ConfigPath == "" {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "node-metadata", Message: "metadata kubeadm.configPath is required"})
	} else if configPath != "" && metadata.Kubeadm.ConfigPath != configPath {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{
			Field:   "node-metadata",
			Message: fmt.Sprintf("metadata kubeadm config path %s, plan requires %s", metadata.Kubeadm.ConfigPath, configPath),
		})
	}
	if node.KubeadmConfig.Intent != "" && metadata.Kubeadm.Intent == "" {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{Field: "node-metadata", Message: "metadata kubeadm.intent is required"})
	} else if node.KubeadmConfig.Intent != "" && metadata.Kubeadm.Intent != node.KubeadmConfig.Intent {
		c.diagnostics = append(c.diagnostics, inventory.Diagnostic{
			Field:   "node-metadata",
			Message: fmt.Sprintf("metadata kubeadm intent %s, plan requires %s", metadata.Kubeadm.Intent, node.KubeadmConfig.Intent),
		})
	}
	return metadata
}

func (c *checker) kubernetesSysextPath(metadata NodeMetadata) string {
	if !validKubernetesActivationPath(metadata.Kubernetes.ActivationPath) {
		return ""
	}
	return metadata.Kubernetes.ActivationPath
}

func validKubernetesActivationPath(value string) bool {
	value = strings.TrimSpace(value)
	cleaned := path.Clean("/" + strings.TrimPrefix(value, "/"))
	return value == cleaned && strings.HasPrefix(cleaned, "/run/extensions/") && cleaned != "/run/extensions/"
}

func (c *checker) timeout() time.Duration {
	if c.Timeout != 0 {
		return c.Timeout
	}
	return 10 * time.Second
}

func (c *checker) outputLimit() uint32 {
	if c.OutputLimit != 0 {
		return c.OutputLimit
	}
	return 64 << 10
}

func (c *checker) fileLimit() uint32 {
	if c.FileLimit != 0 {
		return c.FileLimit
	}
	return 256 << 10
}

func (c *checker) metadataPath() string {
	if strings.TrimSpace(c.MetadataPath) != "" {
		return c.MetadataPath
	}
	return NodeMetadataPath
}

func kubeadmConfigPath(node inventory.PlannedNode) string {
	if strings.TrimSpace(node.KubeadmConfig.Path) != "" {
		return node.KubeadmConfig.Path
	}
	if strings.TrimSpace(node.KubeadmConfig.Ref) != "" {
		return "/etc/katl/kubeadm/" + node.KubeadmConfig.Ref + "/config.yaml"
	}
	return ""
}

func commandDiagnostic(field string, argv []string, result CommandResult) inventory.Diagnostic {
	parts := []string{fmt.Sprintf("%q exited %d", strings.Join(argv, " "), result.ExitStatus)}
	if result.Stdout != "" {
		parts = append(parts, "stdout: "+inventory.Redact(strings.TrimSpace(result.Stdout)))
	}
	if result.Stderr != "" {
		parts = append(parts, "stderr: "+inventory.Redact(strings.TrimSpace(result.Stderr)))
	}
	if result.StdoutTruncated || result.StderrTruncated {
		parts = append(parts, "output truncated")
	}
	return inventory.Diagnostic{Field: field, Message: strings.Join(parts, "; ")}
}

func detectKubeadmIntent(data []byte) (inventory.KubeadmIntent, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("decode kubeadm config: %w", err)
		}
		if doc.Kind == 0 {
			continue
		}
		root := documentMap(&doc)
		kind := scalar(root, "kind")
		switch kind {
		case "InitConfiguration", "ClusterConfiguration":
			return inventory.IntentControlPlane, nil
		case "JoinConfiguration":
			if hasMappingKey(root, "controlPlane") {
				return inventory.IntentControlPlane, nil
			}
			return inventory.IntentWorker, nil
		}
	}
	return "", fmt.Errorf("kubeadm config did not contain InitConfiguration or JoinConfiguration")
}

func documentMap(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

func scalar(node *yaml.Node, key string) string {
	if node == nil {
		return ""
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key && node.Content[i+1].Kind == yaml.ScalarNode {
			return node.Content[i+1].Value
		}
	}
	return ""
}

func hasMappingKey(node *yaml.Node, key string) bool {
	if node == nil {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}

var kubernetesVersionPattern = regexp.MustCompile(`v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?`)

func firstKubernetesVersion(value string) string {
	return kubernetesVersionPattern.FindString(value)
}
