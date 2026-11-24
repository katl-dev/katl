//go:build linux

package vmtest

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const waitOnlineHeartbeat = "STATUS=User job systemd-networkd-wait-online.service/start running (50s / 2min)..."

func TestServeNotify(t *testing.T) {
	socket, address := listenUnixNotify(t)
	var log strings.Builder
	served := make(chan struct{})
	go func() {
		defer close(served)
		serveNotify(socket, &log, func() time.Time { return time.Now().UTC() })
	}()

	// Each send waits for the listener to close the connection, as PID 1
	// does, so a listener that holds connections open fails here.
	for _, message := range []string{
		"X_SYSTEMD_SIGNALS_LEVEL=2",
		waitOnlineHeartbeat,
		"X_SYSTEMD_UNIT_ACTIVE=multi-user.target",
		"not a notify message",
		"READY=1\nSTATUS=Ready.",
		"EXIT_STATUS=0\nX_SYSTEMD_SHUTDOWN=poweroff",
	} {
		if err := sendNotify(address, message); err != nil {
			t.Fatalf("send %q: %v", message, err)
		}
	}
	_ = socket.Close()
	<-served

	events := decodeNotifyEvents(t, log.String())
	want := []notifyEvent{
		{UnitActive: "multi-user.target"},
		{Error: "is not a KEY=VALUE assignment"},
		{Ready: true},
		{Shutdown: "poweroff", ExitStatus: "0"},
	}
	if len(events) != len(want) {
		t.Fatalf("logged %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, event := range events {
		if event.Received.IsZero() || event.Peer != "unix" {
			t.Fatalf("event %d lacks receipt time or peer: %+v", i, event)
		}
		if !strings.Contains(event.Error, want[i].Error) || (want[i].Error == "" && event.Error != "") {
			t.Fatalf("event %d error = %q, want %q", i, event.Error, want[i].Error)
		}
		event.Received, event.Peer, event.Error = time.Time{}, "", ""
		want[i].Error = ""
		if event != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, event, want[i])
		}
	}
}

// TestVMNotifyListener checks that a run records what its guest reports and
// stops listening when the domain exits, so a guest left running is refused
// rather than blocked.
func TestVMNotifyListener(t *testing.T) {
	result, config := vmFixture(t)
	config.Expect = "runtime ready"
	socket, address := listenUnixNotify(t)
	runner := VMRunner{
		Executor: vmExecFunc(func(ctx context.Context, serial io.Writer) error {
			if err := sendNotify(address, "READY=1\nSTATUS=Ready."); err != nil {
				return err
			}
			_, _ = io.WriteString(serial, "runtime ready\n")
			<-ctx.Done()
			return ctx.Err()
		}),
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
		notifyListen: func() (notifySocket, uint32, error) { return socket, 4242, nil },
	}

	result = runner.Run(context.Background(), result, config)

	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	data, err := os.ReadFile(result.Artifacts.InstallerNotify)
	if err != nil {
		t.Fatal(err)
	}
	if events := decodeNotifyEvents(t, string(data)); len(events) != 1 || !events[0].Ready {
		t.Fatalf("notify log = %+v, want one READY event", events)
	}
	if err := sendNotify(address, "READY=1"); err == nil {
		t.Fatal("listener still accepts after the run finished")
	}
}

