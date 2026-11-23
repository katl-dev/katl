package workstation

import (
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
)

func mustManagement(t *testing.T, value string) inventory.ManagementAddress {
	t.Helper()
	address, err := inventory.ParseManagementAddress(value)
	if err != nil {
		t.Fatalf("ParseManagementAddress(%q) error = %v", value, err)
	}
	return address
}

func mustHostPort(t *testing.T, value string) inventory.HostPort {
	t.Helper()
	endpoint, err := inventory.ParseHostPort(value)
	if err != nil {
		t.Fatalf("ParseHostPort(%q) error = %v", value, err)
	}
	return endpoint
}
