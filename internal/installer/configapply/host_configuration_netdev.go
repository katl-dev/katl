package configapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/networkdconfig"
)

// RecreateNetdevsFlag names the operator opt-in for live netdev recreation.
const RecreateNetdevsFlag = "--recreate-netdevs"

const readdressGuidance = "stage the change with --mode next-boot when the node stays reachable at the same address, follow the re-address workflow (docs/operations/change-cluster-nodes.md#re-address-a-worker) when its address moves, or list the device in hostConfiguration.preservedNetdevs to leave it to networkd"

// netdevPlan carries the virtual devices a host configuration change owns.
// Ownership comes only from .netdev units in Katl's host configuration, so
// devices created by a CNI or kube-proxy are never selected.
type netdevPlan struct {
	deletes   []string
	recreates []string
	creates   []string
	// retained are unchanged devices; deleting or recreating a lower device
	// also removes devices stacked on it, so they are verified as well.
	retained  []string
	deferred  []string
	preserved []string
	abandoned []string
	// unverified names configured devices that networkd may legitimately
	// never create, with the reason.
	unverified map[string]string
	expected   map[string]netdevExpectation
	// stalePreserved are preservedNetdevs entries naming no netdev.
	stalePreserved []string
	// recreateRequested records --recreate-netdevs for no-op notices.
	recreateRequested bool
	protected         protectedEndpoints

	// Execution state. before is the kernel link state observed before
	// activation; it is the reference for retained devices and rollback.
	before      map[string]kernelLink
	preexisting []string
	kept        map[string]string
	started     bool
	// onDelete records a deletion before it runs, so an interrupted apply
	// leaves the affected device name in the persisted status.
	onDelete func(name string)
}

// netdevExpectation is the kernel state a configured device must reach.
type netdevExpectation struct {
	kind   string
	vlanID string
	// parents are name patterns of the links whose .network stacks the
	// device; parentsKnown is false when a referencing unit matches links by
	// something other than name.
	parents      []string
	parentsKnown bool
	// ports are name patterns of links whose .network enslaves to the device.
	ports []string
}

// stackedKinds are created only when a .network unit on another link names
// them, unless their unit sets Independent=yes.
var stackedKinds = map[string]bool{
	"vlan": true, "macvlan": true, "macvtap": true, "ipvlan": true, "ipvtap": true, "vxlan": true,
	"gre": true, "gretap": true, "ip6gre": true, "ip6gretap": true, "ipip": true, "sit": true,
	"vti": true, "vti6": true, "ip6tnl": true, "erspan": true,
}

func (p netdevPlan) live() bool {
	return len(p.deletes) > 0 || len(p.recreates) > 0 || len(p.creates) > 0
}

func planNetdevs(current, desired map[string]ownedHostFile, preserved []string, recreate bool) netdevPlan {
	desiredFiles := hostFileContents(desired)
	before := networkdconfig.Netdevs(hostFileContents(current))
	after := networkdconfig.Netdevs(desiredFiles)
	references := networkdconfig.References(desiredFiles)
	plan := netdevPlan{unverified: map[string]string{}, expected: map[string]netdevExpectation{}, recreateRequested: recreate}
	for _, name := range sortedStringUnion(before, after) {
		old, existed := before[name]
		next, exists := after[name]
		if exists {
			plan.expect(next, references[name])
		}
		switch {
		case existed && exists && old.Definition == next.Definition:
			plan.retained = append(plan.retained, name)
		case !existed:
			plan.creates = append(plan.creates, name)
		case slices.Contains(preserved, name) && !exists:
			plan.abandoned = append(plan.abandoned, name)
		case slices.Contains(preserved, name):
			plan.preserved = append(plan.preserved, name)
		case !exists:
			plan.deletes = append(plan.deletes, name)
		case recreate:
			plan.recreates = append(plan.recreates, name)
		default:
			plan.deferred = append(plan.deferred, name)
		}
	}
	for _, name := range preserved {
		_, existed := before[name]
		_, exists := after[name]
		if !existed && !exists {
			plan.stalePreserved = append(plan.stalePreserved, name)
		}
	}
	return plan
}

