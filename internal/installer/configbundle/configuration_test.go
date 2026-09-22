package configbundle

import (
	"context"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
)

func TestConfigurationLeavesSelectionToNode(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "  defaults:\n", `  defaults:
    systemExtensions:
      - release: registry.invalid/driver
      - bundle: registry.invalid/userspace:stable
`, 1)
	plan, err := PlanConfiguration(BuildRequest{
		SourcePath: writeSource(t, source),
		ResolveSystemExtension: func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
			t.Fatal("workstation acquired an extension")
			return systemextensionbundle.Resolved{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Plan.Nodes {
		selections := plan.SystemExtensionSelections[node.Name]
		if len(selections) != 2 || selections[0].Release != "registry.invalid/driver" || selections[1].Bundle != "registry.invalid/userspace:stable" {
			t.Fatalf("node %s lost operator intent: %+v", node.Name, selections)
		}
		if len(node.InstallManifest.Node.SystemExtensions) != 0 {
			t.Fatal("unresolved intent was presented as installation-ready metadata")
		}
	}
}
