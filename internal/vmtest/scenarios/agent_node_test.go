package scenarios

import "github.com/katl-dev/katl/internal/bootstrap/inventory"

// agentNode names a node reached over the agent API at address. An address that
// does not parse leaves the node's address unset, so the connection attempt
// fails instead of dialing a different host.
func agentNode(name, address string) inventory.PlannedNode {
	parsed, _ := inventory.ParseManagementAddress(address)
	return inventory.PlannedNode{Name: name, Address: parsed, Access: inventory.Access{Method: "agent"}}
}