func decodeNotifyEvents(t *testing.T, log string) []notifyEvent {
	t.Helper()
	var events []notifyEvent
	scanner := bufio.NewScanner(strings.NewReader(log))
	for scanner.Scan() {
		var event notifyEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func TestParseNotifyMessage(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    map[string]string
		wantErr bool
	}{
		{
			name:    "shutdown",
			message: "EXIT_STATUS=0\nX_SYSTEMD_SHUTDOWN=poweroff",
			want:    map[string]string{"EXIT_STATUS": "0", "X_SYSTEMD_SHUTDOWN": "poweroff"},
		},
		{
			name:    "value containing equals",
			message: "STATUS=a=b\n",
			want:    map[string]string{"STATUS": "a=b"},
		},
		{name: "line without assignment", message: "READY=1\nbogus", wantErr: true},
		{name: "empty", message: "\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNotifyMessage([]byte(tt.message))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseNotifyMessage() = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseNotifyMessage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVMNotifyDomain(t *testing.T) {
	tests := []struct {
		name       string
		vsock      bool
		notifyPort uint32
		wantCID    string
		wantOEM    []string
	}{
		{name: "notify only", notifyPort: 4242, wantCID: `<cid auto="yes"></cid>`, wantOEM: []string{"io.systemd.credential:vmm.notify_socket=vsock-stream:2:4242"}},
		{name: "notify with agent", vsock: true, notifyPort: 4242, wantCID: `<cid auto="no" address="4096"></cid>`, wantOEM: []string{"io.systemd.credential:vmm.notify_socket=vsock-stream:2:4242"}},
		{name: "no listener", wantCID: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, config := vmFixture(t)
			config.VSock = VSockConfig{Enabled: tt.vsock, GuestCID: 4096}
			config.notifyPort = tt.notifyPort
			plan, err := planVM(result, config, probe{
				lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
				stat:     os.Stat,
				access:   func(string) error { return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { releaseCID(result.RunID, plan.VSock.GuestCID) })

			var domain struct {
				SMBIOS struct {
					Mode string `xml:"mode,attr"`
				} `xml:"os>smbios"`
				Sysinfo struct {
					Type string   `xml:"type,attr"`
					OEM  []string `xml:"oemStrings>entry"`
				} `xml:"sysinfo"`
			}
			if err := xml.Unmarshal([]byte(plan.DomainXML), &domain); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(domain.Sysinfo.OEM, tt.wantOEM) {
				t.Fatalf("OEM strings = %q, want %q", domain.Sysinfo.OEM, tt.wantOEM)
			}
			if tt.notifyPort != 0 && (domain.SMBIOS.Mode != "sysinfo" || domain.Sysinfo.Type != "smbios") {
				t.Fatalf("SMBIOS mode = %q, sysinfo type = %q; want the OEM strings exposed to the guest", domain.SMBIOS.Mode, domain.Sysinfo.Type)
			}
			if tt.wantCID == "" {
				if strings.Contains(plan.DomainXML, "<vsock") {
					t.Fatalf("domain has a vsock device without a listener:\n%s", plan.DomainXML)
				}
			} else if !strings.Contains(plan.DomainXML, tt.wantCID) {
				t.Fatalf("domain XML lacks %s:\n%s", tt.wantCID, plan.DomainXML)
			}
		})
	}
}

type vmExecFunc func(ctx context.Context, serial io.Writer) error

func (f vmExecFunc) Run(ctx context.Context, _ string, _ []string, serial io.Writer) error {
	return f(ctx, serial)
}

type unixNotifySocket struct {
	listener *net.UnixListener
}

func (s unixNotifySocket) accept() (notifyConn, string, error) {
	conn, err := s.listener.AcceptUnix()
	if err != nil {
		return nil, "", err
	}
	return conn, "unix", nil
}

func (s unixNotifySocket) Close() error {
	return s.listener.Close()
}

var notifySocketSeq atomic.Int64

// listenUnixNotify uses an abstract socket so the name is independent of the
// length of the test's temporary directory.
func listenUnixNotify(t *testing.T) (unixNotifySocket, string) {
	t.Helper()
	address := fmt.Sprintf("@katl-vmtest-notify-%d-%d", os.Getpid(), notifySocketSeq.Add(1))
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return unixNotifySocket{listener: listener}, address
}

// sendNotify behaves like sd_notify over a stream socket: it half-closes the
// connection after writing and waits for the receiver to close it.
func sendNotify(address, message string) error {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, message); err != nil {
		return err
	}
	if err := conn.CloseWrite(); err != nil {
		return err
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if n, err := conn.Read(make([]byte, 1)); n > 0 || !errors.Is(err, io.EOF) {
		return fmt.Errorf("receiver did not close the connection: n=%d err=%v", n, err)
	}
	return nil
}
