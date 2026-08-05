package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/katl-dev/katl/internal/installer/configbundle"
)

type katlConfigInput struct {
	Archive  []byte
	Bundle   configbundle.Bundle
	Source   bool
	Warnings []configbundle.CompilationWarning
}

func loadKatlConfig(path, createdBy string, planning configbundle.PlanningInputs) (katlConfigInput, error) {
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
		return katlConfigInput{Archive: archive, Bundle: bundle, Source: true, Warnings: result.Warnings}, nil
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
