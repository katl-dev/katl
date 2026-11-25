package scenarios

import "path/filepath"

// kubernetesSourceSysext locates the sysext a scenario bootstraps its nodes
// from. Upgrade scenarios start from the older upgrade source so the target
// payload is a real upgrade; every other scenario uses the default payload.
func kubernetesSourceSysext(repo string, upgrade bool) (artifactPath, metadataPath string) {
	name := "katl-kubernetes"
	if upgrade {
		name = "katl-kubernetes-upgrade-source"
	}
	artifactPath = filepath.Join(repo, "_build/mkosi", name+".raw")
	return artifactPath, artifactPath + ".json"
}
