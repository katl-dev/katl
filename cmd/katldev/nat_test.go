package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeNATTable models the iptables nat table well enough to observe the
// rules katldev leaves behind, rather than the commands it ran.
type fakeNATTable struct {
	chains map[string][]string
}

func (table *fakeNATTable) run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) < 4 || args[0] != "-w" || args[1] != "-t" || args[2] != "nat" {
		return nil, errors.New("unexpected iptables invocation " + strings.Join(args, " "))
	}
	if len(args) == 4 && args[3] == "-S" {
		var listing strings.Builder
		for chain, rules := range table.chains {
			for _, rule := range rules {
				listing.WriteString("-A " + chain + " " + rule + "\n")
			}
		}
		return []byte(listing.String()), nil
	}
	op, chain, rule := args[3], args[4], strings.Join(args[5:], " ")
	rules, exists := table.chains[chain]
	if chain == "OUTPUT" {
		exists = true
	}
	switch op {
	case "-S":
		if !exists {
			return nil, errors.New("no chain")
		}
	case "-N":
		table.chains[chain] = nil
	case "-F":
		table.chains[chain] = nil
	case "-X":
		delete(table.chains, chain)
	case "-A":
		table.chains[chain] = append(rules, rule)
	case "-C", "-D":
		for i, existing := range rules {
			if existing == rule {
				if op == "-D" {
					table.chains[chain] = append(rules[:i:i], rules[i+1:]...)
				}
				return nil, nil
			}
		}
		return nil, errors.New("no rule")
	}
	return nil, nil
}

func TestWorldNATTranslatesToReservedAddress(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	table := &fakeNATTable{chains: map[string][]string{}}
	manager.runIptables = table.run
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cp1 := mustAddWorldNode(t, world, true)
	if err := manager.syncNetwork(context.Background(), world); err != nil {
		t.Fatal(err)
	}
	if len(table.chains) != 0 {
		t.Fatalf("a world without NAT addresses changed the nat table: %v", table.chains)
	}

	if err := manager.setNAT(context.Background(), "cp-1", "198.18.0.10"); err != nil {
		t.Fatal(err)
	}
	chain := natChain(world.World.RunID)
	want := map[string][]string{
		chain:    {"-d 198.18.0.10/32 -j DNAT --to-destination " + cp1.Address},
		"OUTPUT": {"-j " + chain},
	}
	if !reflect.DeepEqual(table.chains, want) {
		t.Fatalf("nat table = %v, want %v", table.chains, want)
	}

	t.Run("follows a readdress", func(t *testing.T) {
		moved, err := world.Scenario.ReassignAddress("cp-1", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.syncNetwork(context.Background(), world); err != nil {
			t.Fatal(err)
		}
		want := map[string][]string{
			chain:    {"-d 198.18.0.10/32 -j DNAT --to-destination " + moved.Address},
			"OUTPUT": {"-j " + chain},
		}
		if !reflect.DeepEqual(table.chains, want) {
			t.Fatalf("nat table = %v, want %v", table.chains, want)
		}
	})

	t.Run("removal clears the table", func(t *testing.T) {
		if err := manager.removeNAT(context.Background(), "cp-1"); err != nil {
			t.Fatal(err)
		}
		if len(table.chains) != 1 || len(table.chains["OUTPUT"]) != 0 {
			t.Fatalf("nat table after removal = %v", table.chains)
		}
		if _, err := os.Stat(manager.natPath()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("nat state after removal: %v", err)
		}
	})
}

func TestWorldNATRejectsAddressesAVMCouldHold(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	manager.runIptables = (&fakeNATTable{chains: map[string][]string{}}).run
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustAddWorldNode(t, world, true)

	inside := strings.TrimSuffix(world.World.Network.Gateway, ".1") + ".250"
	for _, address := range []string{inside, "127.0.0.1", "not-an-ip"} {
		if err := manager.setNAT(context.Background(), "cp-1", address); err == nil {
			t.Errorf("setNAT(%s) succeeded", address)
		}
	}
}

func TestWorldNATRejectsAddressAnotherWorldTranslates(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	table := &fakeNATTable{chains: map[string][]string{
		"KATLDEV-other": {"-d 198.18.0.10/32 -j DNAT --to-destination 10.205.108.2"},
		"OUTPUT":        {"-j KATLDEV-other"},
	}}
	manager.runIptables = table.run
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustAddWorldNode(t, world, true)

	if err := manager.setNAT(context.Background(), "cp-1", "198.18.0.10"); err == nil || !strings.Contains(err.Error(), "KATLDEV-other") {
		t.Fatalf("setNAT() with another world's address error = %v", err)
	}
	if err := manager.setNAT(context.Background(), "cp-1", "198.18.0.11"); err != nil {
		t.Fatalf("setNAT() with a free address: %v", err)
	}
	// Repeating the world's own address is not a conflict.
	if err := manager.setNAT(context.Background(), "cp-1", "198.18.0.11"); err != nil {
		t.Fatalf("setNAT() repeated: %v", err)
	}
}

func TestMatchingConfigAcceptsNATNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	writeKatldevClusterConfig(t, path, "198.18.0.10")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(data), "        address: 198.18.0.10\n", "        address: 198.18.0.10\n      kubernetes:\n        address: 10.200.0.2\n", 1)
	if source == string(data) {
		t.Fatal("fixture has no management address to extend")
	}
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, err := matchingKatldevConfig(path, "10.200.0.2"); err != nil || name != "cp-1" {
		t.Fatalf("match = %q, %v", name, err)
	}
}
