package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

// This file owns the conclusions a command may draw about a node's address:
// whether it is bound to the node, whether the node answers there, and whether
// the node moved. See docs/internal/management-address-ownership.md.
//
// The stages build on each other. A candidate is the address a command will
// use; only checkAddress turns it into a verified address; only
// verified.move builds a move. A policy that refuses a move takes a move, so it
// cannot run before the node has answered at the address given.

// addressSource says where a candidate's endpoint came from.
type addressSource uint8

const (
	// sourceClusterConfig is the node's ClusterConfig management.address.
	sourceClusterConfig addressSource = iota + 1
	// sourceFlag is an address given with --endpoint.
	sourceFlag
	// sourceConfirmed is an endpoint this command confirmed earlier in the
	// same run.
	sourceConfirmed
)

// savedNode is what the saved context records for one node.
type savedNode struct {
	endpoint, configured    inventory.HostPort
	enrollmentID, machineID string
}

// candidate is the address a command is about to use for one node.
type candidate struct {
	node string
	role inventory.SystemRole
	// endpoint is where the command dials; configured is the node's
	// management.address.
	endpoint, configured inventory.HostPort
	source               addressSource
	saved                savedNode
}

// resolveCandidate binds the address a command uses for node. override
// replaces the configured address when it is not zero, and source then says
// where it came from. saved is the saved context as read from disk.
func resolveCandidate(saved workstation.Config, cluster string, node inventory.Node, override inventory.HostPort, source addressSource) candidate {
	c := candidate{node: node.Name, role: node.SystemRole, endpoint: node.ManagementEndpoint(), configured: node.ManagementEndpoint(), source: sourceClusterConfig}
	if !override.IsZero() {
		c.endpoint, c.source = override, source
	}
	for _, profile := range saved.Clusters {
		if strings.TrimSpace(profile.Name) != strings.TrimSpace(cluster) {
			continue
		}
		for _, recorded := range profile.Nodes {
			if recorded.Name != node.Name {
				continue
			}
			// An unreadable record binds nothing.
			c.saved.endpoint, _ = inventory.DecodeHostPort(recorded.ManagementEndpoint)
			c.saved.configured, _ = inventory.DecodeHostPort(recorded.ConfiguredEndpoint)
			c.saved.enrollmentID = strings.TrimSpace(recorded.EnrollmentID)
			c.saved.machineID = strings.TrimSpace(recorded.MachineID)
		}
	}
	return c
}

// bound reports whether an earlier verified observation bound the candidate's
// endpoint to the node. Every --endpoint is unverified, even one equal to the
// saved endpoint (#1112 decides that case).
func (c candidate) bound() bool {
	switch c.source {
	case sourceConfirmed:
		return true
	case sourceFlag:
		return false
	default:
		return !c.saved.endpoint.IsZero() && c.saved.endpoint.Equal(c.endpoint)
	}
}

func (c candidate) binding() managementAddress {
	if c.bound() {
		return addressBound
	}
	return addressUnverified
}

// addressStep is what the checking command offers when the check fails.
type addressStep struct {
	claim string
	// rerun repeats the check, if the reader can rerun it.
	rerun   string
	timeout time.Duration
	// authorityRemedy replaces transport.ManagementAuthorityRemedy.
	authorityRemedy string
	// prior is the trust a trust-changing command may have left on the node;
	// zero for a check that changed nothing.
	prior managementTrust
	// configPath is the ClusterConfig that hints name.
	configPath string
}

// configuredCredentialsStep is the step of a read with the ClusterConfig's
// management credentials.
func configuredCredentialsStep(configPath, rerun string, timeout time.Duration) addressStep {
	return addressStep{claim: "the node answers with the configured management credentials", rerun: rerun, timeout: timeout, configPath: configPath}
}

// statusObserver reads a node's status at endpoint.
type statusObserver func(context.Context, inventory.HostPort) (*agentapi.NodeStatus, error)

// verified is a candidate whose endpoint returned the node's status over the
// configured credentials and reported the node's identity.
type verified struct {
	candidate candidate
	identity  transport.NodeIdentity
	status    *agentapi.NodeStatus
}

// checkAddress reads the node's status at the candidate's endpoint and
// confirms the node answered. Its errors are the only explanation of a failed
// check, and they never conclude that the node moved.
func checkAddress(ctx context.Context, c candidate, observe statusObserver, step addressStep) (verified, error) {
	if step.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, step.timeout)
		defer cancel()
	}
	status, err := observe(ctx, c.endpoint)
	if err != nil {
		return verified{}, c.failure(step, err)
	}
	check := c.managementCheck(step)
	answered := fmt.Sprintf("check that %s is %s's management address", c.endpoint, c.node)
	if got := strings.TrimSpace(status.GetInventoryNodeName()); got != c.node {
		return verified{}, fmt.Errorf("verify node %s management endpoint %s: the endpoint answered as node %q, not %s; %s", c.node, c.endpoint, got, c.node, check.thenRerun(answered))
	}
	// A ClusterConfig address may reach a reinstalled node, which context
	// save records. An --endpoint is bound to the node only by the
	// installation the saved context records.
	if c.source == sourceFlag && c.saved.enrollmentID != "" && (c.saved.enrollmentID != strings.TrimSpace(status.GetEnrollmentId()) || (c.saved.machineID != "" && c.saved.machineID != strings.TrimSpace(status.GetMachineId()))) {
		return verified{}, fmt.Errorf("verify node %s management endpoint %s: the endpoint answered as a different KatlOS installation than the saved context records for %s; %s", c.node, c.endpoint, c.node, check.thenRerun(answered))
	}
	identity, err := transport.ObserveNode(c.node, status)
	if err != nil {
		return verified{}, c.failure(step, err)
	}
	return verified{candidate: c, identity: identity, status: status}, nil
}

