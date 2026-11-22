package configapply

import (
	"context"
	"encoding/json"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

const (
	vlan20Netdev = "[NetDev]\nName=bond0.20\nKind=vlan\n\n[VLAN]\nId=20\n"
	vlan21Netdev = "[NetDev]\nName=bond0.20\nKind=vlan\n\n[VLAN]\nId=21\n"
	bondNetdev   = "[NetDev]\nName=bond0\nKind=bond\n\n[Bond]\nMode=802.3ad\n"
	dummyNetdev  = "[NetDev]\nName=katl0\nKind=dummy\n"
)

func networkdFiles(files map[string]string) manifest.HostConfiguration {
	set := manifest.HostConfigurationSet{State: manifest.HostConfigurationPresent}
	for _, filePath := range slices.Sorted(func(yield func(string) bool) {
		for filePath := range files {
			if !yield(filePath) {
				return
			}
		}
	}) {
		content := files[filePath]
		set.Files = append(set.Files, manifest.HostConfigurationFile{Path: filePath, Content: &content, Mode: 0o644})
	}
	return manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{"network": set}}
}

func TestPlanNetdevChanges(t *testing.T) {
	base := map[string]string{
		"/etc/systemd/network/20-bond0.netdev":    bondNetdev,
		"/etc/systemd/network/30-bond0.20.netdev": vlan20Netdev,
	}
	with := func(changes map[string]string) map[string]string {
		files := map[string]string{}
		for filePath, content := range base {
			files[filePath] = content
		}
		for filePath, content := range changes {
			if content == "" {
				delete(files, filePath)
			} else {
				files[filePath] = content
			}
		}
		return files
	}
	for _, tt := range []struct {
		name      string
		desired   map[string]string
		preserved []string
		recreate  bool
		live      bool
		effects   []string
	}{
		{
			name:    "removed netdev is deleted live",
			desired: with(map[string]string{"/etc/systemd/network/30-bond0.20.netdev": ""}),
			live:    true,
			effects: []string{"delete netdev bond0.20"},
		},
		{
			name:    "changed VLAN id waits for next boot",
			desired: with(map[string]string{"/etc/systemd/network/30-bond0.20.netdev": vlan21Netdev}),
			effects: []string{"recreate netdev bond0.20"},
		},
		{
			name:     "changed VLAN id recreates live on request",
			desired:  with(map[string]string{"/etc/systemd/network/30-bond0.20.netdev": vlan21Netdev}),
			recreate: true,
			live:     true,
			effects:  []string{"recreate netdev bond0.20"},
		},
		{
			name:    "drop-in change is a recreation",
			desired: with(map[string]string{"/etc/systemd/network/20-bond0.netdev.d/50-mode.conf": "[Bond]\nMode=active-backup\n"}),
			effects: []string{"recreate netdev bond0"},
		},
		{
			name:    "kind change is a recreation",
			desired: with(map[string]string{"/etc/systemd/network/20-bond0.netdev": "[NetDev]\nName=bond0\nKind=bridge\n"}),
			effects: []string{"recreate netdev bond0"},
		},
		{
			name: "comments, description, and file names are not settings",
			desired: with(map[string]string{
				"/etc/systemd/network/30-bond0.20.netdev": "",
				"/etc/systemd/network/40-vlan.netdev":     "# uplink VLAN\n[NetDev]\nDescription=uplink\nName = bond0.20\nKind=vlan\n\n[VLAN]\nId=20\n",
			}),
			live: true,
		},
		{
			name:      "preserved netdev keeps networkd behaviour",
			desired:   with(map[string]string{"/etc/systemd/network/30-bond0.20.netdev": ""}),
			preserved: []string{"bond0.20"},
			live:      true,
		},
		{
			name:    "added netdev is created by networkd",
			desired: with(map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev}),
			live:    true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			desired := networkdFiles(tt.desired)
			desired.PreservedNetdevs = tt.preserved
			plan := planHostConfigurationChange(networkdFiles(base), desired, tt.recreate)
			if plan.Live != tt.live {
				t.Fatalf("live = %t, want %t; message %q", plan.Live, tt.live, plan.Message)
			}
			var effects []string
			for _, effect := range plan.Effects {
				if strings.HasPrefix(effect.Target, "netdev ") {
					effects = append(effects, effect.Action+" "+effect.Target)
				}
			}
			if !slices.Equal(effects, tt.effects) {
				t.Fatalf("netdev effects = %q, want %q", effects, tt.effects)
			}
			if !tt.live && !strings.Contains(plan.Message, RecreateNetdevsFlag) {
				t.Fatalf("staged message %q does not name %s", plan.Message, RecreateNetdevsFlag)
			}
		})
	}
}

