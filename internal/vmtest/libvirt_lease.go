package vmtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

type LibvirtLease struct {
	MACAddress string `json:"macAddress"`
	IPAddress  string `json:"ipAddress"`
	RawLine    string `json:"rawLine,omitempty"`
}

func WaitLibvirtLease(ctx context.Context, virsh, uri, network, mac string, timeout time.Duration) (LibvirtLease, error) {
	if strings.TrimSpace(mac) == "" {
		return LibvirtLease{}, errors.New("libvirt lease MAC address is required")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		lease, err := DiscoverLibvirtLease(ctx, virsh, uri, network, mac)
		if err == nil {
			return lease, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return LibvirtLease{}, fmt.Errorf("wait for libvirt lease for %s on %s: %w", mac, network, lastErr)
			}
			return LibvirtLease{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func DiscoverLibvirtLease(ctx context.Context, virsh, uri, network, mac string) (LibvirtLease, error) {
	virsh = first(virsh, "virsh")
	args := []string{}
	if strings.TrimSpace(uri) != "" {
		args = append(args, "-c", uri)
	}
	args = append(args, "net-dhcp-leases", network, "--mac", mac)
	output, err := exec.CommandContext(ctx, virsh, args...).CombinedOutput()
	if err != nil {
		return LibvirtLease{}, fmt.Errorf("virsh %s: %w\n%s", strings.Join(args, " "), err, output)
	}
	lease, ok := parseLibvirtLease(output, mac)
	if !ok {
		return LibvirtLease{}, fmt.Errorf("no libvirt DHCP lease for MAC %s in network %s", mac, network)
	}
	return lease, nil
}

func parseLibvirtLease(output []byte, mac string) (LibvirtLease, bool) {
	want := strings.ToLower(strings.TrimSpace(mac))
	var selected LibvirtLease
	var selectedExpiry time.Time
	found := false
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if strings.ToLower(field) != want || i+2 >= len(fields) {
				continue
			}
			address := strings.TrimSpace(fields[i+2])
			if slash := strings.IndexByte(address, '/'); slash >= 0 {
				address = address[:slash]
			}
			if ip := net.ParseIP(address); ip == nil {
				continue
			}
			candidate := LibvirtLease{
				MACAddress: strings.TrimSpace(mac),
				IPAddress:  address,
				RawLine:    strings.TrimSpace(line),
			}
			expiry := time.Time{}
			if i >= 2 {
				expiry, _ = time.ParseInLocation("2006-01-02 15:04:05", fields[i-2]+" "+fields[i-1], time.Local)
			}
			if !found || expiry.After(selectedExpiry) || expiry.IsZero() {
				selected = candidate
				selectedExpiry = expiry
				found = true
			}
		}
	}
	return selected, found
}
