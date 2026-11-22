package configapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
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
	protected protectedEndpoints

	// Execution state, recorded from kernel link state before activation.
	preexisting []string
	present     []string
	dependents  []string
	started     bool
}

func (p netdevPlan) live() bool {
	return len(p.deletes) > 0 || len(p.recreates) > 0 || len(p.creates) > 0
}

func planNetdevs(current, desired map[string]ownedHostFile, preserved []string, recreate bool) netdevPlan {
	before := networkdconfig.Netdevs(hostFileContents(current))
	after := networkdconfig.Netdevs(hostFileContents(desired))
	var plan netdevPlan
	for _, name := range sortedStringUnion(before, after) {
		old, existed := before[name]
		next, exists := after[name]
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
	return plan
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
			notices = append(notices, fmt.Sprintf("netdev %s settings changed; it is recreated after the reboot", name))
		}
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
	return notices
}

// protectedEndpoints are the addresses whose devices a live apply must keep.
type protectedEndpoints struct {
	addresses map[netip.Addr]string
	// defaultRoute protects the default-route devices that kubelet uses when
	// the node has no explicit Kubernetes address.
	defaultRoute bool
}

func protectedNetworkEndpoints(current, desired manifest.NodeConfig, management netip.Addr) protectedEndpoints {
	protected := protectedEndpoints{addresses: map[netip.Addr]string{}}
	add := func(value, role string) {
		if address, err := netip.ParseAddr(strings.TrimSpace(value)); err == nil {
			if _, exists := protected.addresses[address.Unmap()]; !exists {
				protected.addresses[address.Unmap()] = role
			}
		}
	}
	if management.IsValid() && !management.IsLoopback() {
		add(management.String(), "management endpoint")
	}
	for _, node := range []manifest.NodeConfig{current, desired} {
		add(node.Kubernetes.Address, "Kubernetes node address")
		if node.Kubernetes.Address == "" {
			protected.defaultRoute = true
		}
		if endpoint := node.ControlPlaneEndpoint; endpoint != nil {
			add(endpoint.Host, "control-plane endpoint")
			if endpoint.Advertisement != nil {
				add(endpoint.Advertisement.VIP, "control-plane endpoint")
			}
		}
	}
	return protected
}

// kernelLink is the subset of `ip -j -d address show` that netdev lifecycle needs.
type kernelLink struct {
	Name     string `json:"ifname"`
	Parent   string `json:"link"`
	Master   string `json:"master"`
	AddrInfo []struct {
		Local string `json:"local"`
	} `json:"addr_info"`
}