func (p *netdevPlan) expect(netdev networkdconfig.Netdev, references []networkdconfig.Reference) {
	kind := strings.ToLower(netdev.Kind)
	expectation := netdevExpectation{kind: kind, vlanID: netdev.VLANID, parentsKnown: true}
	stacked := 0
	for _, reference := range references {
		switch reference.Key {
		case "Bond", "Bridge", "VRF":
			expectation.ports = append(expectation.ports, reference.Names...)
		default:
			stacked++
			expectation.parents = append(expectation.parents, reference.Names...)
			if len(reference.Names) == 0 {
				expectation.parentsKnown = false
			}
		}
	}
	independent := strings.Contains(netdev.Definition, "]Independent=yes") || strings.Contains(netdev.Definition, "]Independent=true")
	switch {
	case netdev.Conditional:
		p.unverified[netdev.Name] = "its .netdev has [Match] conditions that may not apply to this host"
	case stackedKinds[kind] && !independent && stacked == 0:
		p.unverified[netdev.Name] = "no .network unit references it, so networkd does not create it"
	}
	p.expected[netdev.Name] = expectation
}

func hostFileContents(files map[string]ownedHostFile) map[string]string {
	contents := make(map[string]string, len(files))
	for filePath, file := range files {
		if !networkdconfig.IsPath(filePath) {
			continue
		}
		if file.File.Content != nil {
			contents[filePath] = *file.File.Content
		} else {
			contents[filePath] = "Source=" + file.File.Source
		}
	}
	return contents
}

// notices describe netdev lifecycle effects in the plan output for both live
// and next-boot applies.
func (p netdevPlan) notices(live bool) []string {
	var notices []string
	for _, name := range p.deletes {
		if live {
			notices = append(notices, fmt.Sprintf("netdev %s is no longer configured; Katl deletes it after reloading networkd", name))
		} else {
			notices = append(notices, fmt.Sprintf("netdev %s is no longer configured; it is not created after the reboot", name))
		}
	}
	for _, name := range p.recreates {
		if live {
			notices = append(notices, fmt.Sprintf("netdev %s settings changed; Katl recreates it now, briefly interrupting traffic through it", name))
		} else {
			notices = append(notices, fmt.Sprintf("netdev %s settings changed; it is recreated after the reboot, so %s has no effect for this apply", name, RecreateNetdevsFlag))
		}
	}
	if p.recreateRequested && len(p.recreates) == 0 {
		notices = append(notices, fmt.Sprintf("%s has no effect: no Katl-managed netdev settings changed", RecreateNetdevsFlag))
	}
	for _, name := range p.deferred {
		notices = append(notices, fmt.Sprintf("netdev %s settings changed; networkd applies them only when it creates the device, so the device is recreated after the reboot; rerun with %s to recreate it live", name, RecreateNetdevsFlag))
	}
	for _, name := range p.preserved {
		notices = append(notices, fmt.Sprintf("netdev %s is listed in preservedNetdevs; Katl leaves the existing device as networkd does, and the change takes effect when the device is next created", name))
	}
	for _, name := range p.abandoned {
		notices = append(notices, fmt.Sprintf("netdev %s is no longer configured but is listed in preservedNetdevs; Katl leaves the existing device in place, and it is not created after the next reboot", name))
	}
	for _, name := range p.stalePreserved {
		notices = append(notices, fmt.Sprintf("preservedNetdevs entry %s names no netdev in the current or desired configuration", name))
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range p.unverified {
			if (slices.Contains(p.creates, name) || slices.Contains(p.recreates, name)) && !yield(name) {
				return
			}
		}
	}) {
		notices = append(notices, fmt.Sprintf("netdev %s is not verified after apply because %s", name, p.unverified[name]))
	}
	return notices
}

// protectedEndpoints are the addresses whose devices a live apply must keep.
type protectedEndpoints struct {
	addresses map[netip.Addr]string
	// hostnames are control-plane endpoint names resolved when the guard runs.
	hostnames []string
	// defaultRoute protects the default-route devices that kubelet uses when
	// the node has no explicit Kubernetes address.
	defaultRoute bool
}