func TestNetdevNoticesFollowAcceptedMode(t *testing.T) {
	current := networkdFiles(map[string]string{"/etc/systemd/network/30-bond0.20.netdev": vlan20Netdev})
	changed := networkdFiles(map[string]string{"/etc/systemd/network/30-bond0.20.netdev": vlan21Netdev})
	removed := networkdFiles(map[string]string{"/etc/systemd/network/10-lan.network": "[Match]\nName=en*\n"})
	abandoned := removed
	abandoned.PreservedNetdevs = []string{"bond0.20"}

	for _, tt := range []struct {
		name    string
		desired manifest.HostConfiguration
		mode    string
		want    string
	}{
		{name: "deferred recreation", desired: changed, mode: generation.ApplyModeNextBoot, want: "rerun with --recreate-netdevs"},
		{name: "live deletion", desired: removed, mode: generation.ApplyModeLive, want: "Katl deletes it after reloading networkd"},
		{name: "preserved removal", desired: abandoned, mode: generation.ApplyModeLive, want: "Katl leaves the existing device in place"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			domains := domainAccumulator{}
			domains.addHostConfiguration(planHostConfigurationChange(current, tt.desired, false))
			decision, err := Plan(generation.ApplyModeAuto, domains.changes())
			if err != nil {
				t.Fatal(err)
			}
			if decision.AcceptedMode != tt.mode {
				t.Fatalf("accepted mode = %q, want %q", decision.AcceptedMode, tt.mode)
			}
			if !slices.ContainsFunc(decision.Warnings, func(warning string) bool { return strings.Contains(warning, tt.want) }) {
				t.Fatalf("warnings = %q, want one containing %q", decision.Warnings, tt.want)
			}
		})
	}
}

// fakeNetwork models kernel links and the networkd behaviour observed on
// systemd 262: a reload creates a configured device that is absent only when
// its definition changed since the previous reload.
type fakeNetwork struct {
	links    map[string]kernelLink
	loaded   map[string]string
	visible  map[string]string
	routes   []kernelRoute
	commands []string
}

func (n *fakeNetwork) Run(_ context.Context, command Command) (CommandResult, error) {
	argv := strings.Join(command.Argv, " ")
	n.commands = append(n.commands, argv)
	switch {
	case argv == "ip -j -d address show":
		links := make([]kernelLink, 0, len(n.links))
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for name := range n.links {
				if !yield(name) {
					return
				}
			}
		}) {
			links = append(links, n.links[name])
		}
		data, _ := json.Marshal(links)
		return CommandResult{Stdout: string(data)}, nil
	case strings.HasPrefix(argv, "ip -j -4 route show default"):
		data, _ := json.Marshal(n.routes)
		return CommandResult{Stdout: string(data)}, nil
	case strings.HasPrefix(argv, "ip -j -6 route show default"):
		return CommandResult{Stdout: "[]"}, nil
	case strings.HasPrefix(argv, "networkctl delete "):
		name := strings.TrimPrefix(argv, "networkctl delete ")
		if _, exists := n.links[name]; !exists {
			return CommandResult{ExitStatus: 1, Stderr: "No such device"}, nil
		}
		delete(n.links, name)
		for other, link := range n.links {
			if link.Parent == name {
				delete(n.links, other)
			}
		}
	case argv == "networkctl reload":
		for name, definition := range n.visible {
			if _, exists := n.links[name]; !exists && n.loaded[name] != definition {
				n.links[name] = kernelLink{Name: name, Parent: "eth0"}
			}
		}
		n.loaded = maps.Clone(n.visible)
	}
	return CommandResult{}, nil
}

func (n *fakeNetwork) index(command string) int {
	return slices.Index(n.commands, command)
}

// switchingActivator makes the candidate configuration visible to fakeNetwork.
type switchingActivator struct {
	network *fakeNetwork
	desired map[string]string
	current map[string]string
}

func (a switchingActivator) Activate(context.Context, generation.Record) error {
	a.network.visible = a.desired
	return nil
}

func (a switchingActivator) Rollback(context.Context, string) error {
	a.network.visible = a.current
	return nil
}

