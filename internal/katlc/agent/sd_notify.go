package agent

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// notifyReady tells systemd that the service is ready. It does nothing when
// the process is not supervised with Type=notify or Type=notify-reload.
func notifyReady() error {
	return notify("READY=1")
}

// notifyReloading tells systemd the agent is re-serving its management
// credentials.
func notifyReloading() error {
	now, err := monotonicUsec()
	if err != nil {
		return err
	}
	return notify(fmt.Sprintf("RELOADING=1\nMONOTONIC_USEC=%d", now))
}

// notifyReloaded reports a finished re-serve. systemd completes a reload it
// began by SIGHUP only on a RELOADING=1 stamped no earlier than the signal, so
// the report carries its own stamp, taken when the re-serve absorbed pending
// signals: a SIGHUP that arrived after an earlier RELOADING=1 is still
// answered.
func notifyReloaded(stamp uint64) error {
	return notify(fmt.Sprintf("RELOADING=1\nREADY=1\nMONOTONIC_USEC=%d", stamp))
}

// monotonicUsec reads CLOCK_MONOTONIC, the clock systemd compares
// MONOTONIC_USEC with.
func monotonicUsec() (uint64, error) {
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &now); err != nil {
		return 0, fmt.Errorf("read monotonic clock: %w", err)
	}
	return uint64(now.Nano() / 1000), nil
}

func notify(state string) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	if path[0] == '@' {
		path = "\x00" + path[1:]
	}
	conn, err := net.Dial("unixgram", path)
	if err != nil {
		return fmt.Errorf("notify systemd: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("notify systemd: %w", err)
	}
	return nil
}
