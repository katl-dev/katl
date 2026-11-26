package kubeadmconfig

import (
	"reflect"
	"strings"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

// NodeInput is the kubeadm input of one node: the shared configuration
// carrying that node's registration. Only ForNode and ReadNodeInput construct
// it, so every holder of a node's input holds the same form.
type NodeInput struct {
	plan Plan
}

// ForNode returns the kubeadm input node holds for the shared plan. Katl owns
// the registration fields (node-ip, node-labels, taints and the advertise
// address), and they come from the node's desired configuration. Registration
// that plan already carries is replaced rather than rejected, so ForNode of a
// shared, registered or stale plan converges on one result. External nodes
// take plan unchanged.
func ForNode(plan Plan, node manifest.NodeConfig) (NodeInput, error) {
	if node.Kubernetes.External {
		return NodeInput{plan: clonePlan(plan)}, nil
	}
	shared, stripped, err := withoutNodeRegistration(plan, true)
	if err != nil {
		return NodeInput{}, err
	}
	if !stripped {
		// Registering the original keeps its bytes when it holds no registration.
		shared = plan
	}
	registered, err := withNodeRegistration(shared, registrationFor(node))
	if err != nil {
		return NodeInput{}, err
	}
	return NodeInput{plan: registered}, nil
}

// ReadNodeInput reads node's input from its stored files. Releases stored the
// input in different forms, so it is normalised through ForNode.
func ReadNodeInput(name string, files []File, node manifest.NodeConfig) (NodeInput, error) {
	plan, err := PlanFromInputFiles(name, files)
	if err != nil {
		return NodeInput{}, err
	}
	return ForNode(plan, node)
}

// IsZero reports whether the input was not built by ForNode or ReadNodeInput.
func (i NodeInput) IsZero() bool { return i.plan.Name == "" }

// Name is the ref of the KubeadmConfig the input was built from.
func (i NodeInput) Name() string { return i.plan.Name }

// Plan returns a copy of the input's files and ownership.
func (i NodeInput) Plan() Plan { return clonePlan(i.plan) }

// InputFiles returns the files to persist, including ownership metadata.
func (i NodeInput) InputFiles() ([]File, error) { return i.plan.InputFiles() }

// InputDigest binds the input's native files and ownership.
func (i NodeInput) InputDigest() (string, error) { return i.plan.InputDigest() }

// NativeEtcFiles returns the input's native files, verbatim.
func (i NodeInput) NativeEtcFiles() []confext.NativeEtcFile { return i.plan.NativeEtcFiles() }

// SameKubeadmConfig reports whether two inputs configure kubeadm alike.
//
// It ignores node registration. The identity and metadata domains plan the
// node address, labels and taints, and this comparison does not own
// registration. Generations already hold the input in several forms:
// v2026.11.0 and v2026.11.1 applies stored the shared input, bootstrap, join
// and bundles stored it registered, and a label or taint edit leaves an
// earlier registration in place. Comparing registration would plan a kubeadm
// change for a generation whose kubeadm configuration is unchanged.
func (i NodeInput) SameKubeadmConfig(other NodeInput) bool {
	return SameSharedConfig(i.plan, other.plan)
}

// SameSharedConfig is SameKubeadmConfig for plans that may or may not carry
// registration.
func SameSharedConfig(previous, desired Plan) bool {
	previous, _, err := withoutNodeRegistration(previous, false)
	if err != nil {
		return false
	}
	desired, _, err = withoutNodeRegistration(desired, false)
	if err != nil {
		return false
	}
	if previous.NodeLocalKubelet != desired.NodeLocalKubelet || previous.SharedKubeletBase != desired.SharedKubeletBase {
		return false
	}
	previousFiles := previous.NativeEtcFiles()
	desiredFiles := desired.NativeEtcFiles()
	if len(previousFiles) != len(desiredFiles) {
		return false
	}
	for i := range previousFiles {
		previousContent := strings.TrimSuffix(previousFiles[i].Content, "\n")
		desiredContent := strings.TrimSuffix(desiredFiles[i].Content, "\n")
		previousFiles[i].Content = ""
		desiredFiles[i].Content = ""
		if previousContent != desiredContent || !reflect.DeepEqual(previousFiles[i], desiredFiles[i]) {
			return false
		}
	}
	return true
}

// BootstrapInitConfig renders the config bootstrap passes to kubeadm init for
// this input, as a file at BootstrapInitConfigPath.
func (i NodeInput) BootstrapInitConfig(endpoint, vip string) (confext.NativeEtcFile, error) {
	rendered, err := RenderInitConfig(i.plan.Config.Content, endpoint, vip)
	if err != nil {
		return confext.NativeEtcFile{}, err
	}
	content, err := delegateInitHealth(rendered)
	if err != nil {
		return confext.NativeEtcFile{}, err
	}
	return confext.NativeEtcFile{Path: BootstrapInitConfigPath, Content: string(content), Mode: 0o600}, nil
}
