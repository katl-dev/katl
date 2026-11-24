package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/vmtest"
)

func TestMultihomeInterfaceFor(t *testing.T) {
	node := vmtest.Node{Name: "cp-1", Address: "10.200.4.11", MACAddress: "52:54:00:ab:cd:ef"}
	got, err := multihomeInterfaceFor(node, "10.201.9.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if want := (multihomeInterface{MACAddress: "52:54:01:ab:cd:ef", Address: "10.201.9.11"}); got != want {
		t.Fatalf("second NIC = %+v, want %+v", got, want)
	}

	if _, err := multihomeInterfaceFor(node, "10.201.0.0/16"); err == nil {
		t.Fatal("accepted a second network that is not a /24")
	}
}

func TestWorldMultihomeLifecycle(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	ctx := context.Background()
	world, err := manager.createWorld(ctx)
	if err != nil {
		t.Fatal(err)
	}
	node := mustAddWorldNode(t, world, true)
	if err := manager.syncNetwork(ctx, world); err != nil {
		t.Fatal(err)
	}
	domain := world.World.Libvirt.DomainPrefix + node.Name
	fake.domains[domain] = world.owner()
	fake.states[domain] = "running"
	if err := manager.writeState(node, installerState{
		APIVersion: installerStateAPIVersion, Kind: installerStateKind, RepoRoot: manager.repoRoot,
		World: world.World.RunID, Node: node.Name, DomainName: domain, LibvirtURI: world.World.Libvirt.URI,
	}); err != nil {
		t.Fatal(err)
	}

	if err := manager.setMultihome(ctx, node.Name); err != nil {
		t.Fatal(err)
	}
	state, err := manager.readMultihome()
	if err != nil {
		t.Fatal(err)
	}
	nic := state.Nodes[node.Name]
	network := fake.networks[multihomeNetworkName(world.World.RunID)]
	if network == nil || !network.active || strings.Contains(network.xml, "<forward") || !strings.Contains(network.xml, nic.MACAddress) {
		t.Fatalf("second network = %+v, want an active isolated network reserving %s", network, nic.MACAddress)
	}
	if !slices.Equal(fake.persistentNICs[domain], []string{nic.MACAddress}) || !slices.Equal(fake.liveNICs[domain], []string{nic.MACAddress}) {
		t.Fatalf("NICs persistent=%v live=%v, want %s in both", fake.persistentNICs[domain], fake.liveNICs[domain], nic.MACAddress)
	}

	t.Run("rerun converges", func(t *testing.T) {
		// An earlier attach reached the persistent definition only.
		fake.liveNICs[domain] = nil
		if err := manager.setMultihome(ctx, node.Name); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(fake.persistentNICs[domain], []string{nic.MACAddress}) || !slices.Equal(fake.liveNICs[domain], []string{nic.MACAddress}) {
			t.Fatalf("NICs persistent=%v live=%v, want %s once in each", fake.persistentNICs[domain], fake.liveNICs[domain], nic.MACAddress)
		}
	})

	t.Run("removal cleans up", func(t *testing.T) {
		if err := manager.removeMultihome(ctx, node.Name); err != nil {
			t.Fatal(err)
		}
		if len(fake.persistentNICs[domain]) != 0 || len(fake.liveNICs[domain]) != 0 {
			t.Fatalf("NICs after removal persistent=%v live=%v", fake.persistentNICs[domain], fake.liveNICs[domain])
		}
		if _, exists := fake.networks[multihomeNetworkName(world.World.RunID)]; exists {
			t.Fatal("second network survived removal of its last NIC")
		}
		if _, err := os.Stat(manager.multihomePath()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("multihome state after removal: %v", err)
		}
	})
}
