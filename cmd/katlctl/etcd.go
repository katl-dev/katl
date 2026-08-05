package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

type etcdStatusClient interface {
	GetEtcdStatus(context.Context, *agentapi.GetEtcdStatusRequest, ...grpc.CallOption) (*agentapi.EtcdStatus, error)
}

type etcdOptions struct {
	configPath  string
	coordinator string
	output      string
}

type etcdRemoveOptions struct {
	etcdOptions
	targetNode string
	memberID   string
	timeout    time.Duration
}

type etcdRemovalPlan struct {
	Coordinator inventory.PlannedNode
	Target      inventory.PlannedNode
	Status      *agentapi.EtcdStatus
	Member      *agentapi.EtcdMember
}

func newEtcdCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "etcd", Short: "Inspect and maintain stacked etcd membership"}
	statusOpts := etcdOptions{output: "text"}
	members := &cobra.Command{
		Use:   "members",
		Short: "Show stacked etcd members and endpoint health",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return runEtcdMembers(ctx, statusOpts, stdout, stderr)
		},
	}
	members.Flags().StringVar(&statusOpts.configPath, "config", "", "ClusterConfig YAML or Katl config bundle")
	members.Flags().StringVar(&statusOpts.coordinator, "coordinator", "", "healthy control-plane node used for inspection")
	members.Flags().StringVarP(&statusOpts.output, "output", "o", "text", "output format: text or json")
	cmd.AddCommand(members)

	removeOpts := etcdRemoveOptions{etcdOptions: etcdOptions{output: "text"}, timeout: 5 * time.Minute}
	remove := &cobra.Command{
		Use:   "remove NODE",
		Short: "Remove one explicitly identified stale etcd member",
		Long:  "Remove one stale stacked-etcd member after validating cluster identity, node name, peer URL, current membership, and quorum safety on a surviving control plane.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			removeOpts.targetNode = args[0]
			return runEtcdRemove(ctx, removeOpts, stdout, stderr)
		},
	}
	remove.Flags().StringVar(&removeOpts.configPath, "config", "", "ClusterConfig YAML or Katl config bundle")
	remove.Flags().StringVar(&removeOpts.coordinator, "coordinator", "", "healthy surviving control-plane node")
	remove.Flags().StringVar(&removeOpts.memberID, "member-id", "", "observed hexadecimal member ID to remove")
	remove.Flags().DurationVar(&removeOpts.timeout, "timeout", removeOpts.timeout, "operation wait timeout")
	remove.Flags().StringVarP(&removeOpts.output, "output", "o", "text", "output format: text or json")
	cmd.AddCommand(remove)
	_ = stderr
	return cmd
}

func runEtcdMembers(ctx context.Context, opts etcdOptions, stdout, stderr io.Writer) error {
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	inv, err := loadWipeInventory(opts.configPath, "", stderr)
	if err != nil {
		return err
	}
	coordinator, err := selectEtcdCoordinator(inv, opts.coordinator, "")
	if err != nil {
		return err
	}
	status, err := getEtcdStatus(ctx, coordinator)
	if err != nil {
		return err
	}
	return printEtcdStatus(stdout, coordinator.Name, status, opts.output)
}

