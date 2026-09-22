package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/configbundle"
)

func sourceHasSystemExtensions(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		return false, nil // A compiled bundle already records its selections.
	}
	defaults, _ := source.Spec.Defaults.SystemExtensions.Get()
	if len(defaults) > 0 {
		return true, nil
	}
	for _, node := range source.Spec.Nodes {
		entries, _ := node.SystemExtensions.Get()
		if len(entries) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func renderUpgradeConfig(ctx context.Context, path, node string) ([]byte, error) {
	loaded, err := loadNodeConfiguration(ctx, path, configbundle.PlanningInputs{
		Nodes: []string{node},
	})
	if err != nil {
		return nil, err
	}
	render, ok := loaded.Nodes[node]
	if !ok {
		return nil, fmt.Errorf("node %q is not in the selected configuration", node)
	}
	render.DesiredVersion = strconv.FormatInt(configApplyNow().UnixNano(), 10)
	render.ApplyMode = generation.ApplyModeNextBoot
	return configapply.RenderNodeConfigurationChange(render)
}