func protectedNetworkEndpoints(current, desired manifest.NodeConfig, management netip.Addr) protectedEndpoints {
	protected := protectedEndpoints{addresses: map[netip.Addr]string{}}
	add := func(value, role string) {
		address, ok := parseEndpointAddress(value)
		if !ok {
			return
		}
		if _, exists := protected.addresses[address]; !exists {
			protected.addresses[address] = role
		}
	}
	if management.IsValid() && !management.IsLoopback() {
		add(management.String(), "management endpoint")
	}
	for _, node := range []manifest.NodeConfig{current, desired} {
		if node.Bootstrap != nil {
			add(node.Bootstrap.NodeAddress, "management address")
		}
		add(node.Kubernetes.Address, "Kubernetes node address")
		if node.Kubernetes.Address == "" {
			protected.defaultRoute = true
		}
		if endpoint := node.ControlPlaneEndpoint; endpoint != nil {
			host := strings.TrimSpace(endpoint.Host)
			if _, ok := parseEndpointAddress(host); ok {
				add(host, "control-plane endpoint")
			} else if host != "" && endpoint.Advertisement == nil && !slices.Contains(protected.hostnames, host) {
				// With a VIP, the name is expected to resolve to the protected VIP.
				protected.hostnames = append(protected.hostnames, host)
			}
			if endpoint.Advertisement != nil {
				add(endpoint.Advertisement.VIP, "control-plane endpoint")
			}
		}
	}
	return protected
}

