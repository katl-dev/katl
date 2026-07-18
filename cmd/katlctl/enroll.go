package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/spf13/cobra"
)

type contextSaveOptions struct {
	configInput string
	contextPath string
	contextName string
	output      string
}

type contextSaveNodeReport struct {
	Name               string `json:"name"`
	ManagementEndpoint string `json:"managementEndpoint"`
	Connected          bool   `json:"connected"`
}

type contextSaveReport struct {
	APIVersion string                  `json:"apiVersion"`
	Kind       string                  `json:"kind"`
	Context    string                  `json:"context"`
	ConfigPath string                  `json:"configPath"`
	Nodes      []contextSaveNodeReport `json:"nodes"`
}

func newContextSaveCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := contextSaveOptions{output: "text"}
	cmd := &cobra.Command{
		Use:   "save",
		Short: "Save installed KatlOS nodes as the current workstation context",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runContextSave(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.configInput, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().StringVar(&opts.contextPath, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "context name; defaults to the cluster name")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	return cmd
}

type contextFileOptions struct {
	path   string
	output string
}

func addContextFileFlags(cmd *cobra.Command, opts *contextFileOptions) {
	cmd.Flags().StringVar(&opts.path, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVarP(&opts.output, "output", "o", "text", "output format: text or json")
}

func contextFilePath(path string) (string, error) {
	if path = strings.TrimSpace(path); path != "" {
		return path, nil
	}
	return workstation.ConfigPath()
}

func loadContexts(path string) (workstation.Config, string, error) {
	resolved, err := contextFilePath(path)
	if err != nil {
		return workstation.Config{}, "", err
	}
	cfg, err := workstation.Load(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return workstation.Config{}, resolved, fmt.Errorf("no saved katlctl contexts; create one with 'katlctl context save --config cluster.yaml'")
		}
		return workstation.Config{}, resolved, err
	}
	return cfg, resolved, nil
}

func newContextListCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := contextFileOptions{output: "text"}
	cmd := &cobra.Command{Use: "list", Short: "List saved workstation contexts", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		_ = stderr
		cfg, _, err := loadContexts(opts.path)
		if err != nil {
			return err
		}
		if opts.output == "json" {
			return json.NewEncoder(stdout).Encode(cfg.Contexts)
		}
		if opts.output != "text" {
			return fmt.Errorf("--output = %q, want text or json", opts.output)
		}
		contexts := append([]workstation.Context(nil), cfg.Contexts...)
		sort.Slice(contexts, func(i, j int) bool { return contexts[i].Name < contexts[j].Name })
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "CURRENT\tCONTEXT\tCLUSTER")
		for _, ctx := range contexts {
			current := ""
			if ctx.Name == cfg.CurrentContext {
				current = "*"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", current, ctx.Name, ctx.Cluster)
		}
		return w.Flush()
	}}
	addContextFileFlags(cmd, &opts)
	return cmd
}

func newContextCurrentCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := contextFileOptions{output: "text"}
	cmd := &cobra.Command{Use: "current", Short: "Print the current workstation context", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		_ = stderr
		cfg, _, err := loadContexts(opts.path)
		if err != nil {
			return err
		}
		if opts.output == "json" {
			return json.NewEncoder(stdout).Encode(map[string]string{"currentContext": cfg.CurrentContext})
		}
		if opts.output != "text" {
			return fmt.Errorf("--output = %q, want text or json", opts.output)
		}
		_, err = fmt.Fprintln(stdout, cfg.CurrentContext)
		return err
	}}
	addContextFileFlags(cmd, &opts)
	return cmd
}

func newContextUseCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := contextFileOptions{output: "text"}
	cmd := &cobra.Command{Use: "use NAME", Short: "Select the current workstation context", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		_ = stderr
		if opts.output != "text" && opts.output != "json" {
			return fmt.Errorf("--output = %q, want text or json", opts.output)
		}
		cfg, path, err := loadContexts(opts.path)
		if err != nil {
			return err
		}
		name := strings.TrimSpace(args[0])
		found := false
		for _, ctx := range cfg.Contexts {
			found = found || ctx.Name == name
		}
		if !found {
			return fmt.Errorf("context %q was not found; run 'katlctl context list'", name)
		}
		cfg.CurrentContext = name
		if err := workstation.Save(path, cfg); err != nil {
			return err
		}
		if opts.output == "json" {
			return json.NewEncoder(stdout).Encode(map[string]string{"currentContext": name})
		}
		_, err = fmt.Fprintf(stdout, "Current context is now %s\n", name)
		return err
	}}
	addContextFileFlags(cmd, &opts)
	return cmd
}

func runContextSave(ctx context.Context, opts contextSaveOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	config, err := loadKatlConfig(opts.configInput, "katlctl context save", configbundle.PlanningInputs{})
	if err != nil {
		return err
	}
	bundle := config.Bundle
	inv := bundle.Manifest.Cluster.BootstrapInventory
	configPath := strings.TrimSpace(opts.contextPath)
	if configPath == "" {
		configPath, err = workstation.ConfigPath()
		if err != nil {
			return err
		}
	}
	contextName := strings.TrimSpace(opts.contextName)
	if contextName == "" {
		contextName = bundle.Manifest.ClusterName
	}
	clusterProfile := workstation.Cluster{Name: bundle.Manifest.ClusterName, ControlPlaneEndpoint: inv.ControlPlaneEndpoint}
	report := contextSaveReport{APIVersion: "katl.dev/v1alpha1", Kind: "ContextSaveReport", Context: contextName, ConfigPath: configPath}

	for _, node := range inv.Nodes {
		endpoint := net.JoinHostPort(strings.TrimSpace(node.Address), "9443")
		conn, err := dialKatlcAgent(ctx, endpoint)
		if err != nil {
			return fmt.Errorf("verify node %s management endpoint: %w", node.Name, err)
		}
		status, statusErr := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		closeErr := conn.Close()
		if statusErr != nil {
			return fmt.Errorf("verify node %s management endpoint: %w", node.Name, statusErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close node %s management endpoint: %w", node.Name, closeErr)
		}
		if strings.TrimSpace(status.GetMachineId()) == "" {
			return fmt.Errorf("verify node %s management endpoint: agent did not report a machine identity", node.Name)
		}
		clusterProfile.Nodes = append(clusterProfile.Nodes, workstation.Node{
			Name: node.Name, ManagementEndpoint: endpoint, SystemRole: node.SystemRole,
		})
		report.Nodes = append(report.Nodes, contextSaveNodeReport{Name: node.Name, ManagementEndpoint: endpoint, Connected: true})
	}

	cfg := workstation.Config{}
	if existing, loadErr := workstation.Load(configPath); loadErr == nil {
		cfg = existing
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}
	cfg = cfg.UpsertCluster(contextName, clusterProfile)
	if err := workstation.Save(configPath, cfg); err != nil {
		return err
	}
	if opts.output == "text" {
		_, err := fmt.Fprintf(stdout, "Saved context %s with %d node(s)\n", contextName, len(report.Nodes))
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode context save report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}
