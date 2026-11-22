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
// its definition changed since the previous reload. created gives the kernel
// link a reload creates; by default a device without a parent.
type fakeNetwork struct {
	links       map[string]kernelLink
	loaded      map[string]string
	visible     map[string]string
	created     map[string]kernelLink
	routes      []kernelRoute
	routeGet    map[string]string
	netdevFiles map[string]string
	failReloads int
	onDelete    func(name string)
	commands    []string
}

func (n *fakeNetwork) Run(_ context.Context, command Command) (CommandResult, error) {
	argv := strings.Join(command.Argv, " ")
	n.commands = append(n.commands, argv)
	switch {
	case argv == "ip -j -d address show":
		links := make([]kernelLink, 0, len(n.links))
		for _, name := range slices.Sorted(maps.Keys(n.links)) {
			links = append(links, n.links[name])
		}
		data, _ := json.Marshal(links)
		return CommandResult{Stdout: string(data)}, nil
	case strings.HasPrefix(argv, "ip -j -4 route show default"):
		data, _ := json.Marshal(n.routes)
		return CommandResult{Stdout: string(data)}, nil
	case strings.HasPrefix(argv, "ip -j -6 route show default"):
		return CommandResult{Stdout: "[]"}, nil
	case strings.HasPrefix(argv, "ip -j route get "):
		device, ok := n.routeGet[strings.TrimPrefix(argv, "ip -j route get ")]
		if !ok {
			return CommandResult{ExitStatus: 2, Stderr: "unreachable"}, nil
		}
		return CommandResult{Stdout: `[{"dev":"` + device + `"}]`}, nil
	case strings.HasPrefix(argv, "networkctl status --json=short "):
		file, ok := n.netdevFiles[strings.TrimPrefix(argv, "networkctl status --json=short ")]
		if !ok {
			return CommandResult{Stdout: `{}`}, nil
		}
		return CommandResult{Stdout: `{"NetDevFile":"` + file + `"}`}, nil
	case strings.HasPrefix(argv, "networkctl delete "):
		name := strings.TrimPrefix(argv, "networkctl delete ")
		if n.onDelete != nil {
			n.onDelete(name)
		}
		if _, exists := n.links[name]; !exists {
			return CommandResult{ExitStatus: 1, Stderr: "No such device"}, nil
		}
		delete(n.links, name)
		for other, link := range n.links {
			if link.Parent == name {
				delete(n.links, other)
			}
			if link.Master == name {
				link.Master = ""
				n.links[other] = link
			}
		}
	case argv == "networkctl reload":
		if n.failReloads > 0 {
			n.failReloads--
			return CommandResult{ExitStatus: 1, Stderr: "reload failed"}, nil
		}
		for name, definition := range n.visible {
			if _, exists := n.links[name]; !exists && n.loaded[name] != definition {
				link, ok := n.created[name]
				if !ok {
					link = kernelLink{Name: name}
				}
				n.links[name] = link
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

func link(name, kind, parent, master string) kernelLink {
	var out kernelLink
	out.Name, out.LinkInfo.Kind, out.Parent, out.Master = name, kind, parent, master
	return out
}

func vlanLink(name, parent string, id int) kernelLink {
	out := link(name, "vlan", parent, "")
	out.LinkInfo.Data.ID = &id
	return out
}

func withAddress(l kernelLink, address string) kernelLink {
	l.AddrInfo = append(l.AddrInfo, struct {
		Local string `json:"local"`
	}{Local: address})
	return l
}

func runNetdevApply(t *testing.T, network *fakeNetwork, activator switchingActivator, current, desired map[string]string, recreate bool) (generation.ConfigApplyStatus, error) {
	t.Helper()
	host := planHostConfigurationChange(networkdFiles(current), networkdFiles(desired), recreate)
	host.netdevs.protected = protectedEndpoints{addresses: map[netip.Addr]string{}}
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true, Effects: host.Effects}})
	return Executor{Runner: network, Activator: activator, HostConfiguration: &host, Now: fixedNow, NetdevSettleTimeout: time.Millisecond}.ExecuteLive(t.Context(), plan)
}

const (
	eth020 = "[NetDev]\nName=eth0.20\nKind=vlan\n[VLAN]\nId=20\n"
	eth021 = "[NetDev]\nName=eth0.20\nKind=vlan\n[VLAN]\nId=21\n"
	eth030 = "[NetDev]\nName=eth0.30\nKind=vlan\n[VLAN]\nId=30\n"
)

func TestExecutorReconcilesOwnedNetdevs(t *testing.T) {
	lan := "[Match]\nName=eth0\n[Network]\nVLAN=eth0.20\nVLAN=eth0.30\n"
	currentFiles := map[string]string{
		"/etc/systemd/network/10-lan.network":    lan,
		"/etc/systemd/network/30-eth0.20.netdev": eth020,
		"/etc/systemd/network/30-eth0.30.netdev": eth030,
	}
	desiredFiles := map[string]string{
		"/etc/systemd/network/10-lan.network":    "[Match]\nName=eth0\n[Network]\nVLAN=eth0.20\n",
		"/etc/systemd/network/30-eth0.20.netdev": eth021,
	}
	network := &fakeNetwork{
		links: map[string]kernelLink{
			"eth0":        link("eth0", "", "", ""),
			"eth0.20":     vlanLink("eth0.20", "eth0", 20),
			"eth0.30":     vlanLink("eth0.30", "eth0", 30),
			"cilium_host": link("cilium_host", "veth", "", ""),
			"lxc1234":     link("lxc1234", "veth", "", ""),
		},
		loaded:  map[string]string{"eth0.20": "Id=20", "eth0.30": "Id=30"},
		created: map[string]kernelLink{"eth0.20": vlanLink("eth0.20", "eth0", 21)},
	}
	activator := switchingActivator{network: network, desired: map[string]string{"eth0.20": "Id=21"}, current: map[string]string{"eth0.20": "Id=20", "eth0.30": "Id=30"}}

	status, err := runNetdevApply(t, network, activator, currentFiles, desiredFiles, true)
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

func TestNetdevVerification(t *testing.T) {
	bond := "[NetDev]\nName=bond0\nKind=bond\n"
	ports := "[Match]\nName=katlp*\n[Network]\nBond=bond0\n"
	lan := "[Match]\nName=eth0\n[Network]\nVLAN=eth0.20\n"
	for _, tt := range []struct {
		name     string
		desired  map[string]string
		created  map[string]kernelLink
		enslaved bool
		want     string
	}{
		{
			name:    "device never created",
			desired: map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev},
			want:    "configured netdev katl0 is missing",
		},
		{
			name:    "bond without its ports",
			desired: map[string]string{"/etc/systemd/network/20-bond0.netdev": bond, "/etc/systemd/network/21-ports.network": ports},
			created: map[string]kernelLink{"bond0": link("bond0", "bond", "", "")},
			want:    "link katlp0 is not a port of netdev bond0",
		},
		{
			name:    "VLAN with the wrong id",
			desired: map[string]string{"/etc/systemd/network/10-lan.network": lan, "/etc/systemd/network/30-eth0.20.netdev": eth020},
			created: map[string]kernelLink{"eth0.20": vlanLink("eth0.20", "eth0", 21)},
			want:    "netdev eth0.20 has VLAN id 21, want 20",
		},
		{
			name:    "VLAN on the wrong parent",
			desired: map[string]string{"/etc/systemd/network/10-lan.network": lan, "/etc/systemd/network/30-eth0.20.netdev": eth020},
			created: map[string]kernelLink{"eth0.20": vlanLink("eth0.20", "eth1", 20)},
			want:    `netdev eth0.20 is on link "eth1", want a link matching eth0`,
		},
		{
			name:    "wrong kind",
			desired: map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev},
			created: map[string]kernelLink{"katl0": link("katl0", "bridge", "", "")},
			want:    "netdev katl0 is a bridge device, want dummy",
		},
		{
			name:    "unreferenced VLAN is reported, not verified",
			desired: map[string]string{"/etc/systemd/network/30-eth0.20.netdev": eth020},
		},
		{
			name:    "conditional netdev is reported, not verified",
			desired: map[string]string{"/etc/systemd/network/40-katl0.netdev": "[Match]\nHost=other\n[NetDev]\nName=katl0\nKind=dummy\n"},
		},
		{
			name:     "bond with its ports",
			desired:  map[string]string{"/etc/systemd/network/20-bond0.netdev": bond, "/etc/systemd/network/21-ports.network": ports},
			created:  map[string]kernelLink{"bond0": link("bond0", "bond", "", "")},
			enslaved: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			network := &fakeNetwork{
				links:   map[string]kernelLink{"eth0": link("eth0", "", "", ""), "eth1": link("eth1", "", "", ""), "katlp0": link("katlp0", "dummy", "", "")},
				loaded:  map[string]string{},
				created: tt.created,
			}
			visible := map[string]string{}
			for name := range tt.created {
				visible[name] = "new"
			}
			if tt.enslaved {
				network.links["katlp0"] = link("katlp0", "dummy", "", "bond0")
			}
			activator := switchingActivator{network: network, desired: visible, current: map[string]string{}}

			_, err := runNetdevApply(t, network, activator, nil, tt.desired, false)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ExecuteLive() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNetdevRollbackReportsUnrestoredDevices(t *testing.T) {
	t.Run("recreated device lost by a failed reload", func(t *testing.T) {
		current := map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev}
		desired := map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev + "MTUBytes=1400\n"}
		network := &fakeNetwork{
			links:       map[string]kernelLink{"katl0": link("katl0", "dummy", "", "")},
			loaded:      map[string]string{"katl0": "1500"},
			failReloads: 1,
		}
		activator := switchingActivator{network: network, desired: map[string]string{"katl0": "1400"}, current: map[string]string{"katl0": "1500"}}

		status, err := runNetdevApply(t, network, activator, current, desired, true)
		if err == nil || status.Rollback == nil || status.Rollback.Result != generation.ConfigApplyActionFailed || !strings.Contains(status.Rollback.Reason, "netdev katl0 is missing") {
			t.Fatalf("err = %v rollback = %#v, want a failed rollback naming katl0", err, status.Rollback)
		}
	})

	t.Run("VLAN stacked on a recreated bond", func(t *testing.T) {
		vlan := "[NetDev]\nName=bond0.30\nKind=vlan\n[VLAN]\nId=30\n"
		bondNetwork := "[Match]\nName=bond0\n[Network]\nVLAN=bond0.30\n"
		current := map[string]string{"/etc/systemd/network/20-bond0.netdev": bondNetdev, "/etc/systemd/network/20-bond0.network": bondNetwork, "/etc/systemd/network/30-bond0.30.netdev": vlan}
		desired := map[string]string{"/etc/systemd/network/20-bond0.netdev": "[NetDev]\nName=bond0\nKind=bond\n[Bond]\nMode=active-backup\n", "/etc/systemd/network/20-bond0.network": bondNetwork, "/etc/systemd/network/30-bond0.30.netdev": vlan}
		network := &fakeNetwork{
			links:   map[string]kernelLink{"bond0": link("bond0", "bond", "", ""), "bond0.30": vlanLink("bond0.30", "bond0", 30)},
			loaded:  map[string]string{"bond0": "802.3ad", "bond0.30": "30"},
			created: map[string]kernelLink{"bond0": link("bond0", "bond", "", "")},
		}
		// Only bond0's definition changes, so this networkd model never
		// recreates the VLAN that the kernel removed with its lower device.
		activator := switchingActivator{network: network, desired: map[string]string{"bond0": "active-backup", "bond0.30": "30"}, current: map[string]string{"bond0": "802.3ad", "bond0.30": "30"}}

		status, err := runNetdevApply(t, network, activator, current, desired, true)
		if err == nil || !strings.Contains(err.Error(), "netdev bond0.30 is missing") {
			t.Fatalf("error = %v, want the stacked VLAN reported missing", err)
		}
		if status.Rollback == nil || status.Rollback.Result != generation.ConfigApplyActionFailed || !strings.Contains(status.Rollback.Reason, "bond0.30") || !strings.Contains(status.Rollback.Reason, "reboot") {
			t.Fatalf("rollback = %#v, want a failed rollback naming bond0.30 with reboot guidance", status.Rollback)
		}
		for _, command := range network.commands {
			if strings.HasPrefix(command, "networkctl reconfigure") {
				t.Fatalf("rollback reconfigured links: %q", command)
			}
		}
	})
}

func TestNetdevDeletionLeavesOtherDefinitions(t *testing.T) {
	current := map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev}
	network := &fakeNetwork{
		links:       map[string]kernelLink{"katl0": link("katl0", "dummy", "", "")},
		loaded:      map[string]string{"katl0": "x"},
		netdevFiles: map[string]string{"katl0": "/run/systemd/network/40-katl0.netdev"},
	}
	activator := switchingActivator{network: network, desired: map[string]string{}, current: map[string]string{"katl0": "x"}}

	status, err := runNetdevApply(t, network, activator, current, map[string]string{"/etc/systemd/network/10-lan.network": "[Match]\nName=eth0\n"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if network.index("networkctl delete katl0") >= 0 {
		t.Fatalf("deleted katl0 although networkd still defines it: %q", network.commands)
	}
	for _, effect := range status.DomainActions[0].Effects {
		if effect.Target == "netdev katl0" && (effect.Status != generation.ConfigApplyActionSkipped || !strings.Contains(effect.Diagnostic, "/run/systemd/network/40-katl0.netdev")) {
			t.Fatalf("effect = %#v, want skipped naming the other unit", effect)
		}
	}
}

func TestNetdevDeletionIsRecordedBeforeItRuns(t *testing.T) {
	current := map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev}
	network := &fakeNetwork{links: map[string]kernelLink{"katl0": link("katl0", "dummy", "", "")}, loaded: map[string]string{"katl0": "x"}}
	activator := switchingActivator{network: network, desired: map[string]string{}, current: map[string]string{"katl0": "x"}}
	host := planHostConfigurationChange(networkdFiles(current), networkdFiles(map[string]string{"/etc/systemd/network/10-lan.network": "[Match]\nName=eth0\n"}), false)
	host.netdevs.protected = protectedEndpoints{addresses: map[netip.Addr]string{}}
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true, Effects: host.Effects}})
	statusPath := executorStatusPath(t, plan)
	var recorded string
	network.onDelete = func(string) {
		status, err := generation.ReadConfigApplyStatus(statusPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, effect := range status.DomainActions[0].Effects {
			if effect.Target == "netdev katl0" {
				recorded = effect.Diagnostic
			}
		}
	}

	if _, err := (Executor{Runner: network, Activator: activator, HostConfiguration: &host, StatusPath: statusPath, Now: fixedNow, NetdevSettleTimeout: time.Millisecond}).ExecuteLive(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded, "deleting netdev katl0") || !strings.Contains(recorded, "reboot") {
		t.Fatalf("persisted diagnostic at deletion = %q", recorded)
	}
}