// parseEndpointAddress accepts an address with an optional port, brackets, or
// IPv6 zone and returns the bare address that kernel link state reports.
func parseEndpointAddress(value string) (netip.Addr, bool) {
	value = strings.TrimSpace(value)
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	address, err := netip.ParseAddr(strings.Trim(value, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return address.WithZone("").Unmap(), true
}

// kernelLink is the subset of `ip -j -d address show` that netdev lifecycle needs.
type kernelLink struct {
	Name     string `json:"ifname"`
	Parent   string `json:"link"`
	Master   string `json:"master"`
	LinkInfo struct {
		Kind string `json:"info_kind"`
		Data struct {
			ID      *int   `json:"id"`
			Remote  string `json:"remote"`
			Remote6 string `json:"remote6"`
		} `json:"info_data"`
	} `json:"linkinfo"`
	AddrInfo []struct {
		Local string `json:"local"`
	} `json:"addr_info"`
}

type kernelRoute struct {
	Device   string `json:"dev"`
	Nexthops []struct {
		Device string `json:"dev"`
	} `json:"nexthops"`
}

// tunnelKinds carry traffic over an underlay that `link` does not name.
var tunnelKinds = map[string]bool{
	"wireguard": true, "vxlan": true, "geneve": true, "gre": true, "gretap": true, "ip6gre": true,
	"ip6gretap": true, "ipip": true, "sit": true, "vti": true, "vti6": true, "ip6tnl": true, "erspan": true,
}

func (e Executor) readKernelLinks(ctx context.Context) (map[string]kernelLink, error) {
	var links []kernelLink
	if err := e.readJSON(ctx, "netdev-links", &links, "ip", "-j", "-d", "address", "show"); err != nil {
		return nil, err
	}
	out := make(map[string]kernelLink, len(links))
	for _, link := range links {
		out[link.Name] = link
	}
	return out, nil
}

func (e Executor) readJSON(ctx context.Context, name string, target any, argv ...string) error {
	command := Command{Name: name, Argv: argv, Timeout: e.timeout()}
	result, err := e.Runner.Run(ctx, command)
	if err == nil && !commandSucceeded(command, result) {
		err = commandFailure(command, result)
	}
	if err != nil {
		return fmt.Errorf("read kernel link state: %w", err)
	}
	if err := json.Unmarshal([]byte(result.Stdout), target); err != nil {
		return fmt.Errorf("read kernel link state: decode %s output: %w", argv[0], err)
	}
	return nil
}

// prepareNetdevs refuses unsafe changes and records the kernel link state
// that retained devices and rollback are verified against.
func (e Executor) prepareNetdevs(ctx context.Context) error {
	host := e.HostConfiguration
	if host == nil || !host.netdevs.live() {
		return nil
	}
	links, err := e.readKernelLinks(ctx)
	if err != nil {
		return err
	}
	if err := e.guardNetdevs(ctx, links, slices.Concat(host.netdevs.deletes, host.netdevs.recreates)); err != nil {
		return err
	}
	host.netdevs.before = links
	for _, name := range host.netdevs.creates {
		if _, exists := links[name]; exists {
			host.netdevs.preexisting = append(host.netdevs.preexisting, name)
		}
	}
	return nil
}

func (e Executor) guardNetdevs(ctx context.Context, links map[string]kernelLink, targets []string) error {
	netdevs := e.HostConfiguration.netdevs
	if len(targets) == 0 {
		return nil
	}
	var refusals []string
	// A deleted lower device takes its stacked devices with it; ports are only
	// released.
	for _, name := range targets {
		if !slices.Contains(netdevs.deletes, name) {
			continue
		}
		for _, link := range links {
			if link.Parent == name && slices.Contains(netdevs.retained, link.Name) {
				refusals = append(refusals, fmt.Sprintf("refusing to delete netdev %s because netdev %s stays configured and depends on it; remove %s as well or keep %s", name, link.Name, link.Name, name))
			}
		}
	}

	// reasons maps each protected device to the address that requires it.
	reasons := map[string]string{}
	protect := func(device, reason string) {
		if _, exists := reasons[device]; !exists && device != "" {
			reasons[device] = reason
		}
	}
	addresses := netdevs.protected.addresses
	for _, host := range netdevs.protected.hostnames {
		resolved, err := e.resolveHost(ctx, host)
		if err != nil || len(resolved) == 0 {
			return fmt.Errorf("refusing to delete or recreate netdevs live because the control-plane endpoint %s cannot be resolved to check which link carries it (%v); %s", host, err, readdressGuidance)
		}
		addresses = maps.Clone(addresses)
		for _, address := range resolved {
			if _, exists := addresses[address]; !exists {
				addresses[address] = "control-plane endpoint " + host
			}
		}
	}
	for _, link := range slices.Sorted(maps.Keys(links)) {
		for _, info := range links[link].AddrInfo {
			address, ok := parseEndpointAddress(info.Local)
			if !ok {
				continue
			}
			if role, ok := addresses[address]; ok {
				protect(link, fmt.Sprintf("%s %s on %s", role, address, link))
			}
		}
	}
	if netdevs.protected.defaultRoute {
		for _, family := range []string{"-4", "-6"} {
			var routes []kernelRoute
			if err := e.readJSON(ctx, "netdev-default-routes", &routes, "ip", "-j", family, "route", "show", "default"); err != nil {
				return err
			}
			for _, route := range routes {
				devices := []string{route.Device}
				for _, hop := range route.Nexthops {
					devices = append(devices, hop.Device)
				}
				for _, device := range devices {
					protect(device, fmt.Sprintf("Kubernetes node address on default-route link %s", device))
				}
			}
		}
	}
	// A protected device also needs its lower device, its ports, and the
	// underlay of a tunnel.
	var unknownUnderlay []string
	queue := slices.Sorted(maps.Keys(reasons))
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		link := links[name]
		var needed []string
		if link.Parent != "" {
			needed = append(needed, link.Parent)
		}
		for _, other := range links {
			if other.Master == name {
				needed = append(needed, other.Name)
			}
		}
		if tunnelKinds[link.LinkInfo.Kind] && link.Parent == "" {
			remote := firstNonEmpty(link.LinkInfo.Data.Remote, link.LinkInfo.Data.Remote6)
			underlay := ""
			if remote != "" {
				var routes []kernelRoute
				if err := e.readJSON(ctx, "netdev-tunnel-underlay", &routes, "ip", "-j", "route", "get", remote); err == nil && len(routes) > 0 {
					underlay = routes[0].Device
				}
			}
			if underlay == "" {
				unknownUnderlay = append(unknownUnderlay, name)
			} else {
				needed = append(needed, underlay)
			}
		}
		for _, dependency := range needed {
			if _, exists := reasons[dependency]; !exists {
				reasons[dependency] = reasons[name]
				queue = append(queue, dependency)
			}
		}
	}
	for _, name := range unknownUnderlay {
		refusals = append(refusals, fmt.Sprintf("refusing to delete or recreate netdevs live because tunnel %s carries the %s and Katl cannot tell which link carries the tunnel; %s", name, reasons[name], readdressGuidance))
	}
	var protectedRefusals []string
	for _, name := range targets {
		if reason, ok := reasons[name]; ok {
			verb := "delete"
			if slices.Contains(netdevs.recreates, name) {
				verb = "recreate"
			}
			protectedRefusals = append(protectedRefusals, fmt.Sprintf("refusing to %s netdev %s because it carries or provides the %s", verb, name, reason))
		}
	}
	if len(protectedRefusals) > 0 {
		refusals = append(refusals, strings.Join(protectedRefusals, "; ")+"; doing so live would remove that address; "+readdressGuidance)
	}
	if len(refusals) == 0 {
		return nil
	}
	return errors.New(strings.Join(refusals, "; "))
}

func (e Executor) resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resolve := e.ResolveHost
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	addresses, err := resolve(ctx, host)
	for i := range addresses {
		addresses[i] = addresses[i].WithZone("").Unmap()
	}
	return addresses, err
}

