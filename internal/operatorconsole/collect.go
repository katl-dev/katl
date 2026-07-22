package operatorconsole

import (
	"context"
	"errors"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/generation"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

type Collector struct {
	Mode                  Mode
	Version               string
	Root                  string
	StatusPath            string
	HandoffPath           string
	ManagementAddress     string
	Hostname              func() (string, error)
	Interfaces            func() ([]net.Interface, error)
	Addrs                 func(net.Interface) ([]net.Addr, error)
	DefaultRouteInterface func() (string, error)
	ProbeControlPlanePods func(context.Context) (ControlPlanePodStatuses, error)
	Now                   func() time.Time
}

const (
	maxDisplayInterfaces  = 3
	maxDisplayAddresses   = 2
	kubernetesProbePeriod = 5 * time.Second
)

func (c Collector) Collect(snapshot *Snapshot) {
	interfaces := snapshot.DisplayInterfaces
	controlPlanePods := snapshot.ControlPlanePods
	kubernetesStatusAt := snapshot.KubernetesStatusAt
	kubernetesError := snapshot.KubernetesError
	*snapshot = Snapshot{
		Mode:               c.Mode,
		DisplayInterfaces:  interfaces[:0],
		ControlPlanePods:   controlPlanePods,
		KubernetesStatusAt: kubernetesStatusAt,
		KubernetesError:    kubernetesError,
	}
	if c.Mode == ModeInstaller {
		snapshot.Version = strings.TrimSpace(c.Version)
	}
	if c.Hostname == nil {
		c.Hostname = os.Hostname
	}
	if c.Interfaces == nil {
		c.Interfaces = net.Interfaces
	}
	if c.Addrs == nil {
		c.Addrs = func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() }
	}
	if c.DefaultRouteInterface == nil {
		c.DefaultRouteInterface = func() (string, error) {
			return readDefaultRouteInterface(rooted(c.Root, "/proc/net/route"))
		}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	now := c.Now()
	if hostname, err := c.Hostname(); err == nil {
		snapshot.Hostname = hostname
	}
	snapshot.ManagementAddress, snapshot.DisplayInterfaces, snapshot.AdditionalInterfaces = c.collectNetwork(snapshot.DisplayInterfaces)
	snapshot.SSHEnabled = c.Mode == ModeRuntime || fileExists(rooted(c.Root, "/etc/katl/installer-ssh.enabled"))

	statusPath := c.StatusPath
	if statusPath == "" {
		if c.Mode == ModeInstaller {
			statusPath = rooted(c.Root, "/run/katl/state/status.json")
		} else {
			statusPath, _ = installstatus.RuntimeStatusPath(cleanRoot(c.Root))
		}
	}
	record, err := installstatus.ReadFile(statusPath)
	if err == nil {
		snapshot.State = record.State
		snapshot.CurrentStep = record.CurrentStep
		if c.Mode == ModeInstaller {
			snapshot.Generation = record.InstalledGeneration
		}
		snapshot.DestructiveMutation = record.DestructiveMutation
		snapshot.LastError = record.LastError
		snapshot.RetryHint = record.RetryHint
		snapshot.UpdatedAt = record.UpdatedAt
	} else if !errors.Is(err, os.ErrNotExist) {
		snapshot.StatusError = err.Error()
	}
	if statusCanBecomeStale(snapshot.State) && !snapshot.UpdatedAt.IsZero() && now.Sub(snapshot.UpdatedAt) > 10*time.Minute {
		snapshot.StatusStale = true
	}
	if snapshot.State == "" {
		if c.Mode == ModeInstaller {
			snapshot.State = "starting-installer"
		} else {
			snapshot.State = "starting-runtime"
		}
	}
	if c.Mode == ModeInstaller {
		path := c.HandoffPath
		if path == "" {
			path = rooted(c.Root, HandoffPath)
		}
		if handoff, err := ReadHandoff(path); err == nil {
			snapshot.Handoff = handoff
			if snapshot.State == "starting-installer" {
				snapshot.State = installstatus.StateWaitingForConfig
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			snapshot.HandoffError = "installer handoff is unreadable; restart the installer handoff service"
		}
	} else {
		snapshot.KubernetesConfigured = fileExists(rooted(c.Root, "/etc/kubernetes/kubelet.conf"))
		c.collectGeneration(snapshot)
		c.collectKubernetes(snapshot, now)
	}
}

func statusCanBecomeStale(state string) bool {
	switch state {
	case installstatus.StateRunning, installstatus.StateRuntimeBootedNotReady:
		return true
	default:
		return false
	}
}

func (c Collector) collectNetwork(result []NetworkInterface) (string, []NetworkInterface, int) {
	interfaces, err := c.Interfaces()
	if err != nil {
		return "", result[:0], 0
	}
	management := normalizeManagementAddress(c.ManagementAddress)
	preferredInterface := ""
	if management == "" {
		preferredInterface, _ = c.DefaultRouteInterface()
	}
	if cap(result) < len(interfaces) {
		result = make([]NetworkInterface, 0, len(interfaces))
	} else {
		result = result[:0]
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 || virtualInterface(iface.Name) {
			continue
		}
		addresses, err := c.Addrs(iface)
		if err != nil {
			continue
		}
		usable := make([]string, 0, len(addresses))
		for _, address := range addresses {
			ip, _, parseErr := net.ParseCIDR(address.String())
			if parseErr != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				continue
			}
			usable = append(usable, address.String())
			if management == "" && iface.Name == preferredInterface && ip.To4() != nil {
				management = ip.String()
			}
			if preferredInterface == "" && management != "" && ip.String() == management {
				preferredInterface = iface.Name
			}
		}
		if len(usable) == 0 && iface.Name != preferredInterface {
			continue
		}
		sort.Slice(usable, func(i, j int) bool {
			leftIPv4 := strings.Contains(usable[i], ".")
			rightIPv4 := strings.Contains(usable[j], ".")
			if leftIPv4 != rightIPv4 {
				return leftIPv4
			}
			return usable[i] < usable[j]
		})
		index := len(result)
		result = result[:index+1]
		addressesBuffer := result[index].Addresses[:0]
		if cap(addressesBuffer) < min(len(usable), maxDisplayAddresses) {
			addressesBuffer = make([]string, 0, min(len(usable), maxDisplayAddresses))
		}
		addressesBuffer = append(addressesBuffer, usable[:min(len(usable), maxDisplayAddresses)]...)
		result[index] = NetworkInterface{
			Name:                iface.Name,
			Addresses:           addressesBuffer,
			AdditionalAddresses: max(len(usable)-maxDisplayAddresses, 0),
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == preferredInterface {
			return true
		}
		if result[j].Name == preferredInterface {
			return false
		}
		return result[i].Name < result[j].Name
	})
	omitted := max(len(result)-maxDisplayInterfaces, 0)
	return management, result[:min(len(result), maxDisplayInterfaces)], omitted
}

func normalizeManagementAddress(value string) string {
	value = strings.TrimSpace(value)
	if slash := strings.IndexByte(value, '/'); slash >= 0 {
		value = value[:slash]
	}
	ip := net.ParseIP(value)
	if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return ""
	}
	return ip.String()
}