func TestNetdevGuard(t *testing.T) {
	management := netip.MustParseAddr("192.0.2.10")
	explicit := manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Address: "198.51.100.1"}}
	links := map[string]kernelLink{
		"eth0":     link("eth0", "", "", "bond0"),
		"eth1":     link("eth1", "", "", "bond0"),
		"bond0":    link("bond0", "bond", "", ""),
		"bond0.20": withAddress(vlanLink("bond0.20", "bond0", 20), "192.0.2.10"),
		"bond0.30": vlanLink("bond0.30", "bond0", 30),
		"katl0":    link("katl0", "dummy", "", ""),
		"uplink0":  link("uplink0", "dummy", "", ""),
		"uplink1":  link("uplink1", "dummy", "", ""),
		"wg0":      withAddress(link("wg0", "wireguard", "", ""), "203.0.113.5"),
		"vx0":      withAddress(link("vx0", "vxlan", "", ""), "203.0.113.6"),
		"v6":       withAddress(link("v6", "dummy", "", ""), "fe80::10"),
	}
	vx := links["vx0"]
	vx.LinkInfo.Data.Remote = "198.51.100.9"
	links["vx0"] = vx
	bootstrap := explicit
	bootstrap.Bootstrap = &manifest.BootstrapIntent{NodeAddress: "[fe80::10%eth0]:9443"}
	dns := explicit
	dns.ControlPlaneEndpoint = &controlplaneendpoint.Config{Host: "api.home.arpa"}
	for _, tt := range []struct {
		name      string
		deletes   []string
		retained  []string
		protected protectedEndpoints
		routes    []kernelRoute
		resolve   func(context.Context, string) ([]netip.Addr, error)
		refused   string
	}{
		{name: "device carrying the management address", deletes: []string{"bond0.20"}, protected: protectedNetworkEndpoints(explicit, explicit, management), refused: "refusing to delete netdev bond0.20 "},
		{name: "lower device of the management VLAN", deletes: []string{"bond0"}, protected: protectedNetworkEndpoints(explicit, explicit, management), refused: "refusing to delete netdev bond0 "},
		{name: "unrelated device", deletes: []string{"katl0"}, protected: protectedNetworkEndpoints(explicit, explicit, management)},
		{name: "default route carries the implicit Kubernetes address", deletes: []string{"uplink0"}, protected: protectedNetworkEndpoints(manifest.NodeConfig{}, manifest.NodeConfig{}, netip.Addr{}), routes: []kernelRoute{{Device: "uplink0"}}, refused: "refusing to delete netdev uplink0 "},
		{name: "multipath default route", deletes: []string{"uplink1"}, protected: protectedNetworkEndpoints(manifest.NodeConfig{}, manifest.NodeConfig{}, netip.Addr{}), routes: []kernelRoute{{Nexthops: []struct {
			Device string `json:"dev"`
		}{{Device: "uplink0"}, {Device: "uplink1"}}}}, refused: "refusing to delete netdev uplink1 "},
		{name: "control-plane VIP", deletes: []string{"bond0.20"}, protected: protectedNetworkEndpoints(explicit, manifest.NodeConfig{Kubernetes: explicit.Kubernetes, ControlPlaneEndpoint: controlPlaneEndpointWithVIP("192.0.2.10")}, netip.Addr{}), refused: "refusing to delete netdev bond0.20 "},
		{name: "recorded management address with zone", deletes: []string{"v6"}, protected: protectedNetworkEndpoints(bootstrap, bootstrap, netip.Addr{}), refused: "refusing to delete netdev v6 "},
		{name: "control-plane name resolves to the device", deletes: []string{"bond0.20"}, protected: protectedNetworkEndpoints(dns, dns, netip.Addr{}), resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
		}, refused: "refusing to delete netdev bond0.20 "},
		{name: "control-plane name does not resolve", deletes: []string{"katl0"}, protected: protectedNetworkEndpoints(dns, dns, netip.Addr{}), resolve: func(context.Context, string) ([]netip.Addr, error) {
			return nil, context.DeadlineExceeded
		}, refused: "api.home.arpa cannot be resolved"},
		{name: "tunnel with unknown underlay", deletes: []string{"katl0"}, protected: protectedEndpoints{addresses: map[netip.Addr]string{netip.MustParseAddr("203.0.113.5"): "management endpoint"}}, refused: "tunnel wg0 carries"},
		{name: "tunnel underlay", deletes: []string{"uplink0"}, protected: protectedEndpoints{addresses: map[netip.Addr]string{netip.MustParseAddr("203.0.113.6"): "management endpoint"}}, refused: "refusing to delete netdev uplink0 "},
		{name: "lower device of a retained netdev", deletes: []string{"bond0"}, retained: []string{"bond0.30"}, protected: protectedEndpoints{}, refused: "netdev bond0.30 stays configured and depends on it"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host := HostConfigurationChangePlan{netdevs: netdevPlan{deletes: tt.deletes, retained: tt.retained, protected: tt.protected}}
			network := &fakeNetwork{links: links, routes: tt.routes, routeGet: map[string]string{"198.51.100.9": "uplink0"}}
			err := Executor{Runner: network, HostConfiguration: &host, ResolveHost: tt.resolve}.guardNetdevs(t.Context(), links, tt.deletes)
			if tt.refused == "" {
				if err != nil {
					t.Fatalf("guard refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.refused) {
				t.Fatalf("error = %v, want %q", err, tt.refused)
			}
		})
	}
}