type kernelRoute struct {
	Device string `json:"dev"`
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

// prepareNetdevs refuses changes that would remove a protected address and
// records the kernel relationships needed if a rollback must rebuild devices.
func (e Executor) prepareNetdevs(ctx context.Context) error {
	host := e.HostConfiguration
	if host == nil || !host.netdevs.live() {
		return nil
	}
	links, err := e.readKernelLinks(ctx)
	if err != nil {
		return err
	}
	if err := e.guardNetdevs(ctx, links); err != nil {
		return err
	}
	for _, name := range host.netdevs.creates {
		if _, exists := links[name]; exists {
			host.netdevs.preexisting = append(host.netdevs.preexisting, name)
		}
	}
	for _, name := range host.netdevs.retained {
		if _, exists := links[name]; exists {
			host.netdevs.present = append(host.netdevs.present, name)
		}
	}
	var dependents []string
	for _, name := range slices.Concat(host.netdevs.recreates, host.netdevs.deletes) {
		if link, exists := links[name]; exists && link.Parent != "" {
			dependents = append(dependents, link.Parent)
		}
		for _, link := range links {
			if link.Master == name {
				dependents = append(dependents, link.Name)
			}
		}
	}
	slices.Sort(dependents)
	host.netdevs.dependents = slices.Compact(dependents)
	return nil
}

func (e Executor) guardNetdevs(ctx context.Context, links map[string]kernelLink) error {
	netdevs := e.HostConfiguration.netdevs
	targets := slices.Concat(netdevs.deletes, netdevs.recreates)
	if len(targets) == 0 {
		return nil
	}
	// reasons maps each protected device to the address that requires it.
	reasons := map[string]string{}
	for _, link := range links {
		for _, info := range link.AddrInfo {
			address, err := netip.ParseAddr(info.Local)
			if err != nil {
				continue
			}
			if role, ok := netdevs.protected.addresses[address.Unmap()]; ok {
				reasons[link.Name] = fmt.Sprintf("%s %s on %s", role, address, link.Name)
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
				if _, exists := reasons[route.Device]; !exists && route.Device != "" {
					reasons[route.Device] = fmt.Sprintf("Kubernetes node address on default-route link %s", route.Device)
				}
			}
		}
	}
	// A protected device also needs its lower device and its ports.
	queue := make([]string, 0, len(reasons))
	for name := range reasons {
		queue = append(queue, name)
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		var needed []string
		if parent := links[name].Parent; parent != "" {
			needed = append(needed, parent)
		}
		for _, link := range links {
			if link.Master == name {
				needed = append(needed, link.Name)
			}
		}
		for _, dependency := range needed {
			if _, exists := reasons[dependency]; !exists {
				reasons[dependency] = reasons[name]
				queue = append(queue, dependency)
			}
		}
	}
	var refusals []string
	for _, name := range targets {
		if reason, ok := reasons[name]; ok {
			verb := "delete"
			if slices.Contains(netdevs.recreates, name) {
				verb = "recreate"
			}
			refusals = append(refusals, fmt.Sprintf("refusing to %s netdev %s because it carries or provides the %s", verb, name, reason))
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	return fmt.Errorf("%s; doing so live would remove that address; %s", strings.Join(refusals, "; "), readdressGuidance)
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
	return e.deleteNetdevs(ctx, host.netdevs.recreates)
}

// reconcileNetdevs runs after networkd has reloaded the new configuration:
// it deletes devices whose .netdev was removed and verifies kernel state.
func (e Executor) reconcileNetdevs(ctx context.Context) error {
	host := e.HostConfiguration
	if host == nil || !host.netdevs.live() {
		return nil
	}
	host.netdevs.started = true
	if err := e.deleteNetdevs(ctx, host.netdevs.deletes); err != nil {
		return err
	}
	return e.verifyNetdevs(ctx, slices.Concat(host.netdevs.creates, host.netdevs.recreates, host.netdevs.present), host.netdevs.deletes)
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
	return e.deleteNetdevs(ctx, slices.Concat(names, host.netdevs.recreates))
}

// restoreNetdevs rebuilds restored devices that the rollback reload did not
// recreate, such as a VLAN whose definition networkd had not yet reloaded.
func (e Executor) restoreNetdevs(ctx context.Context) error {
	host := e.HostConfiguration
	if host == nil || !host.netdevs.started {
		return nil
	}
	restored := slices.Concat(host.netdevs.recreates, host.netdevs.deletes)
	if len(restored) == 0 {
		return nil
	}
	if e.verifyNetdevs(ctx, restored, nil) == nil || len(host.netdevs.dependents) == 0 {
		return nil
	}
	command := Command{Name: "networkd-reconfigure-netdev-links", Argv: append([]string{"networkctl", "reconfigure"}, host.netdevs.dependents...), Timeout: e.timeout()}
	result, err := e.Runner.Run(ctx, command)
	if err == nil && !commandSucceeded(command, result) {
		err = commandFailure(command, result)
	}
	if err != nil {
		return err
	}
	return e.verifyNetdevs(ctx, restored, nil)
}

func (e Executor) deleteNetdevs(ctx context.Context, names []string) error {
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

// verifyNetdevs reads kernel link state independently of networkd and waits
// briefly for networkd's asynchronous device creation.
func (e Executor) verifyNetdevs(ctx context.Context, present, absent []string) error {
	deadline := time.Now().Add(e.netdevSettleTimeout())
	for {
		links, err := e.readKernelLinks(ctx)
		if err != nil {
			return err
		}
		var drift []string
		for _, name := range present {
			if _, exists := links[name]; !exists {
				drift = append(drift, "configured netdev "+name+" is missing")
			}
		}
		for _, name := range absent {
			if _, exists := links[name]; exists {
				drift = append(drift, "removed netdev "+name+" is still present")
			}
		}
		if len(drift) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kernel link state does not match the configuration: %s; inspect 'networkctl status' and 'journalctl -u systemd-networkd'", strings.Join(drift, "; "))
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), fmt.Errorf("kernel link state does not match the configuration: %s", strings.Join(drift, "; ")))
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
	if !host.Live || len(host.netdevs.deletes)+len(host.netdevs.recreates) == 0 {
		return nil
	}
	host.netdevs.protected = protectedNetworkEndpoints(request.CurrentManifest.Node, desired.Node, management)
	executor := Executor{Runner: runner, HostConfiguration: &host}
	links, err := executor.readKernelLinks(ctx)
	if err != nil {
		return err
	}
	return executor.guardNetdevs(ctx, links)
}

func markNetdevEffects(action *generation.ConfigApplyDomainAction, plan netdevPlan, cause error) {
	result := generation.ConfigApplyActionPassed
	if cause != nil {
		result = generation.ConfigApplyActionFailed
	}
	for _, name := range plan.deletes {
		markEffect(action, "delete", "netdev "+name, result, cause)
	}
	for _, name := range plan.recreates {
		markEffect(action, "recreate", "netdev "+name, result, cause)
	}
}
