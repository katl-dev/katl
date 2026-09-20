package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/bootstrap/kubeconfig"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newNodeLogsCommand(ctx context.Context, stdout io.Writer) *cobra.Command {
	var targetOptions managementTargetOptions
	request := agentapi.JournalRequest{Lines: 100, Boot: "0", Format: "text"}
	timeout := 30 * time.Minute
	cmd := &cobra.Command{
		Use: "logs [NODE]", Short: "Read or follow a node's system journal",
		Long:    "Read the native system journal through the management API. The current boot and last 100 entries are shown by default. JSON output is one journal object per line. Ctrl+C stops following without changing the node.",
		Example: "katlctl node logs cp-1 --config cluster.yaml --unit kubelet --follow\nkatlctl node logs cp-1 --config cluster.yaml --boot=-1 --lines 200",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := selectHostNode(&targetOptions.nodeName, args); err != nil {
				return err
			}
			if request.Lines < 0 || request.Lines > 10000 {
				return fmt.Errorf("--lines must be between 0 and 10000")
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			target, err := resolveManagementTarget(ctx, targetOptions)
			if err != nil {
				return err
			}
			ctx = withManagementTarget(ctx, target)
			conn, err := dialKatlcAgent(ctx, target.endpoint)
			if err != nil {
				return err
			}
			defer conn.Close()
			observed, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
			if err != nil {
				return err
			}
			if target.nodeName != "" {
				if err := bindManagementStatus(&target, observed); err != nil {
					return err
				}
			}
			stream, err := conn.Client.ReadJournal(ctx, &request)
			if err != nil {
				return diagnosticError("read journal", err)
			}
			for {
				entry, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					if errors.Is(ctx.Err(), context.Canceled) {
						return nil
					}
					return diagnosticError("read journal", err)
				}
				if _, err := fmt.Fprintln(stdout, entry.GetLine()); err != nil {
					return err
				}
			}
		},
	}
	addManagementTargetFlags(cmd, &targetOptions)
	cmd.Flags().StringArrayVarP(&request.Units, "unit", "u", nil, "systemd unit name or pattern; repeat for multiple units")
	cmd.Flags().Int32VarP(&request.Lines, "lines", "n", request.Lines, "number of recent entries (0–10000)")
	cmd.Flags().StringVar(&request.Boot, "boot", request.Boot, "boot offset (0=current, -1=previous) or boot ID")
	cmd.Flags().StringVar(&request.Since, "since", "", "journal time filter, for example '1 hour ago'")
	cmd.Flags().BoolVarP(&request.Follow, "follow", "f", false, "continue streaming new entries")
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "overall request deadline, including follow")
	addOutputFlag(cmd, &request.Format, request.Format, "text", "json")
	return cmd
}

func newClusterKubeconfigCommand(ctx context.Context, stdout io.Writer) *cobra.Command {
	opts := clusterStatusOptions{timeout: 30 * time.Second, output: "text"}
	var node, server, tlsName string
	var force bool
	cmd := &cobra.Command{
		Use: "kubeconfig [PATH]", Short: "Retrieve cluster admin access on this workstation",
		Long:    "Retrieve embedded admin credentials from a bootstrapped control-plane node and write a private kubeconfig (default: ./kubeconfig). Existing different content requires --force. This does not merge into ~/.kube/config or change any context. The configured control-plane endpoint is used unless --server overrides it.",
		Example: "katlctl cluster kubeconfig --config cluster.yaml\nkatlctl cluster kubeconfig ./lab.kubeconfig --config cluster.yaml --node cp-1",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(ctx, opts.timeout)
			defer cancel()
			resolved, err := resolveClusterTopology(ctx, opts)
			if err != nil {
				return err
			}
			selection := kubeconfig.EndpointSelection{ControlPlaneEndpoint: resolved.ControlPlaneEndpoint, ServerOverride: server, TLSServerName: tlsName}
			if tlsName != "" && server == "" {
				return fmt.Errorf("--tls-server-name requires --server")
			}
			if _, err := kubeconfig.SelectEndpoint(selection); err != nil {
				return err
			}
			path := "kubeconfig"
			if len(args) != 0 {
				path = args[0]
			}
			var failures []error
			for _, candidate := range resolved.Nodes {
				if node != "" && candidate.Name != node {
					continue
				}
				if candidate.SystemRole != inventory.RoleControlPlane {
					continue
				}
				target, err := targetFromTopology(resolved.Topology, candidate.Name)
				if err != nil {
					return err
				}
				credentials, err := readClusterCredentials(ctx, target)
				if err != nil {
					failures = append(failures, fmt.Errorf("%s: %w", candidate.Name, err))
					continue
				}
				result, err := kubeconfig.Write(kubeconfig.Request{
					Path: path, Overwrite: force, Endpoint: selection,
					ClusterName: resolved.ClusterName, ContextName: resolved.ClusterName,
					CertificateAuthorityData: credentials.CertificateAuthorityData,
					ClientCertificateData:    credentials.ClientCertificateData, ClientKeyData: credentials.ClientKeyData,
				})
				if errors.Is(err, kubeconfig.ErrExists) {
					return fmt.Errorf("%s already contains a different kubeconfig; choose another path or use --force", path)
				}
				if err != nil {
					return err
				}
				if opts.output == "json" {
					return writeJSON(stdout, struct {
						Path   string `json:"path"`
						Server string `json:"server"`
						Node   string `json:"node"`
					}{result.Path, result.Server, candidate.Name})
				}
				_, err = fmt.Fprintf(stdout, "Kubeconfig saved to %s (server %s).\n%s\n", result.Path, result.Server, result.NextStep())
				return err
			}
			if len(failures) == 0 {
				return fmt.Errorf("no matching control-plane node; choose --node from the control-plane nodes in your configuration")
			}
			return fmt.Errorf("retrieve kubeconfig: %w", errors.Join(failures...))
		},
	}
	cmd.Flags().StringVar(&opts.clusterConfig, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().StringVar(&opts.contextName, "context", "", "optional saved context")
	cmd.Flags().StringVar(&node, "node", "", "control-plane node to retrieve credentials from; default tries each")
	cmd.Flags().StringVar(&server, "server", "", "Kubernetes API endpoint override, for example https://192.0.2.10:6443")
	cmd.Flags().StringVar(&tlsName, "tls-server-name", "", "certificate server name when --server uses an alternate address")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing kubeconfig with different content")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "overall retrieval deadline")
	addOutputFlag(cmd, &opts.output, opts.output, "text", "json")
	return cmd
}

func readClusterCredentials(ctx context.Context, target managementTarget) (kubeconfig.Credentials, error) {
	// Bound each attempt so an unreachable node cannot consume the whole fallback budget.
	ctx, cancel := context.WithTimeout(withManagementTarget(ctx, target), 10*time.Second)
	defer cancel()
	conn, err := dialKatlcAgent(ctx, target.endpoint)
	if err != nil {
		return kubeconfig.Credentials{}, err
	}
	defer conn.Close()
	observed, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return kubeconfig.Credentials{}, err
	}
	if err := bindManagementStatus(&target, observed); err != nil {
		return kubeconfig.Credentials{}, err
	}
	response, err := conn.Client.GetKubeconfig(ctx, &agentapi.GetKubeconfigRequest{})
	if err != nil {
		return kubeconfig.Credentials{}, diagnosticError("retrieve kubeconfig", err)
	}
	return kubeconfig.ParseCredentials(response.GetKubeconfig())
}

func diagnosticError(action string, err error) error {
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("%s: this KatlOS version does not support the command; upgrade the node", action)
	}
	return fmt.Errorf("%s: %w", action, err)
}
