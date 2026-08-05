package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type configResolveOptions struct {
	node   string
	output string
}

func newConfigResolveCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := configResolveOptions{output: "yaml"}
	cmd := &cobra.Command{
		Use:   "resolve SOURCE --node NODE",
		Short: "Show the effective configuration for one node",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			_ = stderr
			if opts.output != "yaml" && opts.output != "json" {
				return fmt.Errorf("--output = %q, want yaml or json", opts.output)
			}
			report, err := resolveNodeConfig(args[0], opts.node, stderr)
			if err != nil {
				return err
			}
			return writeConfigInspection(stdout, opts.output, report)
		},
	}
	cmd.Flags().StringVar(&opts.node, "node", "", "node name to resolve (required for multi-node configs)")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: yaml or json")
	return cmd
}

type configDiffOptions struct {
	node   string
	output string
}

func newConfigDiffCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := configDiffOptions{output: "text"}
	cmd := &cobra.Command{
		Use:   "diff BEFORE AFTER --node NODE",
		Short: "Compare effective node configuration",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			_ = stderr
			if opts.output != "text" && opts.output != "yaml" && opts.output != "json" {
				return fmt.Errorf("--output = %q, want text, yaml, or json", opts.output)
			}
			before, err := resolveNodeConfig(args[0], opts.node, stderr)
			if err != nil {
				return fmt.Errorf("resolve before config: %w", err)
			}
			after, err := resolveNodeConfig(args[1], opts.node, stderr)
			if err != nil {
				return fmt.Errorf("resolve after config: %w", err)
			}
			report, err := configbundle.DiffNodeResolutions(before, after)
			if err != nil {
				return err
			}
			if opts.output == "text" {
				return writeConfigDiffText(stdout, report)
			}
			return writeConfigInspection(stdout, opts.output, report)
		},
	}
	cmd.Flags().StringVar(&opts.node, "node", "", "node name to compare (required for multi-node configs)")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text, yaml, or json")
	return cmd
}

func resolveNodeConfig(sourcePath, nodeName string, stderr io.Writer) (configbundle.NodeResolution, error) {
	archive, result, err := configbundle.BuildArchive(configbundle.BuildRequest{
		SourcePath:     sourcePath,
		KatlctlVersion: version,
		KatlctlCommit:  commit,
		CreatedBy:      "katlctl config resolve",
	})
	if err != nil {
		return configbundle.NodeResolution{}, err
	}
	if err := writeCompilationWarnings(stderr, result.Warnings); err != nil {
		return configbundle.NodeResolution{}, err
	}
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" && len(result.Manifest.Nodes) == 1 {
		nodeName = result.Manifest.Nodes[0].Name
	}
	selected, err := configbundle.ReadSelectedNode(bytes.NewReader(archive), configbundle.ReadOptions{
		NodeName:                nodeName,
		AllowMissingKatlosImage: true,
	})
	if err != nil {
		return configbundle.NodeResolution{}, err
	}
	return configbundle.InspectSelectedNode(selected)
}

func writeConfigInspection(stdout io.Writer, output string, report any) error {
	switch output {
	case "json":
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal config inspection: %w", err)
		}
		_, err = stdout.Write(append(data, '\n'))
		return err
	case "yaml":
		data, err := yaml.Marshal(report)
		if err != nil {
			return fmt.Errorf("marshal config inspection: %w", err)
		}
		_, err = stdout.Write(data)
		return err
	default:
		return fmt.Errorf("--output = %q, want yaml or json", output)
	}
}

func writeConfigDiffText(stdout io.Writer, report configbundle.ConfigDiff) error {
	if len(report.Changes) == 0 {
		_, err := fmt.Fprintf(stdout, "%s has no effective changes for node %s\n", report.AfterCluster, report.Node)
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintf(w, "PATH\tCLASSIFICATION\tREQUIRED OPERATION\n"); err != nil {
		return err
	}
	for _, change := range report.Changes {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", change.Path, change.Classification, change.RequiredOperation); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "Overall: %s\n", report.Classification.Overall)
	return err
}
