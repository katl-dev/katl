package vmtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

func TestPhaseNetworkEvidence(t *testing.T) {
	for _, phase := range []string{"timeout", "unreserved timeout", "guest dial timeout", "success", "aborted hook"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			virsh := filepath.Join(dir, "virsh")
			destroyed := filepath.Join(dir, "destroyed")
			t.Setenv("KATL_FAKE_DESTROYED", destroyed)
			mac := "52:54:00:01:02:03"
			t.Setenv("KATL_FAKE_MAC", mac)
			unreserved := phase == "unreserved timeout"
			if unreserved {
				t.Setenv("KATL_FAKE_UNRESERVED", "1")
			} else {
				t.Setenv("KATL_FAKE_UNRESERVED", "")
			}
			writeExecutable(t, virsh, `#!/usr/bin/env bash
set -eu
shift 2
case "$1" in
  define|start|undefine) ;;
  console) exec sleep 60 ;;
  domstate) echo running ;;
  destroy) touch "$KATL_FAKE_DESTROYED" ;;
  dumpxml)
    test ! -e "$KATL_FAKE_DESTROYED"
    echo '<domain><devices><vsock><cid address="41"/></vsock>'
    if [[ -z "$KATL_FAKE_UNRESERVED" ]]; then
      echo '<interface><mac address="52:54:00:01:02:05"/><source network="isolated"/><target dev="vnet5"/></interface>'
    fi
    echo '<interface><mac address="52:54:00:01:02:03"/><source network="isolated"/><target dev="vnet9"/></interface>
          </devices></domain>'
    ;;
  net-dumpxml)
    test "$2" = isolated && test ! -e "$KATL_FAKE_DESTROYED"
    echo '<network><bridge name="virbr7"/></network>'
    ;;
  net-dhcp-leases)
    test "$*" = "net-dhcp-leases isolated --mac $KATL_FAKE_MAC" && test ! -e "$KATL_FAKE_DESTROYED"
    echo 'Expiry Time  MAC address  Protocol  IP address'
    ;;
  *) exit 1 ;;
esac
`)
			result, config := vmFixture(t)
			config.KVM, config.VirshPath = KVMOff, virsh
			config.Network = VMNetworkConfig{Name: "isolated"}
			if !unreserved {
				config.Network.MAC = mac
			}
			config.VSock.Enabled = true
			evidenceDir := strings.TrimSuffix(result.Artifacts.InstallerSerial, ".log") + "-network-evidence"
			guest := &networkEvidenceGuest{fakeGuestClient: fakeGuestClient{
				command: &vmtestpb.CommandResult{Stdout: []byte(`[{"ifname":"other0","address":"52:54:00:01:02:05"},{"ifname":"enp1s9","address":"52:54:00:01:02:03"}]`)},
				journal: &vmtestpb.JournalResult{Text: "enp1s9: Lost carrier\n"},
			}}
			connects := 0
			connectGuest := func(ctx context.Context, vsock VSockPlan, _ string) (guestConn, error) {
				connects++
				if vsock.GuestCID != 41 || ctx.Err() != nil {
					return nil, fmt.Errorf("guest capture got CID %d, context error %v", vsock.GuestCID, ctx.Err())
				}
				if _, err := os.Stat(destroyed); !errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("guest capture ran after destroy: %v", err)
				}
				if phase == "guest dial timeout" {
					if _, err := os.Stat(filepath.Join(evidenceDir, "network.json")); err != nil {
						return nil, fmt.Errorf("host evidence was not persisted before dialing: %w", err)
					}
					return nil, context.DeadlineExceeded
				}
				return guest, nil
			}
			readHost := func(path string) ([]byte, error) {
				values := map[string]string{
					"/sys/class/net/vnet9/carrier": "1\n", "/sys/class/net/vnet9/operstate": "unknown\n",
					"/sys/class/net/virbr7/carrier": "1\n", "/sys/class/net/virbr7/operstate": "up\n",
				}
				if value, ok := values[path]; ok {
					return []byte(value), nil
				}
				return nil, fmt.Errorf("unexpected host path %s", path)
			}
			runner := VMRunner{
				guestConnector: connectGuest,
				notifyListen:   func() (notifySocket, uint32, error) { return nil, 0, os.ErrNotExist },
				probe: probe{
					access:   func(string) error { return nil },
					readFile: readHost,
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				result = runner.RunWithVM(ctx, result, config, func(vm *VMHandle) error {
					switch phase {
					case "timeout", "unreserved timeout", "guest dial timeout":
						wait, cancel := context.WithDeadline(vm.ctx, time.Now())
						defer cancel()
						return wait.Err()
					case "aborted hook":
						runtime.Goexit() // testing.T.Fatal exits a hook the same way.
					}
					return nil
				})
			}()
			<-done
			if phase == "success" {
				if _, err := os.Stat(evidenceDir); !errors.Is(err, os.ErrNotExist) || connects != 0 || result.Status != StatusPassed {
					t.Fatalf("successful phase produced evidence or failed: stat=%v, connections=%d, failure=%s", err, connects, result.FailureSummary)
				}
				return
			}
			if phase != "aborted hook" && (result.Status != StatusFailed || result.FailureSummary != context.DeadlineExceeded.Error()) {
				t.Fatalf("phase timeout result = %s, %q", result.Status, result.FailureSummary)
			}

			var evidence networkEvidence
			if err := json.Unmarshal([]byte(readFile(t, filepath.Join(evidenceDir, "network.json"))), &evidence); err != nil {
				t.Fatal(err)
			}
			tap := &linkState{Interface: "vnet9", Carrier: "1", Operstate: "unknown"}
			bridge := &linkState{Interface: "virbr7", Carrier: "1", Operstate: "up"}
			if evidence.MAC != mac || !reflect.DeepEqual(evidence.Tap, tap) || !reflect.DeepEqual(evidence.Bridge, bridge) ||
				!strings.Contains(evidence.DHCPLeases, "Expiry Time") || len(evidence.HostErrors) != 0 {
				t.Fatalf("network evidence = %+v, tap = %+v, bridge = %+v", evidence, evidence.Tap, evidence.Bridge)
			}
			if _, err := os.Stat(destroyed); err != nil {
				t.Fatalf("executor did not destroy the domain after capture: %v", err)
			}
			if phase == "guest dial timeout" {
				if evidence.GuestError != "connect guest control: "+context.DeadlineExceeded.Error() {
					t.Fatalf("guest error = %q", evidence.GuestError)
				}
				return
			}
			if evidence.GuestError != "" {
				t.Fatalf("guest error = %q", evidence.GuestError)
			}
			if guest.commandReq == nil || !reflect.DeepEqual(guest.commandReq.Argv, []string{"ip", "-j", "-d", "link", "show"}) ||
				guest.journalReq == nil || !reflect.DeepEqual(guest.journalReq.Units, []string{"systemd-networkd.service"}) {
				t.Fatalf("guest command = %v, journal request = %v", guest.commandReq, guest.journalReq)
			}
			var report GuestDiagnosticReport
			if err := json.Unmarshal([]byte(readFile(t, filepath.Join(evidenceDir, "diagnostics.json"))), &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Files) != 2 || len(report.Journals) != 1 || len(report.Errors) != 0 {
				t.Fatalf("guest report = %+v", report)
			}
			got := map[string]string{}
			for _, file := range report.Files {
				got[file.Path] = readFile(t, file.Artifact)
			}
			if want := (map[string]string{"/sys/class/net/enp1s9/carrier": "0\n", "/sys/class/net/enp1s9/operstate": "down\n"}); !reflect.DeepEqual(got, want) {
				t.Fatalf("guest link files = %q, want %q", got, want)
			}
			if got := readFile(t, report.Journals[0].Artifact); got != "enp1s9: Lost carrier\n" {
				t.Fatalf("networkd journal = %q", got)
			}
		})
	}
}

type networkEvidenceGuest struct{ fakeGuestClient }

func (g *networkEvidenceGuest) Close() error { return nil }

func (g *networkEvidenceGuest) ReadFile(_ context.Context, req *vmtestpb.ReadFileRequest) (*vmtestpb.FileResult, error) {
	switch req.Path {
	case "/sys/class/net/enp1s9/carrier":
		return &vmtestpb.FileResult{Content: []byte("0\n")}, nil
	case "/sys/class/net/enp1s9/operstate":
		return &vmtestpb.FileResult{Content: []byte("down\n")}, nil
	default:
		return nil, fmt.Errorf("unexpected guest path %s", req.Path)
	}
}