func TestNetdevGuardRechecksBeforeEachDeletion(t *testing.T) {
	current := map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev}
	network := &fakeNetwork{links: map[string]kernelLink{"katl0": link("katl0", "dummy", "", "")}, loaded: map[string]string{"katl0": "x"}}
	// The management address moves onto katl0 after preflight.
	activator := switchingActivator{network: network, desired: map[string]string{}, current: map[string]string{"katl0": "x"}}
	host := planHostConfigurationChange(networkdFiles(current), networkdFiles(map[string]string{"/etc/systemd/network/10-lan.network": "[Match]\nName=eth0\n"}), false)
	host.netdevs.protected = protectedEndpoints{addresses: map[netip.Addr]string{netip.MustParseAddr("192.0.2.10"): "management endpoint"}}
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true, Effects: host.Effects}})
	executor := Executor{Runner: network, Activator: movingActivator{switchingActivator: activator, address: "192.0.2.10"}, HostConfiguration: &host, Now: fixedNow, NetdevSettleTimeout: time.Millisecond}

	if _, err := executor.ExecuteLive(t.Context(), plan); err == nil || !strings.Contains(err.Error(), "refusing to delete netdev katl0") {
		t.Fatalf("error = %v, want a refusal at deletion time", err)
	}
	if network.index("networkctl delete katl0") >= 0 {
		t.Fatalf("deleted katl0: %q", network.commands)
	}
}

