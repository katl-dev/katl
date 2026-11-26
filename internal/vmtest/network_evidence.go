package vmtest

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

const carrierUnavailable = "carrier unavailable: interface down"

type networkEvidence struct {
	CapturedAt time.Time  `json:"capturedAt"`
	Domain     string     `json:"domain"`
	Network    string     `json:"network,omitempty"`
	MAC        string     `json:"mac,omitempty"`
	SerialLog  string     `json:"serialLog"`
	Tap        *linkState `json:"tap,omitempty"`
	Bridge     *linkState `json:"bridge,omitempty"`
	DHCPLeases string     `json:"dhcpLeases,omitempty"`
	HostErrors []string   `json:"hostErrors,omitempty"`
	GuestError string     `json:"guestError,omitempty"`
}

type linkState struct {
	Interface string   `json:"interface"`
	Carrier   string   `json:"carrier,omitempty"`
	Operstate string   `json:"operstate,omitempty"`
	Errors    []string `json:"errors,omitempty"`
}

func (r VMRunner) captureNetworkFailure(ctx context.Context, plan VMPlan, result Result, virsh func(context.Context, ...string) (string, error)) {
	dir := strings.TrimSuffix(plan.SerialLog, ".log") + "-network-evidence"
	evidence := networkEvidence{
		CapturedAt: time.Now().UTC(), Domain: plan.DomainName,
		Network: plan.LibvirtNetwork, MAC: plan.MACAddress, SerialLog: plan.SerialLog,
	}
	hostCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	evidence.captureHost(hostCtx, virsh, r.probe.withDefaults().readFile)
	cancel()

	path := filepath.Join(dir, "network.json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "vmtest network evidence: %v\n", err)
		return
	}
	if err := writeJSON(path, evidence); err != nil {
		fmt.Fprintf(os.Stderr, "vmtest network evidence: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "vmtest network evidence: %s\n", path)

	// Persist the host snapshot before contacting the affected guest.
	// Keep these diagnostics separate from the scenario's normal guest data.
	result.Artifacts.GuestDir = dir
	if err := r.captureGuestNetwork(ctx, plan.VSock, result, evidence.MAC); err != nil {
		evidence.GuestError = err.Error()
		if err := writeJSON(path, evidence); err != nil {
			fmt.Fprintf(os.Stderr, "vmtest network evidence: %v\n", err)
		}
	}
}

func (e *networkEvidence) captureHost(ctx context.Context, virsh func(context.Context, ...string) (string, error), readFile func(string) ([]byte, error)) {
	if e.Network == "" {
		return
	}
	var domain struct {
		Interfaces []domainInterface `xml:"devices>interface"`
	}
	if err := readLiveXML(ctx, virsh, &domain, "dumpxml", e.Domain); err != nil {
		e.HostErrors = append(e.HostErrors, err.Error())
	} else {
		for _, iface := range domain.Interfaces {
			if iface.Source.Network != e.Network || iface.MAC == nil {
				continue
			}
			if e.MAC != "" && !strings.EqualFold(iface.MAC.Address, e.MAC) {
				continue
			}
			// Unreserved guests can have a MAC assigned by libvirt.
			e.MAC = iface.MAC.Address
			if iface.Target != nil && iface.Target.Dev != "" {
				e.Tap = readLinkState(iface.Target.Dev, readFile)
			}
			break
		}
		if e.Tap == nil {
			e.HostErrors = append(e.HostErrors, "no live tap for MAC "+e.MAC)
		}
	}

	var network struct {
		Bridge struct {
			Name string `xml:"name,attr"`
		} `xml:"bridge"`
	}
	if err := readLiveXML(ctx, virsh, &network, "net-dumpxml", e.Network); err != nil {
		e.HostErrors = append(e.HostErrors, err.Error())
	} else if network.Bridge.Name != "" {
		e.Bridge = readLinkState(network.Bridge.Name, readFile)
	} else {
		e.HostErrors = append(e.HostErrors, "no live bridge for network "+e.Network)
	}

	leases, err := virsh(ctx, libvirtLeaseArgs(e.Network, e.MAC)...)
	e.DHCPLeases = leases
	if err != nil {
		e.HostErrors = append(e.HostErrors, err.Error())
	}
}

func readLiveXML(ctx context.Context, virsh func(context.Context, ...string) (string, error), target any, args ...string) error {
	output, err := virsh(ctx, args...)
	if err != nil {
		return err
	}
	if err := xml.Unmarshal([]byte(output), target); err != nil {
		return fmt.Errorf("decode virsh %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func readLinkState(name string, readFile func(string) ([]byte, error)) *linkState {
	state := &linkState{Interface: name}
	for _, file := range []struct {
		name  string
		value *string
	}{{"carrier", &state.Carrier}, {"operstate", &state.Operstate}} {
		path := filepath.Join("/sys/class/net", name, file.name)
		data, err := readFile(path)
		if isCarrierUnavailable(path, err) {
			state.Carrier = carrierUnavailable
			continue
		}
		if err != nil {
			state.Errors = append(state.Errors, err.Error())
			continue
		}
		*file.value = strings.TrimSpace(string(data))
	}
	return state
}

func readGuestFileWithCarrierState(ctx context.Context, client GuestAgentClient, req *vmtestpb.ReadFileRequest) (*vmtestpb.FileResult, error) {
	result, err := client.ReadFile(ctx, req)
	if !isCarrierUnavailable(req.Path, err) {
		return result, err
	}
	content := []byte(carrierUnavailable + "\n")
	return &vmtestpb.FileResult{Content: content, SizeBytes: uint32(len(content))}, nil
}

func isCarrierUnavailable(path string, err error) bool {
	if err == nil || filepath.Base(path) != "carrier" || filepath.Dir(filepath.Dir(path)) != "/sys/class/net" {
		return false
	}
	// Linux returns EINVAL for carrier reads while the interface is administratively down.
	// The guest agent transmits the read error as text.
	readErr := &os.PathError{Op: "read", Path: path, Err: syscall.EINVAL}
	return errors.Is(err, syscall.EINVAL) || err.Error() == "read_file_failed: "+readErr.Error()
}

func (r VMRunner) captureGuestNetwork(ctx context.Context, vsock VSockPlan, result Result, mac string) error {
	if !vsock.Enabled || vsock.GuestCID == 0 {
		return errors.New("guest control is unavailable; see the serial log")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	client, err := r.connectGuest(dialCtx, vsock, result.Artifacts.VSockTranscript)
	cancel()
	if err != nil {
		return fmt.Errorf("connect guest control: %w", err)
	}
	defer client.Close()

	// Bound the whole guest exchange, including a write to an unresponsive peer.
	const linkTimeout, diagnosticsTimeout = 2 * time.Second, 6 * time.Second
	ctx, cancel = context.WithTimeout(ctx, linkTimeout+diagnosticsTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()

	guest := NewGuestControl(result, client)
	guest.Timeout = linkTimeout
	link, err := guestInterfaceForMAC(ctx, guest, mac)
	diagnostics := GuestDiagnostics{
		Timeout: diagnosticsTimeout,
		Journals: []GuestJournalRequest{
			{Name: "networkd", Units: []string{"systemd-networkd.service"}, MaxBytes: 128 << 10},
		},
	}
	if err == nil {
		for _, file := range []string{"carrier", "operstate"} {
			diagnostics.Files = append(diagnostics.Files, GuestFileRequest{
				Name: file, Path: filepath.Join("/sys/class/net", link, file), MaxBytes: 128, StoreContent: true,
			})
		}
	}
	guest.CollectDiagnostics(ctx, diagnostics)
	return err
}

func guestInterfaceForMAC(ctx context.Context, guest *GuestControl, mac string) (string, error) {
	command, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name: "ip-link", Argv: []string{"ip", "-j", "-d", "link", "show"},
	})
	if err != nil {
		return "", err
	}
	output, err := os.ReadFile(command.Stdout)
	if err != nil {
		return "", err
	}
	var links []struct {
		Name string `json:"ifname"`
		MAC  string `json:"address"`
	}
	if err := json.Unmarshal(output, &links); err != nil {
		return "", fmt.Errorf("decode ip link: %w", err)
	}
	for _, link := range links {
		if link.Name != "" && mac != "" && strings.EqualFold(link.MAC, mac) {
			return link.Name, nil
		}
	}
	return "", fmt.Errorf("no guest interface for MAC %s", mac)
}