func (c candidate) managementCheck(step addressStep) managementCheck {
	return managementCheck{claim: step.claim, address: c.binding(), endpoint: c.endpoint, rerun: step.rerun, timeout: step.timeout, authorityRemedy: step.authorityRemedy}
}

func (c candidate) failure(step addressStep, err error) error {
	check := c.managementCheck(step)
	explained := managementReadError(check, err)
	if step.prior != 0 {
		explained = managementTrustChangeError(check, step.prior, err)
	}
	return fmt.Errorf("verify node %s management endpoint %s: %w%s", c.node, c.endpoint, explained, c.hint(err, check.address, step.configPath))
}

// hint is the clause that follows a failed check, if any.
func (c candidate) hint(err error, binding managementAddress, configPath string) string {
	// The saved context reached the node at its configured address, so that
	// address is a verified one the reader can use instead.
	if c.source == sourceFlag && transport.ClassifyManagementEvidence(err, nil, nil) == transport.ManagementUnreachable &&
		!c.configured.IsZero() && c.saved.endpoint.Equal(c.configured) && !c.configured.Equal(c.endpoint) {
		return fmt.Sprintf("; the saved context and %s both place %s at %s, so omit --endpoint to use it", configPath, c.node, managementHost(c.configured))
	}
	switch {
	case !addressHintApplies(err, binding):
	case c.source == sourceClusterConfig && c.role == inventory.RoleWorker:
		return "; " + readdressHint(c.node, c.endpoint, configPath, c.saved.endpoint)
	case c.source != sourceClusterConfig && !c.configured.IsZero() && !c.configured.Equal(c.endpoint):
		return "; " + movedHint(c.node, c.configured, configPath)
	}
	return ""
}

// checkSavedEndpoints checks each node in endpoints with the credentials the
// saved context holds for it, as context rebind does, for a configuration
// that names no secrets of its own.
func checkSavedEndpoints(ctx context.Context, configPath, clusterName string, endpoints map[string]inventory.HostPort) (map[string]verified, error) {
	if len(endpoints) == 0 {
		return nil, nil
	}
	inv, err := readManagementInventory(configPath)
	if err != nil {
		return nil, err
	}
	saved := savedContextOnDisk()
	checked := make(map[string]verified, len(endpoints))
	for _, node := range inv.Nodes {
		endpoint, ok := endpoints[node.Name]
		if !ok {
			continue
		}
		nodeCtx, err := managementContextForNode(ctx, configPath, node.Name)
		if err != nil {
			return nil, err
		}
		c := resolveCandidate(saved, clusterName, node, endpoint, sourceFlag)
		v, err := checkAddress(nodeCtx, c, contextEndpointStatus, configuredCredentialsStep(configPath, "the same command", defaultManagementTimeout))
		if err != nil {
			return nil, err
		}
		checked[node.Name] = v
	}
	return checked, nil
}

// savedContextOnDisk reads the saved context from its file. Commands that
// observe nodes replace it in memory with their own observations, which bind
// nothing.
func savedContextOnDisk() workstation.Config {
	path, err := workstation.ConfigPath()
	if err != nil {
		return workstation.Config{}
	}
	return savedContextAt(path)
}

func savedContextAt(path string) workstation.Config {
	saved, err := workstation.Load(path)
	if err != nil {
		return workstation.Config{}
	}
	return saved
}

// move is a verified node whose configuration places it on another host.
type move struct {
	from verified
	to   inventory.HostPort
}

// move returns the move to the node's management.address. Ports are not
// compared: a forwarded port on the same host is the same node address.
func (v verified) move() (move, bool) {
	to := v.candidate.configured
	if to.IsZero() || to.Host.Equal(v.candidate.endpoint.Host) {
		return move{}, false
	}
	return move{from: v, to: to}, true
}

// moveRefusal names a policy that refuses a move.
type moveRefusal uint8

const (
	// refuseControlPlane: Katl cannot change a control plane's address.
	refuseControlPlane moveRefusal = iota + 1
	// refuseLiveMove: a move needs a reboot, which --mode live excludes.
	refuseLiveMove
)

func (m move) refuse(reason moveRefusal, configPath string) error {
	node := m.from.candidate.node
	from, to := m.from.candidate.endpoint.Host.String(), m.to.Host.String()
	switch reason {
	case refuseControlPlane:
		return fmt.Errorf("refusing to move control-plane node %s from %s to %s: %s answers at %s, and Katl cannot change a control plane's advertise address, etcd peer URLs, serving certificate SANs, or a control-plane endpoint derived from its address; restore management.address to %s, or plan 'katlctl node wipe %s --config %s --plan', reinstall it with the new address, and join it with 'katlctl node join %s --config %s'; no node was changed", node, from, to, node, managementHost(m.from.candidate.endpoint), from, node, configPath, node, configPath)
	case refuseLiveMove:
		return fmt.Errorf("moving %s from %s to %s requires a reboot; omit --mode live so apply stages the change for the next boot", node, from, to)
	default:
		panic(fmt.Sprintf("unknown move refusal %d", uint8(reason)))
	}
}