type movingActivator struct {
	switchingActivator
	address string
}

func (a movingActivator) Activate(ctx context.Context, record generation.Record) error {
	a.network.links["katl0"] = withAddress(a.network.links["katl0"], a.address)
	return a.switchingActivator.Activate(ctx, record)
}

func TestNetdevNoOpNotices(t *testing.T) {
	current := networkdFiles(map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev})
	desired := networkdFiles(map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev})
	desired.PreservedNetdevs = []string{"gone0"}
	notices := planHostConfigurationChange(current, desired, true).netdevs.notices(true)
	for _, want := range []string{"--recreate-netdevs has no effect", "preservedNetdevs entry gone0 names no netdev"} {
		if !slices.ContainsFunc(notices, func(notice string) bool { return strings.Contains(notice, want) }) {
			t.Fatalf("notices = %q, want one containing %q", notices, want)
		}
	}

	changed := networkdFiles(map[string]string{"/etc/systemd/network/40-katl0.netdev": dummyNetdev + "MTUBytes=1400\n", "/etc/modules-load.d/80-x.conf": "dummy\n"})
	staged := planHostConfigurationChange(current, changed, false)
	if staged.Live || !strings.Contains(staged.Message, "kernel module configuration") || !strings.Contains(staged.Message, "netdev katl0") {
		t.Fatalf("staged message = %q, want both reasons", staged.Message)
	}
	nextBoot := planHostConfigurationChange(current, changed, true).netdevs.notices(false)
	if !slices.ContainsFunc(nextBoot, func(notice string) bool {
		return strings.Contains(notice, "--recreate-netdevs has no effect for this apply")
	}) {
		t.Fatalf("next-boot notices = %q", nextBoot)
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