func TestExecutorReconcilesOwnedNetdevs(t *testing.T) {
	currentFiles := map[string]string{
		"/etc/systemd/network/30-eth0.20.netdev": "[NetDev]\nName=eth0.20\nKind=vlan\n[VLAN]\nId=20\n",
		"/etc/systemd/network/30-eth0.30.netdev": "[NetDev]\nName=eth0.30\nKind=vlan\n[VLAN]\nId=30\n",
	}
	desiredFiles := map[string]string{
		"/etc/systemd/network/30-eth0.20.netdev": "[NetDev]\nName=eth0.20\nKind=vlan\n[VLAN]\nId=21\n",
	}
	network := &fakeNetwork{
		links: map[string]kernelLink{
			"eth0":        {Name: "eth0"},
			"eth0.20":     {Name: "eth0.20", Parent: "eth0"},
			"eth0.30":     {Name: "eth0.30", Parent: "eth0"},
			"cilium_host": {Name: "cilium_host"},
			"lxc1234":     {Name: "lxc1234"},
		},
		loaded: map[string]string{"eth0.20": "Id=20", "eth0.30": "Id=30"},
	}
	activator := switchingActivator{network: network, desired: map[string]string{"eth0.20": "Id=21"}, current: map[string]string{"eth0.20": "Id=20", "eth0.30": "Id=30"}}
	host := planHostConfigurationChange(networkdFiles(currentFiles), networkdFiles(desiredFiles), true)
	host.netdevs.protected = protectedEndpoints{addresses: map[netip.Addr]string{}}
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true, Effects: host.Effects}})

	status, err := Executor{Runner: network, Activator: activator, HostConfiguration: &host, Now: fixedNow, NetdevSettleTimeout: time.Millisecond}.ExecuteLive(t.Context(), plan)
	if err != nil {
		t.Fatalf("ExecuteLive() error = %v; commands %q", err, network.commands)
	}

	recreate, reload, remove := network.index("networkctl delete eth0.20"), network.index("networkctl reload"), network.index("networkctl delete eth0.30")
	if recreate < 0 || reload < 0 || remove < 0 || recreate > reload || remove < reload {
		t.Fatalf("commands = %q, want recreated eth0.20 deleted before reload and removed eth0.30 after it", network.commands)
	}
	for _, command := range network.commands {
		if strings.HasPrefix(command, "networkctl delete ") && command != "networkctl delete eth0.20" && command != "networkctl delete eth0.30" {
			t.Fatalf("deleted a device Katl does not own: %q", command)
		}
	}
	if _, exists := network.links["eth0.20"]; !exists {
		t.Fatalf("recreated eth0.20 is missing: %v", network.links)
	}
	for _, name := range []string{"cilium_host", "lxc1234"} {
		if _, exists := network.links[name]; !exists {
			t.Fatalf("CNI device %s was removed", name)
		}
	}
	for _, effect := range status.DomainActions[0].Effects {
		if strings.HasPrefix(effect.Target, "netdev ") && effect.Status != generation.ConfigApplyActionPassed {
			t.Fatalf("effect %#v, want passed", effect)
		}
	}
}

func TestExecutorReportsNetdevDrift(t *testing.T) {
	network := &fakeNetwork{links: map[string]kernelLink{"eth0": {Name: "eth0"}}, loaded: map[string]string{}}
	desired := map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev}
	host := planHostConfigurationChange(manifest.HostConfiguration{}, networkdFiles(desired), false)
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true, Effects: host.Effects}})
	// The activator never exposes katl0, so networkd cannot create it.
	activator := switchingActivator{network: network, desired: map[string]string{}, current: map[string]string{}}

	_, err := Executor{Runner: network, Activator: activator, HostConfiguration: &host, Now: fixedNow, NetdevSettleTimeout: time.Millisecond}.ExecuteLive(t.Context(), plan)
	if err == nil || !strings.Contains(err.Error(), "configured netdev katl0 is missing") {
		t.Fatalf("error = %v, want missing-device drift", err)
	}
}

