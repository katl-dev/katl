package vmtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// systemd's service manager reports its own lifecycle to a hypervisor named by
// the vmm.notify_socket system credential. The harness passes that credential
// as an SMBIOS OEM string and listens on a host vsock port, so it observes the
// guest's PID 1 without parsing serial output or adding anything to the image.
//
// Each sd_notify message arrives on its own stream connection. The sender
// half-closes the connection and then blocks until the receiver closes it, so
// a slow receiver stalls PID 1. The listener therefore reads each connection
// to EOF under a short deadline and closes it before accepting the next one,
// which also keeps events in the order PID 1 sent them.
const (
	notifyCredential      = "io.systemd.credential:vmm.notify_socket"
	notifyHostCID         = 2
	notifyReadTimeout     = 5 * time.Second
	notifyMaxMessageBytes = 64 << 10
)

func notifySocketCredential(port uint32) string {
	return fmt.Sprintf("%s=vsock-stream:%d:%d", notifyCredential, notifyHostCID, port)
}

// notifyEvent is one PID 1 lifecycle report. The log keeps only readiness,
// reached units and shutdown; PID 1 also sends job STATUS= heartbeats several
// times a second, which are not lifecycle events.
type notifyEvent struct {
	Received   time.Time `json:"received"`
	Peer       string    `json:"peer,omitempty"`
	Ready      bool      `json:"ready,omitempty"`
	UnitActive string    `json:"unitActive,omitempty"`
	Shutdown   string    `json:"shutdown,omitempty"`
	ExitStatus string    `json:"exitStatus,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func parseNotifyMessage(data []byte) (map[string]string, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("notify message line %q is not a KEY=VALUE assignment", line)
		}
		fields[key] = value
	}
	if len(fields) == 0 {
		return nil, errors.New("notify message is empty")
	}
	return fields, nil
}

// lifecycleEvent reports whether a message carries a lifecycle event.
func lifecycleEvent(fields map[string]string) (notifyEvent, bool) {
	event := notifyEvent{
		Ready:      fields["READY"] == "1",
		UnitActive: fields["X_SYSTEMD_UNIT_ACTIVE"],
		Shutdown:   fields["X_SYSTEMD_SHUTDOWN"],
	}
	if event.Shutdown != "" {
		event.ExitStatus = fields["EXIT_STATUS"]
	}
	return event, event.Ready || event.UnitActive != "" || event.Shutdown != ""
}

type notifyConn interface {
	io.ReadCloser
	SetReadDeadline(time.Time) error
}

type notifySocket interface {
	accept() (notifyConn, string, error)
	Close() error
}

// serveNotify appends each lifecycle event to log until the socket is closed.
// Messages that cannot be read or parsed are logged as errors so the record
// shows what the guest attempted.
func serveNotify(socket notifySocket, log io.Writer, now func() time.Time) {
	encoder := json.NewEncoder(log)
	for {
		conn, peer, err := socket.accept()
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(now().Add(notifyReadTimeout))
		data, readErr := io.ReadAll(io.LimitReader(conn, notifyMaxMessageBytes))
		_ = conn.Close()

		var event notifyEvent
		fields, err := parseNotifyMessage(data)
		switch {
		case readErr != nil:
			event.Error = readErr.Error()
		case err != nil:
			event.Error = err.Error()
		default:
			var ok bool
			if event, ok = lifecycleEvent(fields); !ok {
				continue
			}
		}
		event.Received = now()
		event.Peer = peer
		_ = encoder.Encode(event)
	}
}