func virtualInterface(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{"katl-api", "cilium", "lxc", "veth", "docker", "br-", "virbr", "flannel", "cali", "dummy", "kube", "tun", "tap"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (c Collector) collectKubernetes(snapshot *Snapshot, now time.Time) {
	root := cleanRoot(c.Root)
	intentAvailable := false
	if intent, _, err := installer.ReadClusterIntent(root); err == nil {
		intentAvailable = true
		snapshot.ControlPlane = strings.TrimSpace(intent.SystemRole) == "control-plane"
		snapshot.ControlPlaneEndpoint = strings.TrimSpace(intent.Inventory.ControlPlaneEndpoint)
		if snapshot.ControlPlaneEndpoint == "" {
			snapshot.ControlPlaneEndpoint = strings.TrimSpace(intent.Inventory.ControlPlaneEndpointVIP)
		}
	}
	if !intentAvailable && fileExists(rooted(c.Root, "/etc/kubernetes/manifests/kube-apiserver.yaml")) {
		snapshot.ControlPlane = true
	}
	if !snapshot.ControlPlane {
		snapshot.ControlPlanePods = ControlPlanePodStatuses{}
		snapshot.KubernetesStatusAt = time.Time{}
		snapshot.KubernetesError = ""
		return
	}
	if !snapshot.KubernetesConfigured {
		snapshot.ControlPlanePods = initialControlPlanePods(KubernetesPodNotStarted)
		snapshot.KubernetesStatusAt = now
		snapshot.KubernetesError = ""
		return
	}
	if !snapshot.KubernetesStatusAt.IsZero() && now.Sub(snapshot.KubernetesStatusAt) < kubernetesProbePeriod {
		return
	}
	probe := c.ProbeControlPlanePods
	if probe == nil {
		probe = probeControlPlanePods
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pods, err := probe(ctx)
	if err != nil {
		pods = initialControlPlanePods(KubernetesPodUnknown)
		snapshot.KubernetesError = boundedKubernetesError(err)
	} else {
		snapshot.KubernetesError = ""
	}
	snapshot.ControlPlanePods = pods
	snapshot.KubernetesStatusAt = now
}

func boundedKubernetesError(err error) string {
	if err == nil {
		return ""
	}
	const limit = 240
	value := strings.Join(strings.Fields(err.Error()), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func readDefaultRouteInterface(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	selected := ""
	metric := uint64(^uint32(0))
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		flags, flagsErr := strconv.ParseUint(fields[3], 16, 32)
		candidateMetric, metricErr := strconv.ParseUint(fields[6], 10, 32)
		if flagsErr != nil || metricErr != nil || flags&0x1 == 0 || candidateMetric >= metric {
			continue
		}
		selected, metric = fields[0], candidateMetric
	}
	if selected == "" {
		return "", os.ErrNotExist
	}
	return selected, nil
}

func (c Collector) collectGeneration(snapshot *Snapshot) {
	root := cleanRoot(c.Root)
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		snapshot.GenerationError = "generation metadata is unavailable; inspect the KatlOS generation store"
		return
	}
	id := strings.TrimSpace(selection.BootedGenerationID)
	if id == "" {
		id = strings.TrimSpace(selection.DefaultGenerationID)
	}
	if id == "" {
		snapshot.GenerationError = "generation metadata does not identify the running KatlOS generation"
		return
	}
	bootedSpec, status, err := generation.ReadGeneration(root, id)
	if err != nil {
		snapshot.GenerationError = "running generation metadata is unreadable; inspect the KatlOS generation store"
		return
	}
	snapshot.CurrentSoftware = softwareFromSpec(bootedSpec)
	snapshot.GenerationHealth = status.HealthState

	nextID := strings.TrimSpace(selection.TargetBootGenerationID)
	if nextID == "" {
		nextID = strings.TrimSpace(selection.DefaultGenerationID)
	}
	if nextID == "" || nextID == id {
		snapshot.NextBootSoftware = snapshot.CurrentSoftware
	} else if spec, _, nextErr := generation.ReadGeneration(root, nextID); nextErr == nil {
		snapshot.NextBootSoftware = softwareFromSpec(spec)
	} else {
		snapshot.NextBootSoftware = Software{Generation: nextID}
		snapshot.GenerationError = "next-boot generation metadata is unreadable; inspect the KatlOS generation store"
	}

	snapshot.LiveSoftware = snapshot.CurrentSoftware
	target := strings.TrimSpace(selection.TargetBootGenerationID)
	if target != "" && target != id {
		// Bootstrap and online lifecycle operations activate their candidate
		// before arming it for boot. Report the selected software immediately,
		// while keeping the booted and next-boot generations distinct.
		if spec, _, targetErr := generation.ReadGeneration(root, target); targetErr == nil {
			snapshot.LiveSoftware = softwareFromSpec(spec)
		} else if snapshot.GenerationError == "" {
			snapshot.GenerationError = "live-selected generation metadata is unreadable; inspect the KatlOS generation store"
		}
	}
}

func softwareFromSpec(spec generation.GenerationSpec) Software {
	software := Software{
		Generation:    strings.TrimSpace(spec.GenerationID),
		KatlOSVersion: strings.TrimSpace(spec.RuntimeVersion),
	}
	for _, extension := range spec.Sysexts {
		if extension.Name == "kubernetes" {
			software.KubernetesVersion = strings.TrimSpace(extension.PayloadVersion)
			break
		}
	}
	return software
}

func cleanRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return "/"
	}
	return root
}

func rooted(root, path string) string {
	root = cleanRoot(root)
	if root == "/" {
		return path
	}
	return strings.TrimRight(root, "/") + path
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
