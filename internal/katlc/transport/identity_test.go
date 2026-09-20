package transport

import (
	"testing"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestNodeIdentityBindsOneInstallation(t *testing.T) {
	status := &agentapi.NodeStatus{InventoryNodeName: "cp-1", EnrollmentId: "install-2", MachineId: "machine-2"}
	identity, err := ObserveNode("cp-1", status)
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.Verify(status); err != nil {
		t.Fatal(err)
	}

	for _, changed := range []*agentapi.NodeStatus{
		{InventoryNodeName: "cp-2", EnrollmentId: "install-2", MachineId: "machine-2"},
		{InventoryNodeName: "cp-1", EnrollmentId: "install-3", MachineId: "machine-2"},
		{InventoryNodeName: "cp-1", EnrollmentId: "install-2", MachineId: "machine-3"},
		{InventoryNodeName: "cp-1", MachineId: "machine-2"},
		nil,
	} {
		if err := identity.Verify(changed); err == nil {
			t.Fatalf("accepted changed or absent installation: %v", changed)
		}
	}
}
