package operatorconsole

import (
	"errors"
	"net"
	"os"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/generation"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

type Collector struct {
	Mode        Mode
	Version     string
	Root        string
	StatusPath  string
	HandoffPath string
	Hostname    func() (string, error)
	Interfaces  func() ([]net.Interface, error)
	Addrs       func(net.Interface) ([]net.Addr, error)
}

func (c Collector) Collect(snapshot *Snapshot) {
	network := snapshot.Network
	*snapshot = Snapshot{
		Mode:    c.Mode,
		Version: strings.TrimSpace(c.Version),
		Network: network[:0],
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
	if hostname, err := c.Hostname(); err == nil {
		snapshot.Hostname = hostname
	}
	snapshot.Network = c.collectNetwork(snapshot.Network)
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
		snapshot.Generation = record.InstalledGeneration
		snapshot.DestructiveMutation = record.DestructiveMutation
		snapshot.LastError = record.LastError
		snapshot.RetryHint = record.RetryHint
		snapshot.UpdatedAt = record.UpdatedAt
	} else if !errors.Is(err, os.ErrNotExist) {
		snapshot.StatusError = err.Error()
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
		}
	} else {
		c.collectGeneration(snapshot)
	}
}

func (c Collector) collectNetwork(result []NetworkInterface) []NetworkInterface {
	interfaces, err := c.Interfaces()
	if err != nil {
		return result[:0]
	}
	if cap(result) < len(interfaces) {
		result = make([]NetworkInterface, 0, len(interfaces))
	} else {
		result = result[:0]
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		index := len(result)
		result = result[:index+1]
		addressesBuffer := result[index].Addresses[:0]
		result[index] = NetworkInterface{Name: iface.Name, Addresses: addressesBuffer}
		item := &result[index]
		addresses, err := c.Addrs(iface)
		if err == nil {
			if cap(item.Addresses) < len(addresses) {
				item.Addresses = make([]string, 0, len(addresses))
			}
			for _, address := range addresses {
				ip, _, err := net.ParseCIDR(address.String())
				if err != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
					continue
				}
				item.Addresses = append(item.Addresses, address.String())
			}
		}
		sort.Strings(item.Addresses)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (c Collector) collectGeneration(snapshot *Snapshot) {
	root := cleanRoot(c.Root)
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		return
	}
	id := strings.TrimSpace(selection.BootedGenerationID)
	if id == "" {
		id = strings.TrimSpace(selection.DefaultGenerationID)
	}
	if id == "" {
		return
	}
	snapshot.Generation = id
	_, status, err := generation.ReadGeneration(root, id)
	if err != nil {
		return
	}
	snapshot.GenerationHealth = status.HealthState
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
