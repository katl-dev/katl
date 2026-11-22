package vmtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

// netdevSets are netdev host-configuration sets inserted into the live
// network fixture: a VLAN on the management NIC and a bond of dummy ports
// with a VLAN stacked on it.
func netdevSets(vlanID int, bondMode string, extra string) string {
	return fmt.Sprintf(`        vmtest-netdevs:
          files:
            - path: /etc/systemd/network/80-katl-vmtest-dhcp.network.d/60-vlan.conf
              content: |
                [Network]
                VLAN=katlv20
            - path: /etc/systemd/network/40-katlv20.netdev
              content: |
                [NetDev]
                Name=katlv20
                Kind=vlan
                [VLAN]
                Id=%d
            - path: /etc/systemd/network/40-katlv20.network
              content: |
                [Match]
                Name=katlv20
                [Link]
                RequiredForOnline=no
                [Network]
                Address=198.51.100.120/32
            - path: /etc/systemd/network/41-katlb0.netdev
              content: |
                [NetDev]
                Name=katlb0
                Kind=bond
                [Bond]
                Mode=%s
            - path: /etc/systemd/network/41-katlb0.network
              content: |
                [Match]
                Name=katlb0
                [Link]
                RequiredForOnline=no
                [Network]
                VLAN=katlb0.30
                LinkLocalAddressing=no
            - path: /etc/systemd/network/42-katlp0.netdev
              content: |
                [NetDev]
                Name=katlp0
                Kind=dummy
            - path: /etc/systemd/network/42-katlp1.netdev
              content: |
                [NetDev]
                Name=katlp1
                Kind=dummy
            - path: /etc/systemd/network/42-katlp.network
              content: |
                [Match]
                Name=katlp*
                [Link]
                RequiredForOnline=no
                [Network]
                Bond=katlb0
            - path: /etc/systemd/network/43-katlb0.30.netdev
              content: |
                [NetDev]
                Name=katlb0.30
                Kind=vlan
                [VLAN]
                Id=30
            - path: /etc/systemd/network/43-katlb0.30.network
              content: |
                [Match]
                Name=katlb0.30
                [Link]
                RequiredForOnline=no
                [Network]
                Address=198.51.100.130/32
%s`, vlanID, bondMode, extra)
}

const netdevGuardSet = `        vmtest-netdev-guard:
          files:
            - path: /etc/systemd/network/44-katlguard0.netdev
              content: |
                [NetDev]
                Name=katlguard0
                Kind=dummy
            - path: /etc/systemd/network/44-katlguard0.network
              content: |
                [Match]
                Name=katlguard0
                [Link]
                RequiredForOnline=no
                [Network]
                Address=198.51.100.140/32
                [Route]
                Gateway=198.51.100.141
                GatewayOnLink=yes
                Metric=4000
`

const netdevInvalidVLANSet = `        vmtest-netdev-invalid:
          files:
            - path: /etc/systemd/network/40-katlv20.network.d/61-invalid-vlan.conf
              content: |
                [Network]
                VLAN=katlbad0
            - path: /etc/systemd/network/45-katlbad0.netdev
              content: |
                [NetDev]
                Name=katlbad0
                Kind=vlan
                [VLAN]
                Id=4095
`

