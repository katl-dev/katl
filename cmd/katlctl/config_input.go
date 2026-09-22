package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/configbundle"
)

type katlConfigInput struct {
	Archive []byte
	Bundle  configbundle.Bundle
	Source  bool
}

type nodeConfigurationInput struct {
	Source      bool
	ClusterName string
	Inventory   inventory.Inventory
	Nodes       map[string]configapply.RenderNodeRequest
}

func loadNodeConfiguration(ctx context.Context, path string, planning configbundle.PlanningInputs) (nodeConfigurationInput, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nodeConfigurationInput{}, err
	}
	result := nodeConfigurationInput{Nodes: make(map[string]configapply.RenderNodeRequest)}
	if _, err := configbundle.DecodeSource(bytes.NewReader(data)); err == nil {
		compiled, err := configbundle.PlanConfiguration(configbundle.BuildRequest{
			Context:    ctx,
			SourcePath: path,
			Planning:   planning,
		})
		if err != nil {
			return nodeConfigurationInput{}, err
		}
		result.ClusterName = compiled.ClusterName
		result.Source = true
		result.Inventory = compiled.Plan.BootstrapInventory
		for _, node := range compiled.Plan.Nodes {
			selections := compiled.SystemExtensionSelections[node.Name]
			result.Nodes[node.Name] = configapply.RenderNodeRequest{
				NodeName:                  node.Name,
				Manifest:                  node.InstallManifest,
				KubeadmConfigs:            compiled.KubeadmConfigs,
				SourceID:                  compiled.ClusterName,
				SystemExtensionSelections: &selections,
				APIProxy:                  node.APIProxy,
			}
		}
		return result, nil
	}
	loaded, err := loadKatlConfig(path, configBundleCreator, configbundle.PlanningInputs{}, nil)
	if err != nil {
		return nodeConfigurationInput{}, err
	}
	result.ClusterName = loaded.Bundle.Manifest.ClusterName
	result.Inventory = loaded.Bundle.Manifest.Cluster.BootstrapInventory
	for _, node := range result.Inventory.Nodes {
		selected, err := configbundle.ReadSelectedNode(bytes.NewReader(loaded.Archive), configbundle.ReadOptions{
			NodeName:                node.Name,
			AllowMissingKatlosImage: true,
		})
		if err != nil {
			return nodeConfigurationInput{}, err
		}
		result.Nodes[node.Name] = configapply.RenderNodeRequest{
			NodeName:                node.Name,
			Manifest:                selected.InstallManifest,
			KubeadmConfigs:          selected.KubeadmConfigs,
			SourceID:                result.ClusterName,
			SystemExtensionPayloads: configApplySystemExtensionPayloads(selected.SystemExtensionPayloads),
			APIProxy:                selected.NodeMaterial.APIProxy,
		}
	}
	return result, nil
}

func loadKatlConfig(path, createdBy string, planning configbundle.PlanningInputs, stderr io.Writer) (katlConfigInput, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return katlConfigInput{}, fmt.Errorf("--config is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return katlConfigInput{}, fmt.Errorf("read --config %s: %w", path, err)
	}

	_, sourceErr := configbundle.DecodeSource(bytes.NewReader(data))
	if sourceErr == nil {
		archive, result, err := configbundle.BuildArchive(configbundle.BuildRequest{
			SourcePath:     path,
			KatlctlVersion: version,
			KatlctlCommit:  commit,
			CreatedBy:      createdBy,
			Planning:       planning,
		})
		if err != nil {
			return katlConfigInput{}, fmt.Errorf("compile --config %s: %w", path, err)
		}
		bundle, err := configbundle.ReadBundle(bytes.NewReader(archive), result.Digest)
		if err != nil {
			return katlConfigInput{}, fmt.Errorf("read compiled --config %s: %w", path, err)
		}
		if err := writeCompilationWarnings(stderr, result.Warnings); err != nil {
			return katlConfigInput{}, err
		}
		return katlConfigInput{Archive: archive, Bundle: bundle, Source: true}, nil
	}

	bundle, bundleErr := configbundle.ReadBundle(bytes.NewReader(data), "")
	if bundleErr != nil {
		return katlConfigInput{}, fmt.Errorf("read --config %s as ClusterConfig YAML or Katl config bundle: YAML: %v; bundle: %w", path, sourceErr, bundleErr)
	}
	return katlConfigInput{Archive: data, Bundle: bundle}, nil
}

func writeCompilationWarnings(stderr io.Writer, warnings []configbundle.CompilationWarning) error {
	if stderr == nil {
		return nil
	}
	for _, warning := range warnings {
		if _, err := fmt.Fprintf(stderr, "warning: %s (node %s): %s\n", warning.Path, warning.Node, warning.Message); err != nil {
			return err
		}
		if warning.SuggestedValue != "" {
			if _, err := fmt.Fprintf(stderr, "  suggested value: %s\n", warning.SuggestedValue); err != nil {
				return err
			}
		}
	}
	return nil
}
