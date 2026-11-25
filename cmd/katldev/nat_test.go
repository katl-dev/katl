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
	// denied makes every call fail as sudo does without a password.
	denied bool
}

func (table *fakeNATTable) run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) < 4 || args[0] != "-w" || args[1] != "-t" || args[2] != "nat" {
		return nil, errors.New("unexpected iptables invocation " + strings.Join(args, " "))
	}
	if table.denied {
		return nil, errors.New("sudo: a password is required")
	}
	if len(args) == 4 && args[3] == "-S" {
		var listing strings.Builder
		listing.WriteString("-P OUTPUT ACCEPT\n")
		for chain := range table.chains {
			if chain != "OUTPUT" {
				listing.WriteString("-N " + chain + "\n")
			}
		}
		for chain, rules := range table.chains {
			for _, rule := range rules {
				listing.WriteString("-A " + chain + " " + rule + "\n")
			}
		}
		return []byte(listing.String()), nil
	}
	op, chain, rule := args[3], args[4], strings.Join(args[5:], " ")
	rules := table.chains[chain]
	switch op {
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

func natFixture(t *testing.T) (installerManager, *fakeNATTable, katldevWorld) {
	t.Helper()
	manager := testManager(t, newFakeLibvirt(t))
	table := &fakeNATTable{chains: map[string][]string{}}
	manager.runIptables = table.run
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustAddWorldNode(t, world, true)
	return manager, table, world
}

func TestWorldNATRemovalKeepsRecordWhenHostRuleStays(t *testing.T) {
	manager, table, world := natFixture(t)
	ctx := context.Background()
	if err := manager.setNAT(ctx, "cp-1", "198.18.0.10"); err != nil {
		t.Fatal(err)
	}
	table.denied = true

	if err := manager.removeNAT(ctx, "cp-1"); err == nil {
		t.Fatal("removeNAT() succeeded though iptables was unusable")
	}
	if nat, err := manager.readNAT(); err != nil || nat["cp-1"] != "198.18.0.10" {
		t.Fatalf("nat record after failed removal = %v, %v", nat, err)
	}
	table.denied = false
	if len(table.chains[natChain(world.World.RunID)]) != 1 {
		t.Fatalf("host rule changed by a failed removal: %v", table.chains)
	}

	if err := manager.removeNAT(ctx, "cp-1"); err != nil {
		t.Fatalf("retried removeNAT(): %v", err)
	}
	if len(table.chains) != 1 || len(table.chains["OUTPUT"]) != 0 {
		t.Fatalf("nat table after retried removal = %v", table.chains)
	}
}

func TestWorldDestroyKeepsNATRecordWhenChainStays(t *testing.T) {
	manager, table, world := natFixture(t)
	ctx := context.Background()
	if err := manager.setNAT(ctx, "cp-1", "198.18.0.10"); err != nil {
		t.Fatal(err)
	}
	table.denied = true

	if err := manager.removeWorldObjects(ctx, world.World.RunID); err == nil {
		t.Fatal("removeWorldObjects() succeeded though the chain could not be read")
	}
	if _, err := os.Stat(manager.natPath()); err != nil {
		t.Fatalf("nat record dropped while the host rule stays: %v", err)
	}

	table.denied = false
	if err := manager.removeWorldObjects(ctx, world.World.RunID); err != nil {
		t.Fatal(err)
	}
	if len(table.chains) != 1 || len(table.chains["OUTPUT"]) != 0 {
		t.Fatalf("nat table after destroy = %v", table.chains)
	}
	if _, err := os.Stat(manager.natPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nat record after destroy: %v", err)
	}
}

func TestWorldNATRemovesChainsOfEarlierWorlds(t *testing.T) {
	manager, table, world := natFixture(t)
	// No libvirt network belongs to the earlier world, so its chain is
	// leaked, and it still translates the address to a VM that is gone.
	table.chains = map[string][]string{
		natChain("dev-world-deadbeef"): {"-d 198.18.0.10/32 -j DNAT --to-destination 10.205.108.2"},
		"OUTPUT":                       {"-j " + natChain("dev-world-deadbeef"), "-j KATL-OTHER"},
		"KATL-OTHER":                   nil,
	}

	if err := manager.setNAT(context.Background(), "cp-1", "198.18.0.10"); err != nil {
		t.Fatalf("setNAT() with an address only a leaked chain holds: %v", err)
	}
	own := natChain(world.World.RunID)
	want := map[string][]string{
		own:          {"-d 198.18.0.10/32 -j DNAT --to-destination " + world.Scenario.Nodes[0].Address},
		"OUTPUT":     {"-j KATL-OTHER", "-j " + own},
		"KATL-OTHER": nil,
	}
	if !reflect.DeepEqual(table.chains, want) {
		t.Fatalf("nat table = %v, want %v", table.chains, want)
	}
}

func TestWorldNATKeepsChainsOfLiveWorlds(t *testing.T) {
	fake := newFakeLibvirt(t)
	manager := testManager(t, fake)
	other := testManager(t, fake)
	table := &fakeNATTable{chains: map[string][]string{}}
	manager.runIptables = table.run
	other.runIptables = table.run
	world, err := manager.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	otherWorld, err := other.createWorld(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustAddWorldNode(t, world, true)
	mustAddWorldNode(t, otherWorld, true)
	if err := other.syncNetwork(context.Background(), otherWorld); err != nil {
		t.Fatal(err)
	}
	if err := other.setNAT(context.Background(), "cp-1", "198.18.0.10"); err != nil {
		t.Fatal(err)
	}

	if err := manager.setNAT(context.Background(), "cp-1", "198.18.0.11"); err != nil {
		t.Fatal(err)
	}
	if len(table.chains[natChain(otherWorld.World.RunID)]) != 1 {
		t.Fatalf("another live world's chain was removed: %v", table.chains)
	}
	err = manager.setNAT(context.Background(), "cp-1", "198.18.0.10")
	if err == nil || !strings.Contains(err.Error(), natChain(otherWorld.World.RunID)) {
		t.Fatalf("setNAT() with another world's address error = %v", err)
	}
	// Repeating the world's own address is not a conflict.
	if err := manager.setNAT(context.Background(), "cp-1", "198.18.0.11"); err != nil {
		t.Fatalf("setNAT() repeated: %v", err)
	}
}

func TestWorldNATRefusesAddressWhenHostRulesUnreadable(t *testing.T) {
	manager, table, _ := natFixture(t)
	table.denied = true

	if err := manager.setNAT(context.Background(), "cp-1", "198.18.0.10"); err == nil {
		t.Fatal("setNAT() claimed an address it could not check against other worlds")
	}
	if _, err := os.Stat(manager.natPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nat record written for an unchecked address: %v", err)
	}
}

func TestSyncNetworkDoesNotRequireIptables(t *testing.T) {
	var stderr strings.Builder
	manager, table, world := natFixture(t)
	manager.stderr = &stderr
	ctx := context.Background()
	if err := manager.setNAT(ctx, "cp-1", "198.18.0.10"); err != nil {
		t.Fatal(err)
	}
	table.denied = true

	if err := manager.syncNetwork(ctx, world); err != nil {
		t.Fatalf("syncNetwork() failed on unusable iptables: %v", err)
	}
	if !strings.Contains(stderr.String(), "NAT") {
		t.Fatalf("no warning that the NAT rules were not refreshed: %q", stderr.String())
	}
	if _, err := os.Stat(manager.natPath()); err != nil {
		t.Fatalf("nat record lost: %v", err)
	}
}

// Every command that reads or changes the host NAT rules must tell the
// operator what to fix and what to rerun when sudo cannot run iptables.
func TestWorldNATExplainsUnusableIptables(t *testing.T) {
	const guidance = "could not read or change the host NAT rules"
	setup := func(t *testing.T, record bool) (installerManager, *fakeNATTable, katldevWorld, *strings.Builder) {
		manager, table, world := natFixture(t)
		var stderr strings.Builder
		manager.stderr = &stderr
		if record {
			if err := manager.setNAT(context.Background(), "cp-1", "198.18.0.10"); err != nil {
				t.Fatal(err)
			}
		}
		table.denied = true
		return manager, table, world, &stderr
	}
	check := func(t *testing.T, got, kept, rerun string) {
		t.Helper()
		for _, want := range []string{guidance, "sudo that does not prompt for iptables", "rerun: " + rerun} {
			if !strings.Contains(got, want) {
				t.Errorf("message lacks %q: %s", want, got)
			}
		}
		if hasKept := strings.Contains(got, "its record is kept"); hasKept != (kept == "kept") {
			t.Errorf("record-kept statement = %v, want %q: %s", hasKept, kept, got)
		}
	}

	t.Run("remove", func(t *testing.T) {
		manager, _, _, _ := setup(t, true)
		err := manager.removeNAT(context.Background(), "cp-1")
		if err == nil {
			t.Fatal("removeNAT() succeeded")
		}
		check(t, err.Error(), "kept", "katldev world nat cp-1 --remove")
	})
	t.Run("address", func(t *testing.T) {
		manager, _, _, _ := setup(t, false)
		err := manager.setNAT(context.Background(), "cp-1", "198.18.0.10")
		if err == nil {
			t.Fatal("setNAT() succeeded")
		}
		check(t, err.Error(), "", "katldev world nat cp-1 --address 198.18.0.10")
	})
	t.Run("world remove", func(t *testing.T) {
		manager, _, world, _ := setup(t, true)
		err := manager.removeWorldObjects(context.Background(), world.World.RunID)
		if err == nil {
			t.Fatal("removeWorldObjects() succeeded")
		}
		check(t, err.Error(), "kept", "katldev world remove")
	})
	t.Run("sync warning", func(t *testing.T) {
		manager, _, world, stderr := setup(t, true)
		if err := manager.syncNetwork(context.Background(), world); err != nil {
			t.Fatal(err)
		}
		check(t, stderr.String(), "kept", "katldev world nat cp-1 --address 198.18.0.10")
	})
}
