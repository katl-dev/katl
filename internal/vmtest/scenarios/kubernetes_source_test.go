package scenarios

import (
	"path/filepath"

	"github.com/katl-dev/katl/internal/vmtest"
)

// kubernetesSourceSysext locates the sysext a scenario bootstraps its nodes
// from. Upgrade scenarios start from the older upgrade source so the target
// payload is a real upgrade; every other scenario uses the default payload.
func kubernetesSourceSysext(repo string, upgrade bool) (artifactPath, metadataPath string, err error) {
	name := "katl-kubernetes"
	if upgrade {
		name = "katl-kubernetes-upgrade-source"
	}
	buildDir, err := vmtest.MkosiDir(repo)
	if err != nil {
		return "", "", err
	}
	artifactPath = filepath.Join(buildDir, name+".raw")
	return artifactPath, artifactPath + ".json", nil
}
