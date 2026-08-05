package configbundle

import (
	"fmt"
	"strings"
)

func publicClusterPlanError(source SourceConfig, err error) error {
	message := err.Error()
	for i, node := range source.Spec.Nodes {
		quotedName := fmt.Sprintf("%q", strings.TrimSpace(node.Name))
		path := sourceNodePath(node, i)
		if detail, ok := strings.CutPrefix(message, "node "+quotedName+" manifest: "); ok {
			return fmt.Errorf("%s.%s", path, publicManifestMessage(detail))
		}
		if detail, ok := strings.CutPrefix(message, "node "+quotedName+": "); ok {
			return fmt.Errorf("%s: %s", path, publicCompilerMessage(detail))
		}
		if detail, ok := strings.CutPrefix(message, "node "+quotedName+" "); ok {
			return fmt.Errorf("%s.%s", path, publicNodeMessage(detail))
		}
	}
	return fmt.Errorf("%s", publicCompilerMessage(message))
}

func publicManifestMessage(message string) string {
	replacements := []struct {
		internal string
		public   string
	}{
		{"node.identity.ssh.authorizedKeys", "access.ssh.authorizedKeys"},
		{"node.identity.hostname", "name"},
		{"node.systemRole", "controlPlane"},
		{"node.kernel", "kernel"},
		{"node.systemExtensions", "systemExtensions"},
		{"node.kubernetes.address", "kubernetes.address"},
		{"node.bootstrap.labels", "kubernetes.labels"},
		{"node.bootstrap.taints", "kubernetes.taints"},
		{"node.bootstrap.nodeAddress", "management.address"},
		{"node.bootstrap.inventoryNodeName", "name"},
		{"node.bootstrap.clusterName", "metadata.name"},
		{"install.targetDisk", "install.systemDisk"},
		{"install.volumes", "storage.disks"},
	}
	for _, replacement := range replacements {
		if strings.HasPrefix(message, replacement.internal) {
			return replacement.public + strings.TrimPrefix(message, replacement.internal)
		}
	}
	if detail, ok := strings.CutPrefix(message, "node.hostConfiguration: "); ok {
		return "hostConfiguration." + publicHostConfigurationMessage(detail)
	}
	return publicCompilerMessage(message)
}

func publicNodeMessage(message string) string {
	replacements := []struct {
		internal string
		public   string
	}{
		{"install.targetDisk", "install.systemDisk"},
		{"address", "management.address"},
		{"systemRole", "controlPlane"},
		{"access", "management"},
	}
	for _, replacement := range replacements {
		if strings.HasPrefix(message, replacement.internal) {
			return replacement.public + strings.TrimPrefix(message, replacement.internal)
		}
	}
	return publicCompilerMessage(message)
}

func publicCompilerMessage(message string) string {
	replacer := strings.NewReplacer(
		"spec.kubernetes.payloadVersion", "spec.kubernetes.version",
		"spec.defaults.install.targetDisk", "spec.defaults.install.systemDisk",
		"install.targetDisk", "install.systemDisk",
		"install.volumes", "storage.disks",
	)
	return replacer.Replace(message)
}
