package transport

import (
	"fmt"
	"strings"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

// NodeIdentity binds one operation to an installation observed over an
// authenticated connection. Workstation inventory is not an observation.
type NodeIdentity struct {
	name       string
	enrollment string
	machine    string
}

func ObserveNode(name string, status *agentapi.NodeStatus) (NodeIdentity, error) {
	if status == nil {
		return NodeIdentity{}, fmt.Errorf("node %q did not return status", name)
	}
	if got := strings.TrimSpace(status.GetInventoryNodeName()); got != name {
		return NodeIdentity{}, fmt.Errorf("node %q address answered as enrolled node %q", name, got)
	}
	enrollment := strings.TrimSpace(status.GetEnrollmentId())
	machine := strings.TrimSpace(status.GetMachineId())
	if enrollment == "" || machine == "" {
		return NodeIdentity{}, fmt.Errorf("node %q did not report its installation identity", name)
	}
	return NodeIdentity{name: name, enrollment: enrollment, machine: machine}, nil
}

func (identity NodeIdentity) Verify(status *agentapi.NodeStatus) error {
	observed, err := ObserveNode(identity.name, status)
	if err != nil {
		return err
	}
	if observed != identity {
		return fmt.Errorf("node %q installation changed during the operation; inspect its state before retrying", identity.name)
	}
	return nil
}
