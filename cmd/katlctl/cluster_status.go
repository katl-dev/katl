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
	"sync"
	"text/tabwriter"
	"time"

	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/spf13/cobra"
)

type clusterStatusOptions struct {
	clusterConfig string
	contextFile   string
	contextName   string
	timeout       time.Duration
	output        string
}

type clusterNodeStatus struct {
	Node                 string                      `json:"node"`
	Role                 string                      `json:"role"`
	Endpoint             string                      `json:"endpoint"`
	Reachable            bool                        `json:"reachable"`
	Health               string                      `json:"health,omitempty"`
	KatlOSVersion        string                      `json:"katlosVersion,omitempty"`
	Generation           string                      `json:"generation,omitempty"`
	Activity             string                      `json:"activity,omitempty"`
	Error                string                      `json:"error,omitempty"`
	Kubernetes           *kubernetesStatusReport     `json:"kubernetes,omitempty"`
	ControlPlaneEndpoint *controlPlaneEndpointReport `json:"controlPlaneEndpoint,omitempty"`
}

type clusterStatusReport struct {
	Cluster                     string              `json:"cluster"`
	ControlPlaneEndpoint        string              `json:"controlPlaneEndpoint,omitempty"`
	StableEndpointReachable     bool                `json:"stableEndpointReachable"`
	StableEndpointFailureReason string              `json:"stableEndpointFailureReason,omitempty"`
	Nodes                       []clusterNodeStatus `json:"nodes"`
}

func newClusterStatusCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := clusterStatusOptions{timeout: 15 * time.Second, output: "text"}
	cmd := &cobra.Command{Use: "status", Short: "Show the state of every KatlOS node", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		_ = stderr
		return runClusterStatus(ctx, opts, stdout)
	}}
	cmd.Flags().StringVar(&opts.clusterConfig, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().StringVar(&opts.contextFile, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "optional saved context created by 'katlctl context save'")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "per-node management request timeout")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	return cmd
}

func resolveClusterTopology(opts clusterStatusOptions) (workstation.ResolvedTopology, error) {
	if strings.TrimSpace(opts.clusterConfig) != "" {
		if strings.TrimSpace(opts.contextFile) != "" || strings.TrimSpace(opts.contextName) != "" {
			return workstation.ResolvedTopology{}, fmt.Errorf("--config cannot be combined with --context or --context-file")
		}
		return resolveClusterConfigTopology(opts.clusterConfig)
	}
	resolved, err := workstation.ResolveTopology(workstation.ResolveRequest{ConfigPath: opts.contextFile, ContextName: opts.contextName})
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return workstation.ResolvedTopology{}, fmt.Errorf("no cluster source: use --config cluster.yaml; for shorter repeated commands, first run 'katlctl context save --config cluster.yaml'")
	}
	return resolved, err
}

func runClusterStatus(ctx context.Context, opts clusterStatusOptions, stdout io.Writer) error {
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	topology, err := resolveClusterTopology(opts)
	if err != nil {
		return err
	}
	report := clusterStatusReport{Cluster: topology.ClusterName, ControlPlaneEndpoint: topology.ControlPlaneEndpoint, Nodes: make([]clusterNodeStatus, len(topology.Nodes))}
	var wg sync.WaitGroup
	if topology.ControlPlaneEndpoint != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, opts.timeout)
			defer cancel()
			report.StableEndpointFailureReason = stableEndpointFailure(probeCtx, topology.ControlPlaneEndpoint)
			report.StableEndpointReachable = report.StableEndpointFailureReason == ""
		}()
	}
	for i, node := range topology.Nodes {
		wg.Add(1)
		go func(i int, node workstation.TopologyNode) {
			defer wg.Done()
			result := clusterNodeStatus{Node: node.Name, Role: string(node.SystemRole), Endpoint: node.ManagementEndpoint}
			requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
			defer cancel()
			conn, err := dialKatlcAgent(requestCtx, node.ManagementEndpoint)
			if err != nil {
				result.Error = err.Error()
				report.Nodes[i] = result
				return
			}
			defer conn.Close()
			status, current, err := readHostState(requestCtx, conn.Client, node.Name)
			if err != nil {
				result.Error = err.Error()
				report.Nodes[i] = result
				return
			}
			host := newHostStatusReport(node.Name, node.ManagementEndpoint, status, current)
			result.Reachable = true
			result.Health = host.Health
			result.KatlOSVersion = host.KatlOSVersion
			result.Generation = host.Generation
			result.Activity = host.Activity
			result.Kubernetes = host.Kubernetes
			result.ControlPlaneEndpoint = host.ControlPlaneEndpoint
			report.Nodes[i] = result
		}(i, node)
	}
	wg.Wait()
	sort.Slice(report.Nodes, func(i, j int) bool { return report.Nodes[i].Node < report.Nodes[j].Node })
	if opts.output == "json" {
		return json.NewEncoder(stdout).Encode(report)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if report.ControlPlaneEndpoint != "" {
		fmt.Fprintln(w, "CONTROL PLANE ENDPOINT\tREACHABLE")
		fmt.Fprintf(w, "%s\t%s\n", report.ControlPlaneEndpoint, yesNo(report.StableEndpointReachable))
		if report.StableEndpointFailureReason != "" {
			fmt.Fprintf(w, "\t%s\n", report.StableEndpointFailureReason)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "NODE\tROLE\tREACHABLE\tHEALTH\tKUBERNETES\tKATLOS\tGENERATION\tACTIVITY")
	for _, node := range report.Nodes {
		reachable := "no"
		health, kubernetes, version, generation, activity := "-", "-", "-", "-", "-"
		if node.Reachable {
			reachable = "yes"
			health, version, generation, activity = node.Health, node.KatlOSVersion, node.Generation, node.Activity
			if node.Kubernetes != nil {
				kubernetes = firstNonEmpty(strings.TrimSpace(node.Kubernetes.State), "unknown")
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", node.Node, node.Role, reachable, health, kubernetes, version, generation, activity)
		if node.Error != "" {
			fmt.Fprintf(w, "\t\t\t%s\n", node.Error)
		}
		if node.Kubernetes != nil && node.Kubernetes.FailureReason != "" {
			fmt.Fprintf(w, "\t\t\t\t%s\n", node.Kubernetes.FailureReason)
		}
		if node.ControlPlaneEndpoint != nil && node.ControlPlaneEndpoint.FailureReason != "" {
			fmt.Fprintf(w, "\t\t\tcontrol-plane endpoint: %s\n", node.ControlPlaneEndpoint.FailureReason)
		}
	}
	return w.Flush()
}

func stableEndpointFailure(ctx context.Context, endpoint string) string {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return err.Error()
	}
	_ = conn.Close()
	return ""
}
