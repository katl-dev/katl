// Package inputpath defines the filesystem root of stored kubeadm input.
package inputpath

import (
	"path"
	"strings"
)

const Root = "/etc/katl/kubeadm"

// Contains reports whether p lies within the kubeadm input tree.
func Contains(p string) bool {
	return strings.HasPrefix(path.Clean(p), Root+"/")
}
