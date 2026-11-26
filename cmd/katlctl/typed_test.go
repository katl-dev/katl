package main

import (
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
)

func mustManagement(t testing.TB, value string) inventory.ManagementAddress {
	t.Helper()
	address, err := inventory.ParseManagementAddress(value)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

func mustHostPort(t testing.TB, value string) inventory.HostPort {
	t.Helper()
	endpoint, err := inventory.ParseHostPort(value)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}