// networkd creates a missing device when a reload loads a changed definition
// for it, but it never deletes a device or applies new settings to an
// existing one. Katl therefore deletes recreated devices after the new
// configuration is visible and before networkd reloads it; deleting them after
// the reload would leave stacked devices such as VLANs missing until their
// parent link is reconfigured, which interrupts the parent's addresses.
func (e Executor) prepareNetdevReload(ctx context.Context) error {
	host := e.HostConfiguration
	if host == nil || !host.netdevs.live() {
		return nil
	}
	host.netdevs.started = true
	return e.deleteNetdevs(ctx, host.netdevs.recreates, true)
}

// reconcileNetdevs runs after networkd has reloaded the new configuration:
// it deletes devices whose .netdev was removed and verifies kernel state.
func (e Executor) reconcileNetdevs(ctx context.Context) error {
	host := e.HostConfiguration
	if host == nil || !host.netdevs.live() {
		return nil
	}
	host.netdevs.started = true
	var deletes []string
	for _, name := range host.netdevs.deletes {
		// A unit outside Katl's configuration can still define the device.
		if file := e.networkdNetdevFile(ctx, name); file != "" {
			if host.netdevs.kept == nil {
				host.netdevs.kept = map[string]string{}
			}
			host.netdevs.kept[name] = file
			continue
		}
		deletes = append(deletes, name)
	}
	if err := e.deleteNetdevs(ctx, deletes, true); err != nil {
		return err
	}
	return e.awaitNetdevs(ctx, host.netdevs.forwardDrift, "kernel link state does not match the configuration", "inspect 'networkctl status' and 'journalctl -u systemd-networkd'")
}

// networkdNetdevFile returns the .netdev unit networkd has loaded for a link.
func (e Executor) networkdNetdevFile(ctx context.Context, name string) string {
	command := Command{Name: "networkd-status-" + name, Argv: []string{"networkctl", "status", "--json=short", name}, Timeout: e.timeout()}
	result, err := e.Runner.Run(ctx, command)
	if err != nil || result.ExitStatus != 0 {
		return ""
	}
	var status struct {
		NetDevFile string `json:"NetDevFile"`
	}
	if json.Unmarshal([]byte(result.Stdout), &status) != nil {
		return ""
	}
	return status.NetDevFile
}

// rollbackNetdevs removes devices created by the failed generation before the
// restored generation is reloaded, so the reload recreates its definitions.
func (e Executor) rollbackNetdevs(ctx context.Context) error {
	host := e.HostConfiguration
	if host == nil || !host.netdevs.started {
		return nil
	}
	var names []string
	for _, name := range host.netdevs.creates {
		if !slices.Contains(host.netdevs.preexisting, name) {
			names = append(names, name)
		}
	}
	return e.deleteNetdevs(ctx, slices.Concat(names, host.netdevs.recreates), false)
}

// restoreNetdevs verifies that the restored generation's devices came back
// as they were before the apply. Katl does not reconfigure links to force
// them back, because that can drop the addresses of a protected link.
func (e Executor) restoreNetdevs(ctx context.Context) error {
	host := e.HostConfiguration
	if host == nil || !host.netdevs.started {
		return nil
	}
	return e.awaitNetdevs(ctx, host.netdevs.rollbackDrift, "after rollback, kernel link state does not match the restored generation", "reboot the node to rebuild the devices from the selected generation")
}

