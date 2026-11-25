package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"golang.org/x/sys/unix"
)

// systemdNotifications receives what the agent reports on NOTIFY_SOCKET.
type systemdNotifications struct {
	conn net.PacketConn
}

func listenNotifications(t *testing.T) systemdNotifications {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenPacket("unixgram", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	return systemdNotifications{conn: conn}
}

// next returns the fields of the next notification.
func (n systemdNotifications) next(t *testing.T) map[string]string {
	t.Helper()
	if err := n.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	size, _, err := n.conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no notification: %v", err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(buf[:size]), "\n") {
		key, value, _ := strings.Cut(line, "=")
		fields[key] = value
	}
	return fields
}

// monotonicNow reads the clock systemd stamps the SIGHUP of a reload with.
func monotonicNow(t *testing.T) uint64 {
	t.Helper()
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &now); err != nil {
		t.Fatal(err)
	}
	return uint64(now.Nano() / 1000)
}

// startsReload reports whether systemd would accept fields as the start of a
// reload whose SIGHUP it sent at since. systemd ignores a RELOADING=1 stamped
// before its signal and waits until the reload times out.
func startsReload(fields map[string]string, since uint64) bool {
	stamp, err := strconv.ParseUint(fields["MONOTONIC_USEC"], 10, 64)
	return fields["RELOADING"] == "1" && err == nil && stamp >= since
}

func hangup(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
}

// serveUnderSystemd runs the listener loop the way katlc agent serve does and
// returns its address once it reports READY=1.
func (n *managementTrialNode) serveUnderSystemd(t *testing.T, notices systemdNotifications) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	probe.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- n.server.serveReloadable(ctx, "tcp", address, notifyReady) }()
	t.Cleanup(func() { cancel(); <-done })
	if ready := notices.next(t); ready["READY"] != "1" {
		t.Fatalf("first notification = %v, want READY=1", ready)
	}
	return address
}

// A revert made outside the agent, by the systemd timer or the console, takes
// effect when systemd reloads the agent with SIGHUP. systemd completes the
// reload only on a RELOADING=1 stamped after its signal followed by READY=1,
// and reports success to the command that asked for it once READY=1 arrives.
func TestHangupReservesManagement(t *testing.T) {
	node := newManagementTrialNode(t)
	node.server.managementTrialPoll = time.Hour
	begun, err := node.server.BeginManagementTransition(t.Context(), node.beginRequest())
	if err != nil {
		t.Fatal(err)
	}
	notices := listenNotifications(t)
	address := node.serveUnderSystemd(t, notices)
	trialClient, plaintext := node.trialClient(t, address), plaintextClient(t, address)
	if !node.answers(t, trialClient) {
		t.Fatal("the agent does not serve the trial")
	}

	if _, reverted, err := nodeidentity.RevertManagementTrial(node.root, begun.TrialId, "reverted from the console", node.clock); err != nil || !reverted {
		t.Fatalf("revert = %v, %v", reverted, err)
	}
	since := monotonicNow(t)
	hangup(t)

	started := false
	for {
		fields := notices.next(t)
		started = started || startsReload(fields, since)
		if fields["READY"] == "1" {
			break
		}
	}
	if !started {
		t.Fatal("READY=1 arrived before a RELOADING=1 stamped after the SIGHUP, so systemd would wait out the reload")
	}
	// READY=1 ends the reload, so the installed mode must already be served.
	if !node.answers(t, plaintext) {
		t.Fatal("installed clients are refused when the agent reports the reload done")
	}
	if node.answers(t, trialClient) {
		t.Fatal("the agent still serves the reverted trial")
	}
}

// A SIGHUP that arrives while the loop is re-serving for an in-process change
// is absorbed by that re-serve, which must still end the reload systemd began.
// The loop has already sent its RELOADING=1, stamped before the signal, so the
// report that ends the re-serve has to carry a later stamp.
func TestHangupDuringReserveEndsReload(t *testing.T) {
	node := newManagementTrialNode(t)
	notices := listenNotifications(t)
	address := node.serveUnderSystemd(t, notices)
	plaintext := plaintextClient(t, address)

	// An in-flight call holds the old listener open while the loop stops it.
	record := createAgentOperation(t, node.server.Store, "op-watch")
	watchCtx, stopWatch := context.WithCancel(t.Context())
	defer stopWatch()
	watch, err := plaintext.WatchOperation(watchCtx, &agentapi.WatchOperationRequest{OperationId: record.OperationID, WatchTimeout: "30s"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Recv(); err != nil {
		t.Fatalf("watch = %v", err)
	}

	node.server.management.reselect()
	if fields := notices.next(t); fields["RELOADING"] != "1" || fields["READY"] == "1" {
		t.Fatalf("notification = %v, want RELOADING=1 while the old listener drains", fields)
	}
	since := monotonicNow(t)
	hangup(t)
	eventually(t, "the SIGHUP is pending", func() bool { return len(node.server.management.changes()) == 1 })
	stopWatch()

	fields := notices.next(t)
	if fields["READY"] != "1" || !startsReload(fields, since) {
		t.Fatalf("notification = %v; systemd would wait out the reload it began at %d", fields, since)
	}
	if !node.answers(t, plaintextClient(t, address)) {
		t.Fatal("the agent does not serve after the re-serve")
	}
}
