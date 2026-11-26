package vmtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
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

// notifyEvent is one PID 1 report. Only messages that carry readiness, a unit
// state change, shutdown or a job STATUS are kept; other messages such as
// X_SYSTEMD_SIGNALS_LEVEL are not progress.
type notifyEvent struct {
	Received     time.Time `json:"received"`
	Peer         string    `json:"peer,omitempty"`
	Ready        bool      `json:"ready,omitempty"`
	Status       string    `json:"status,omitempty"`
	UnitActive   string    `json:"unitActive,omitempty"`
	UnitInactive string    `json:"unitInactive,omitempty"`
	Shutdown     string    `json:"shutdown,omitempty"`
	ExitStatus   string    `json:"exitStatus,omitempty"`
	Error        string    `json:"error,omitempty"`
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

// progressEvent reports whether a message carries progress.
func progressEvent(fields map[string]string) (notifyEvent, bool) {
	event := notifyEvent{
		Ready:        fields["READY"] == "1",
		Status:       fields["STATUS"],
		UnitActive:   fields["X_SYSTEMD_UNIT_ACTIVE"],
		UnitInactive: fields["X_SYSTEMD_UNIT_INACTIVE"],
		Shutdown:     fields["X_SYSTEMD_SHUTDOWN"],
	}
	if event.Shutdown != "" {
		event.ExitStatus = fields["EXIT_STATUS"]
	}
	return event, event.Ready || event.Status != "" || event.UnitActive != "" || event.UnitInactive != "" || event.Shutdown != ""
}

type notifyConn interface {
	io.ReadCloser
	SetReadDeadline(time.Time) error
}

type notifySocket interface {
	accept() (notifyConn, string, error)
	Close() error
}

// serveNotify passes each progress event to record until the socket is
// closed. Messages that cannot be read or parsed are recorded as errors so the
// record shows what the guest attempted.
func serveNotify(socket notifySocket, record func(notifyEvent), now func() time.Time) {
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
			if event, ok = progressEvent(fields); !ok {
				continue
			}
		}
		event.Received = now()
		event.Peer = peer
		record(event)
	}
}

// notifyRecorder keeps the events of one run in memory for the harness and
// appends them to the run directory for post-mortem reading.
type notifyRecorder struct {
	mu     sync.Mutex
	events []notifyEvent
	log    *json.Encoder
	change chan struct{}
}

func (r *notifyRecorder) record(event notifyEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	_ = r.log.Encode(event)
	if r.change != nil {
		close(r.change)
		r.change = nil
	}
}

func (r *notifyRecorder) snapshot() []notifyEvent {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notifyEvent(nil), r.events...)
}

// waitForUnitActive starts at a cursor captured before the operation that
// should activate the unit. It also sees events that arrived before the wait.
func (r *notifyRecorder) waitForUnitActive(ctx context.Context, vmDone <-chan struct{}, unit string, cursor int) (int, error) {
	if r == nil {
		return cursor, errors.New("VM has no systemd notification listener")
	}
	for {
		if err := context.Cause(ctx); err != nil {
			return cursor, err
		}
		r.mu.Lock()
		for ; cursor < len(r.events); cursor++ {
			if r.events[cursor].UnitActive == unit {
				r.mu.Unlock()
				return cursor + 1, nil
			}
		}
		if r.change == nil {
			r.change = make(chan struct{})
		}
		change := r.change
		r.mu.Unlock()
		select {
		case <-change:
		case <-vmDone:
			return cursor, errors.New("VM exited")
		case <-ctx.Done():
			return cursor, context.Cause(ctx)
		}
	}
}