func (e Executor) deleteNetdevs(ctx context.Context, names []string, guarded bool) error {
	if len(names) == 0 {
		return nil
	}
	links, err := e.readKernelLinks(ctx)
	if err != nil {
		return err
	}
	// Delete upper devices first; removing a lower device also removes them.
	depth := func(name string) int {
		count := 0
		for seen := map[string]bool{}; links[name].Parent != "" && !seen[name]; count++ {
			seen[name] = true
			name = links[name].Parent
		}
		return count
	}
	ordered := slices.Clone(names)
	slices.SortStableFunc(ordered, func(a, b string) int { return depth(b) - depth(a) })
	for _, name := range ordered {
		if _, exists := links[name]; !exists {
			continue
		}
		if guarded {
			// Addresses can move between the preflight and this deletion.
			if err := e.guardNetdevs(ctx, links, []string{name}); err != nil {
				return err
			}
		}
		if onDelete := e.HostConfiguration.netdevs.onDelete; onDelete != nil {
			onDelete(name)
		}
		command := Command{Name: "networkd-delete-" + name, Argv: []string{"networkctl", "delete", name}, Timeout: e.timeout()}
		result, err := e.Runner.Run(ctx, command)
		if err == nil && !commandSucceeded(command, result) {
			err = commandFailure(command, result)
		}
		if err != nil {
			return fmt.Errorf("delete netdev %s: %w", name, err)
		}
		if links, err = e.readKernelLinks(ctx); err != nil {
			return err
		}
	}
	return nil
}

// forwardDrift compares kernel link state with the applied configuration.
func (p netdevPlan) forwardDrift(links map[string]kernelLink) []string {
	var drift []string
	for _, name := range p.deletes {
		if _, kept := p.kept[name]; kept {
			continue
		}
		if _, exists := links[name]; exists {
			drift = append(drift, "removed netdev "+name+" is still present")
		}
	}
	for _, name := range slices.Concat(p.creates, p.recreates) {
		if _, skip := p.unverified[name]; skip {
			continue
		}
		drift = append(drift, p.expectationDrift(name, links)...)
	}
	for _, name := range p.retained {
		drift = append(drift, p.unchangedDrift(name, links)...)
	}
	return drift
}

// rollbackDrift compares kernel link state with the state before the apply.
func (p netdevPlan) rollbackDrift(links map[string]kernelLink) []string {
	var drift []string
	for _, name := range p.creates {
		if _, exists := links[name]; exists && !slices.Contains(p.preexisting, name) {
			drift = append(drift, "netdev "+name+" from the failed generation is still present")
		}
	}
	for _, name := range slices.Concat(p.deletes, p.recreates, p.retained) {
		drift = append(drift, p.unchangedDrift(name, links)...)
	}
	return drift
}

func (p netdevPlan) expectationDrift(name string, links map[string]kernelLink) []string {
	link, exists := links[name]
	if !exists {
		return []string{"configured netdev " + name + " is missing"}
	}
	expected := p.expected[name]
	var drift []string
	if kind := link.LinkInfo.Kind; kind != "" && expected.kind != "" && kind != kernelKind(expected.kind) {
		drift = append(drift, fmt.Sprintf("netdev %s is a %s device, want %s", name, kind, expected.kind))
	}
	if expected.kind == "vlan" && expected.vlanID != "" && link.LinkInfo.Data.ID != nil && strconv.Itoa(*link.LinkInfo.Data.ID) != expected.vlanID {
		drift = append(drift, fmt.Sprintf("netdev %s has VLAN id %d, want %s", name, *link.LinkInfo.Data.ID, expected.vlanID))
	}
	if expected.parentsKnown && len(expected.parents) > 0 && !networkdconfig.MatchesName(expected.parents, link.Parent) {
		drift = append(drift, fmt.Sprintf("netdev %s is on link %q, want a link matching %s", name, link.Parent, strings.Join(expected.parents, " ")))
	}
	for _, port := range slices.Sorted(maps.Keys(links)) {
		if port != name && networkdconfig.MatchesName(expected.ports, port) && links[port].Master != name {
			drift = append(drift, fmt.Sprintf("link %s is not a port of netdev %s", port, name))
		}
	}
	return drift
}