func TestNetdevGuard(t *testing.T) {
	management := netip.MustParseAddr("192.0.2.10")
	links := map[string]kernelLink{
		"eth0":  {Name: "eth0", Master: "bond0"},
		"eth1":  {Name: "eth1", Master: "bond0"},
		"bond0": {Name: "bond0"},
		"bond0.20": {Name: "bond0.20", Parent: "bond0", AddrInfo: []struct {
			Local string `json:"local"`
		}{{Local: "192.0.2.10"}}},
		"katl0":   {Name: "katl0"},
		"uplink0": {Name: "uplink0"},
	}
	for _, tt := range []struct {
		name      string
		deletes   []string
		protected protectedEndpoints
		routes    []kernelRoute
		refused   string
	}{
		{name: "device carrying the management address", deletes: []string{"bond0.20"}, protected: protectedNetworkEndpoints(manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Address: "198.51.100.1"}}, manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Address: "198.51.100.1"}}, management), refused: "bond0.20"},
		{name: "lower device of the management VLAN", deletes: []string{"bond0"}, protected: protectedNetworkEndpoints(manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Address: "198.51.100.1"}}, manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Address: "198.51.100.1"}}, management), refused: "bond0"},
		{name: "unrelated device", deletes: []string{"katl0"}, protected: protectedNetworkEndpoints(manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Address: "198.51.100.1"}}, manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Address: "198.51.100.1"}}, management)},
		{name: "default route carries the implicit Kubernetes address", deletes: []string{"uplink0"}, protected: protectedNetworkEndpoints(manifest.NodeConfig{}, manifest.NodeConfig{}, netip.Addr{}), routes: []kernelRoute{{Device: "uplink0"}}, refused: "uplink0"},
		{name: "control-plane VIP", deletes: []string{"bond0.20"}, protected: protectedNetworkEndpoints(manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Address: "198.51.100.1"}}, manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Address: "198.51.100.1"}, ControlPlaneEndpoint: controlPlaneEndpointWithVIP("192.0.2.10")}, netip.Addr{}), refused: "bond0.20"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host := HostConfigurationChangePlan{netdevs: netdevPlan{deletes: tt.deletes, protected: tt.protected}}
			network := &fakeNetwork{links: links, routes: tt.routes}
			err := Executor{Runner: network, HostConfiguration: &host}.guardNetdevs(t.Context(), links)
			if tt.refused == "" {
				if err != nil {
					t.Fatalf("guard refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "refusing to delete netdev "+tt.refused+" ") || !strings.Contains(err.Error(), "--mode next-boot") {
				t.Fatalf("error = %v, want refusal for %s with re-address guidance", err, tt.refused)
			}
		})
	}
}

func TestSetRecreateNetdevs(t *testing.T) {
	document := []byte("apiVersion: katl.dev/v1alpha1\nkind: NodeConfigurationChange\nmetadata:\n  sourceID: lab\n  desiredVersion: \"1\"\napply:\n  mode: auto\nspec: {}\n")
	patched, err := SetRecreateNetdevs(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeNodeConfigurationChange(strings.NewReader(string(patched)))
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Apply.RecreateNetdevs || decoded.Apply.Mode != "auto" {
		t.Fatalf("apply = %#v", decoded.Apply)
	}
}

func controlPlaneEndpointWithVIP(vip string) *controlplaneendpoint.Config {
	return &controlplaneendpoint.Config{Host: "api.home.arpa", Advertisement: &controlplaneendpoint.Advertisement{VIP: vip}}
}

func TestExecutorReportsLostStackedNetdev(t *testing.T) {
	vlan := "[NetDev]\nName=bond0.30\nKind=vlan\n[VLAN]\nId=30\n"
	currentFiles := map[string]string{"/etc/systemd/network/20-bond0.netdev": bondNetdev, "/etc/systemd/network/30-bond0.30.netdev": vlan}
	desiredFiles := map[string]string{"/etc/systemd/network/20-bond0.netdev": "[NetDev]\nName=bond0\nKind=bond\n[Bond]\nMode=active-backup\n", "/etc/systemd/network/30-bond0.30.netdev": vlan}
	network := &fakeNetwork{
		links:  map[string]kernelLink{"bond0": {Name: "bond0"}, "bond0.30": {Name: "bond0.30", Parent: "bond0"}},
		loaded: map[string]string{"bond0": "802.3ad", "bond0.30": "30"},
	}
	// Only bond0's definition changes, so this networkd model does not
	// recreate the VLAN that the kernel removed with its lower device.
	activator := switchingActivator{network: network, desired: map[string]string{"bond0": "active-backup", "bond0.30": "30"}, current: map[string]string{"bond0": "802.3ad", "bond0.30": "30"}}
	host := planHostConfigurationChange(networkdFiles(currentFiles), networkdFiles(desiredFiles), true)
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true, Effects: host.Effects}})

	_, err := Executor{Runner: network, Activator: activator, HostConfiguration: &host, Now: fixedNow, NetdevSettleTimeout: time.Millisecond}.ExecuteLive(t.Context(), plan)
	if err == nil || !strings.Contains(err.Error(), "configured netdev bond0.30 is missing") {
		t.Fatalf("error = %v, want the stacked VLAN reported missing", err)
	}
}
