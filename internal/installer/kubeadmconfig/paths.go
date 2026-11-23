package kubeadmconfig

import "github.com/katl-dev/katl/internal/installer/kubeadmconfig/inputpath"

const (
	inputRoot = inputpath.Root

	// BootstrapInitConfigPath holds the config that bootstrap passes to
	// kubeadm init. It is a bootstrap artifact: the input path always holds the
	// node's kubeadm input, never the rendered init config.
	BootstrapInitConfigPath = "/etc/katl/bootstrap/kubeadm-init.yaml"
)

// InputDir is the directory holding kubeadm input ref.
func InputDir(ref string) string {
	return inputRoot + "/" + ref
}

// ConfigPath is where a node holds the configuration of kubeadm input ref.
func ConfigPath(ref string) string {
	return inputRoot + "/" + ref + "/config.yaml"
}

// PatchesDir is where a node holds the kubeadm patches of input ref.
func PatchesDir(ref string) string {
	return inputRoot + "/" + ref + "/patches"
}