// netdevFixture extends the recreated live network fixture with sets, a
// preservedNetdevs list, and optional live netdev recreation.
func netdevFixture(t *testing.T, version int, sets string, preserved string, recreate bool) string {
	t.Helper()
	base, err := os.ReadFile(configApplyFixture(t, "live-network-recreate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	config := strings.Replace(string(base), `desiredVersion: "2"`, fmt.Sprintf("desiredVersion: %q", fmt.Sprint(version)), 1)
	if !recreate {
		config = strings.Replace(config, "  recreateNetdevs: true\n", "", 1)
	}
	prefix := "      sets:\n"
	if preserved != "" {
		prefix = "      preservedNetdevs: [" + preserved + "]\n" + prefix
	}
	config = strings.Replace(config, "      sets:\n", prefix+sets, 1)
	path := filepath.Join(t.TempDir(), fmt.Sprintf("netdev-%d.yaml", version))
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func netdevDetails(t *testing.T, ctx context.Context, guest *GuestControl, name string) string {
	t.Helper()
	guestCommand(t, ctx, guest, "netdev-initialized-"+name, "udevadm", "wait", "--timeout=30", "--initialized=yes", "/sys/class/net/"+name)
	return guestCommandOutput(t, ctx, guest, "netdev-details-"+name, "ip", "-d", "link", "show", name)
}

func assertNetdevAbsent(t *testing.T, ctx context.Context, guest *GuestControl, name string) {
	t.Helper()
	result, err := guest.RunCommand(ctx, GuestCommandRequest{Name: "netdev-absent-" + name, Argv: []string{"ip", "link", "show", name}, AllowFailure: true})
	if err != nil || result.ExitStatus == 0 {
		t.Fatalf("netdev %s is still present: result=%+v err=%v", name, result, err)
	}
}

func applyNetdevFixture(t *testing.T, ctx context.Context, result Result, katlctl, endpoint, name, fixture string, wantFailure bool) *agentapi.OperationStatus {
	t.Helper()
	accepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-"+name, "live", "2026.06.06-vmtest-"+name, fixture, wantFailure)
	status := waitKatlcOperationTerminal(t, ctx, endpoint, accepted.OperationId)
	if wantFailure {
		if status.Result == operation.ResultSucceeded {
			t.Fatalf("%s status = %+v, want failure", name, status)
		}
		return status
	}
	if status.Result != operation.ResultSucceeded || status.ConfigApplyPhase != "active" {
		t.Fatalf("%s status = %+v", name, status)
	}
	return status
}

// runNetdevLifecycleSmoke exercises live netdev reconciliation on a VLAN,
// a bond with a stacked VLAN, a failed verification, and the guard.
func runNetdevLifecycleSmoke(t *testing.T, ctx context.Context, guest *GuestControl, result Result, katlctl, endpoint string) {
	t.Helper()
	applyNetdevFixture(t, ctx, result, katlctl, endpoint, "netdev-create", netdevFixture(t, 3, netdevSets(20, "active-backup", ""), "", false), false)
	if details := netdevDetails(t, ctx, guest, "katlv20"); !strings.Contains(details, "katlv20@enp1s0") || !strings.Contains(details, "vlan protocol 802.1Q id 20 ") {
		t.Fatalf("VLAN katlv20 = %s", details)
	}
	for _, port := range []string{"katlp0", "katlp1"} {
		if details := netdevDetails(t, ctx, guest, port); !strings.Contains(details, "master katlb0") {
			t.Fatalf("bond port %s = %s", port, details)
		}
	}
	if details := netdevDetails(t, ctx, guest, "katlb0.30"); !strings.Contains(details, "katlb0.30@katlb0") || !strings.Contains(details, "id 30 ") {
		t.Fatalf("stacked VLAN katlb0.30 = %s", details)
	}
	vlanIndex := strings.SplitN(guestCommandOutput(t, ctx, guest, "netdev-index-katlv20", "ip", "-o", "link", "show", "katlv20"), ":", 2)[0]

	// Changed settings are recreated live on request.
	applyNetdevFixture(t, ctx, result, katlctl, endpoint, "netdev-recreate-stack", netdevFixture(t, 4, netdevSets(21, "balance-xor", ""), "", true), false)
	if details := netdevDetails(t, ctx, guest, "katlv20"); !strings.Contains(details, "id 21 ") {
		t.Fatalf("recreated VLAN katlv20 = %s", details)
	}
	if index := strings.SplitN(guestCommandOutput(t, ctx, guest, "netdev-index-katlv20", "ip", "-o", "link", "show", "katlv20"), ":", 2)[0]; index == vlanIndex {
		t.Fatalf("katlv20 kept ifindex %s; want a recreated device", index)
	}
	if details := netdevDetails(t, ctx, guest, "katlb0"); !strings.Contains(details, "bond mode balance-xor") {
		t.Fatalf("recreated bond katlb0 = %s", details)
	}
	for _, port := range []string{"katlp0", "katlp1"} {
		if details := netdevDetails(t, ctx, guest, port); !strings.Contains(details, "master katlb0") {
			t.Fatalf("bond port %s after recreation = %s", port, details)
		}
	}
	if details := netdevDetails(t, ctx, guest, "katlb0.30"); !strings.Contains(details, "katlb0.30@katlb0") {
		t.Fatalf("stacked VLAN after bond recreation = %s", details)
	}
	assertGuestAddress(t, ctx, guest, "198.51.100.130", 32)
	// networkd never creates a VLAN with an invalid id, so verification fails
	// and the previous generation is restored.
	before := currentGenerationFromGuest(t, ctx, guest)
	failed := applyNetdevFixture(t, ctx, result, katlctl, endpoint, "netdev-invalid", netdevFixture(t, 6, netdevSets(21, "balance-xor", netdevInvalidVLANSet), "", false), true)
	if !strings.Contains(failed.GetFailureReason(), "configured netdev katlbad0 is missing") {
		t.Fatalf("failed verification reason = %q", failed.GetFailureReason())
	}
	if got := currentGenerationFromGuest(t, ctx, guest); got != before {
		t.Fatalf("current generation after failed netdev apply = %q, want %q", got, before)
	}
	assertNetdevAbsent(t, ctx, guest, "katlbad0")
	if details := netdevDetails(t, ctx, guest, "katlb0.30"); !strings.Contains(details, "katlb0.30@katlb0") {
		t.Fatalf("stacked VLAN after rollback = %s", details)
	}

	// A device carrying a default route stands in for kubelet's node address
	// when the node has no explicit Kubernetes address.
	applyNetdevFixture(t, ctx, result, katlctl, endpoint, "netdev-guard-add", netdevFixture(t, 7, netdevSets(21, "balance-xor", netdevGuardSet), "", false), false)
	refused := applyNetdevFixture(t, ctx, result, katlctl, endpoint, "netdev-guard-refused", netdevFixture(t, 8, netdevSets(21, "balance-xor", ""), "", false), true)
	if reason := refused.GetFailureReason(); !strings.Contains(reason, "refusing to delete netdev katlguard0") || !strings.Contains(reason, "--mode next-boot") {
		t.Fatalf("guard refusal reason = %q", reason)
	}
	netdevDetails(t, ctx, guest, "katlguard0")
	applyNetdevFixture(t, ctx, result, katlctl, endpoint, "netdev-guard-preserved", netdevFixture(t, 9, netdevSets(21, "balance-xor", ""), "katlguard0", false), false)
	netdevDetails(t, ctx, guest, "katlguard0")

	// Removing the sets deletes the VLAN and the bond stack.
	applyNetdevFixture(t, ctx, result, katlctl, endpoint, "netdev-remove", netdevFixture(t, 10, "", "katlguard0", false), false)
	for _, name := range []string{"katlv20", "katlb0", "katlb0.30", "katlp0", "katlp1"} {
		assertNetdevAbsent(t, ctx, guest, name)
	}
}