func runEtcdRemove(ctx context.Context, opts etcdRemoveOptions, stdout, stderr io.Writer) error {
	if strings.TrimSpace(opts.memberID) == "" {
		return fmt.Errorf("--member-id is required; run 'katlctl cluster etcd members' and confirm the stale member identity")
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	inv, err := loadWipeInventory(opts.configPath, "", stderr)
	if err != nil {
		return err
	}
	plan, err := planEtcdRemoval(ctx, inv, opts.targetNode, opts.coordinator, opts.memberID)
	if err != nil {
		return err
	}
	if err := submitEtcdRemoval(ctx, plan, opts.timeout, stderr, "katlctl cluster etcd remove"); err != nil {
		return err
	}
	post, err := getEtcdStatus(ctx, plan.Coordinator)
	if err != nil {
		return err
	}
	return printEtcdStatus(stdout, plan.Coordinator.Name, post, opts.output)
}

func planEtcdRemoval(ctx context.Context, inv inventory.Inventory, targetName, coordinatorName, confirmedMemberID string) (etcdRemovalPlan, error) {
	plan, err := planWipeInventory(inv)
	if err != nil {
		return etcdRemovalPlan{}, err
	}
	var target inventory.PlannedNode
	for _, node := range plan.Nodes {
		if node.Name == strings.TrimSpace(targetName) {
			target = node
			break
		}
	}
	if target.Name == "" {
		return etcdRemovalPlan{}, fmt.Errorf("node %q is not in the cluster config", targetName)
	}
	if target.SystemRole != inventory.RoleControlPlane {
		return etcdRemovalPlan{}, fmt.Errorf("node %q is not a control-plane node", target.Name)
	}
	coordinator, err := selectEtcdCoordinator(inv, coordinatorName, target.Name)
	if err != nil {
		return etcdRemovalPlan{}, err
	}
	status, err := getEtcdStatus(ctx, coordinator)
	if err != nil {
		return etcdRemovalPlan{}, err
	}
	expectedPeer := etcdPeerURL(target.Address)
	var member *agentapi.EtcdMember
	for _, candidate := range status.GetMembers() {
		if candidate.GetName() == target.Name && containsString(candidate.GetPeerUrls(), expectedPeer) {
			member = candidate
			break
		}
	}
	if member == nil {
		return etcdRemovalPlan{}, fmt.Errorf("etcd member for %s with peer URL %s is not present", target.Name, expectedPeer)
	}
	if strings.TrimSpace(confirmedMemberID) != "" && !strings.EqualFold(strings.TrimSpace(confirmedMemberID), member.GetId()) {
		return etcdRemovalPlan{}, fmt.Errorf("--member-id %s does not match current member %s for %s", confirmedMemberID, member.GetId(), target.Name)
	}
	if member.GetId() == status.GetLocalMemberId() {
		return etcdRemovalPlan{}, fmt.Errorf("coordinator %s cannot remove its own member; choose another healthy control plane", coordinator.Name)
	}
	if status.GetHealthyMembers() < status.GetQuorum() {
		return etcdRemovalPlan{}, fmt.Errorf("etcd has %d healthy members, below quorum %d", status.GetHealthyMembers(), status.GetQuorum())
	}
	healthySurvivors := status.GetHealthyMembers()
	if member.GetHealthy() {
		healthySurvivors--
	}
	postQuorum := uint32((len(status.GetMembers())-1)/2 + 1)
	if healthySurvivors < postQuorum {
		return etcdRemovalPlan{}, fmt.Errorf("removal would leave %d healthy members, below resulting quorum %d", healthySurvivors, postQuorum)
	}
	return etcdRemovalPlan{Coordinator: coordinator, Target: target, Status: status, Member: member}, nil
}

func selectEtcdCoordinator(inv inventory.Inventory, requested, excluded string) (inventory.PlannedNode, error) {
	plan, err := planWipeInventory(inv)
	if err != nil {
		return inventory.PlannedNode{}, err
	}
	var candidates []inventory.PlannedNode
	for _, node := range plan.Nodes {
		if node.SystemRole == inventory.RoleControlPlane && node.Name != excluded {
			candidates = append(candidates, node)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	if strings.TrimSpace(requested) != "" {
		for _, node := range candidates {
			if node.Name == strings.TrimSpace(requested) {
				return node, nil
			}
		}
		return inventory.PlannedNode{}, fmt.Errorf("coordinator %q is not a surviving control-plane node", requested)
	}
	if len(candidates) == 0 {
		return inventory.PlannedNode{}, fmt.Errorf("no surviving control-plane node can coordinate etcd maintenance")
	}
	return candidates[0], nil
}

func getEtcdStatus(ctx context.Context, coordinator inventory.PlannedNode) (*agentapi.EtcdStatus, error) {
	connector := newWipeClusterConnector()
	conn, err := connector.Connect(ctx, coordinator)
	if err != nil {
		return nil, fmt.Errorf("connect etcd coordinator %s: %w", coordinator.Name, err)
	}
	defer closeAgentConnection(conn)
	client, ok := conn.Client.(etcdStatusClient)
	if !ok {
		return nil, fmt.Errorf("node %s agent does not support etcd maintenance", coordinator.Name)
	}
	status, err := client.GetEtcdStatus(ctx, &agentapi.GetEtcdStatusRequest{})
	if err != nil {
		return nil, fmt.Errorf("inspect etcd through %s: %w", coordinator.Name, err)
	}
	if status.GetClusterId() == "" || len(status.GetMembers()) == 0 {
		return nil, fmt.Errorf("node %s returned incomplete etcd status", coordinator.Name)
	}
	return status, nil
}

func submitEtcdRemoval(ctx context.Context, plan etcdRemovalPlan, timeout time.Duration, progress io.Writer, actor string) error {
	connector := newWipeClusterConnector()
	conn, err := connector.Connect(ctx, plan.Coordinator)
	if err != nil {
		return fmt.Errorf("connect etcd coordinator %s: %w", plan.Coordinator.Name, err)
	}
	defer closeAgentConnection(conn)
	nodeStatus, err := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return fmt.Errorf("status etcd coordinator %s: %w", plan.Coordinator.Name, err)
	}
	requestID, err := clientRequestID("")
	if err != nil {
		return err
	}
	accepted, err := conn.Client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
		ApiVersion:        operation.APIVersion,
		Kind:              "SubmitOperationRequest",
		ClientRequestId:   requestID,
		OperationKind:     "etcd-member-remove",
		Actor:             actor,
		ExpectedMachineId: nodeStatus.GetMachineId(),
		OperationTimeout:  timeout.String(),
		EtcdMemberRemove: &agentapi.EtcdMemberRemoveOperationRequest{
			TargetNodeName:      plan.Target.Name,
			TargetMemberId:      plan.Member.GetId(),
			TargetPeerUrl:       etcdPeerURL(plan.Target.Address),
			ExpectedClusterId:   plan.Status.GetClusterId(),
			ExpectedMemberCount: uint32(len(plan.Status.GetMembers())),
		},
	})
	if err != nil {
		return fmt.Errorf("submit etcd member removal on %s: %w", plan.Coordinator.Name, err)
	}
	if progress != nil {
		fmt.Fprintf(progress, "etcd member removal node=%s member=%s coordinator=%s status=accepted\n", plan.Target.Name, plan.Member.GetId(), plan.Coordinator.Name)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	current := accepted.GetInitialStatus()
	for {
		if current == nil || !current.GetTerminal() {
			current, err = conn.Client.GetOperation(waitCtx, &agentapi.GetOperationRequest{OperationId: accepted.GetOperationId(), ExpectedRequestDigest: accepted.GetRequestDigest()})
			if err != nil {
				return fmt.Errorf("wait for etcd member removal: %w", err)
			}
		}
		if current.GetTerminal() {
			if current.GetResult() != operation.ResultSucceeded {
				return fmt.Errorf("etcd member removal stopped at %s: %s", current.GetPhase(), current.GetFailureReason())
			}
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait for etcd member removal: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func etcdPeerURL(address string) string {
	host := strings.TrimSpace(address)
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	return "https://" + net.JoinHostPort(host, "2380")
}

func printEtcdStatus(w io.Writer, coordinator string, status *agentapi.EtcdStatus, output string) error {
	if output == "json" {
		return json.NewEncoder(w).Encode(map[string]any{"coordinator": coordinator, "etcd": status})
	}
	fmt.Fprintf(w, "etcd cluster=%s coordinator=%s healthy=%d/%d quorum=%d\n", status.GetClusterId(), coordinator, status.GetHealthyMembers(), len(status.GetMembers()), status.GetQuorum())
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "NODE\tMEMBER\tPEER\tHEALTH\tLEADER")
	for _, member := range status.GetMembers() {
		health := "unhealthy"
		if member.GetHealthy() {
			health = "healthy"
		}
		peer := ""
		if len(member.GetPeerUrls()) > 0 {
			peer = member.GetPeerUrls()[0]
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%t\n", member.GetName(), member.GetId(), peer, health, member.GetLeader())
	}
	return table.Flush()
}