// unchangedDrift reports how a device differs from its state before apply.
func (p netdevPlan) unchangedDrift(name string, links map[string]kernelLink) []string {
	before, existed := p.before[name]
	if !existed {
		return nil
	}
	link, exists := links[name]
	if !exists {
		return []string{"netdev " + name + " is missing"}
	}
	var drift []string
	if link.LinkInfo.Kind != before.LinkInfo.Kind {
		drift = append(drift, fmt.Sprintf("netdev %s is a %s device, was %s", name, link.LinkInfo.Kind, before.LinkInfo.Kind))
	}
	if link.Parent != before.Parent {
		drift = append(drift, fmt.Sprintf("netdev %s is on link %q, was on %q", name, link.Parent, before.Parent))
	}
	if before.LinkInfo.Data.ID != nil && (link.LinkInfo.Data.ID == nil || *link.LinkInfo.Data.ID != *before.LinkInfo.Data.ID) {
		drift = append(drift, fmt.Sprintf("netdev %s changed VLAN id from %d", name, *before.LinkInfo.Data.ID))
	}
	for _, port := range slices.Sorted(maps.Keys(p.before)) {
		if p.before[port].Master != name {
			continue
		}
		if current, exists := links[port]; exists && current.Master != name {
			drift = append(drift, fmt.Sprintf("link %s is no longer a port of netdev %s", port, name))
		}
	}
	return drift
}

func kernelKind(kind string) string {
	if kind == "tap" {
		return "tun"
	}
	return kind
}

// awaitNetdevs reads kernel link state independently of networkd and waits
// briefly for networkd's asynchronous device creation.
func (e Executor) awaitNetdevs(ctx context.Context, driftOf func(map[string]kernelLink) []string, summary, guidance string) error {
	deadline := time.Now().Add(e.netdevSettleTimeout())
	for {
		links, err := e.readKernelLinks(ctx)
		if err != nil {
			return err
		}
		drift := driftOf(links)
		if len(drift) == 0 {
			return nil
		}
		err = fmt.Errorf("%s: %s; %s", summary, strings.Join(drift, "; "), guidance)
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (e Executor) netdevSettleTimeout() time.Duration {
	if e.NetdevSettleTimeout > 0 {
		return e.NetdevSettleTimeout
	}
	return 20 * time.Second
}

// CheckLiveNetdevChanges refuses a live apply whose netdev deletions or
// recreations would remove the node's management, Kubernetes, or
// control-plane address, before any operation is accepted.
func CheckLiveNetdevChanges(ctx context.Context, request TrustedBundleRequest, desired manifest.Manifest, management netip.Addr, runner CommandRunner) error {
	host := planHostConfigurationChange(effectiveHostConfiguration(request.CurrentManifest.Node), effectiveHostConfiguration(desired.Node), request.RecreateNetdevs)
	targets := slices.Concat(host.netdevs.deletes, host.netdevs.recreates)
	if !host.Live || len(targets) == 0 {
		return nil
	}
	host.netdevs.protected = protectedNetworkEndpoints(request.CurrentManifest.Node, desired.Node, management)
	executor := Executor{Runner: runner, HostConfiguration: &host}
	links, err := executor.readKernelLinks(ctx)
	if err != nil {
		return err
	}
	return executor.guardNetdevs(ctx, links, targets)
}

func markNetdevEffects(action *generation.ConfigApplyDomainAction, plan netdevPlan, cause error) {
	result := generation.ConfigApplyActionPassed
	if cause != nil {
		result = generation.ConfigApplyActionFailed
	}
	for _, name := range plan.deletes {
		if file, kept := plan.kept[name]; kept && cause == nil {
			markEffect(action, "delete", "netdev "+name, generation.ConfigApplyActionSkipped, fmt.Errorf("networkd still defines it in %s; Katl left it in place", file))
			continue
		}
		markEffect(action, "delete", "netdev "+name, result, cause)
	}
	for _, name := range plan.recreates {
		markEffect(action, "recreate", "netdev "+name, result, cause)
	}
}

// markNetdevDeleting persists which device a deletion is about to remove, so
// an interrupted apply names it in its recovery guidance.
func markNetdevDeleting(action *generation.ConfigApplyDomainAction, name string) {
	for i := range action.Effects {
		effect := &action.Effects[i]
		if effect.Target == "netdev "+name && effect.Status == generation.ConfigApplyActionPlanned {
			effect.Diagnostic = "deleting netdev " + name + "; if this apply was interrupted, reboot to rebuild it from the selected generation"
		}
	}
}
