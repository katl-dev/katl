package main

import (
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/spf13/cobra"
)

type managementTargetOptions struct {
	configPath     string
	contextName    string
	nodeName       string
	endpoint       string
	agentTokenFile string
}

type managementTarget struct {
	nodeName string
	endpoint string
	token    string
}

func addManagementTargetFlags(cmd *cobra.Command, opts *managementTargetOptions) {
	cmd.Flags().StringVar(&opts.configPath, "config", "", "katlctl workstation config path")
	cmd.Flags().StringVar(&opts.contextName, "context", "", "katlctl context name")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node name in the selected context")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "explicit katlc agent endpoint host:port")
	cmd.Flags().StringVar(&opts.agentTokenFile, "agent-token-file", "", "explicit katlc agent bearer token file")
}

func resolveManagementTarget(opts managementTargetOptions) (managementTarget, error) {
	if endpoint := strings.TrimSpace(opts.endpoint); endpoint != "" {
		if strings.TrimSpace(opts.configPath) != "" || strings.TrimSpace(opts.contextName) != "" {
			return managementTarget{}, fmt.Errorf("--endpoint cannot be combined with --config or --context")
		}
		token, err := readAgentToken(opts.agentTokenFile)
		if err != nil {
			return managementTarget{}, err
		}
		return managementTarget{nodeName: strings.TrimSpace(opts.nodeName), endpoint: endpoint, token: token}, nil
	}

	topology, err := workstation.ResolveTopology(workstation.ResolveRequest{
		ConfigPath:  strings.TrimSpace(opts.configPath),
		ContextName: strings.TrimSpace(opts.contextName),
	})
	if err != nil {
		return managementTarget{}, err
	}
	nodeName := strings.TrimSpace(opts.nodeName)
	if nodeName == "" {
		if len(topology.Nodes) != 1 {
			return managementTarget{}, fmt.Errorf("--node is required because context %q contains %d nodes", topology.ContextName, len(topology.Nodes))
		}
		nodeName = topology.Nodes[0].Name
	}
	for _, node := range topology.Nodes {
		if node.Name != nodeName {
			continue
		}
		tokenPath := strings.TrimSpace(opts.agentTokenFile)
		if tokenPath == "" {
			ref := strings.TrimSpace(node.CredentialRef)
			var ok bool
			tokenPath, ok = strings.CutPrefix(ref, "file:")
			if !ok {
				return managementTarget{}, fmt.Errorf("node %q credentialRef must use file: for katlc management", nodeName)
			}
		}
		token, err := readAgentToken(tokenPath)
		if err != nil {
			return managementTarget{}, err
		}
		return managementTarget{nodeName: nodeName, endpoint: node.ManagementEndpoint, token: token}, nil
	}
	return managementTarget{}, fmt.Errorf("node %q was not found in context %q", nodeName, topology.ContextName)
}
