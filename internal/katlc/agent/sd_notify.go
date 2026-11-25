package agent

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// notifyReady tells systemd that the service is ready, or that a reload has
// finished. It does nothing when the process is not supervised with
// Type=notify or Type=notify-reload.
func notifyReady() error {
	return notify("READY=1")
}

// notifyReloading tells systemd the agent is re-serving its management
// credentials; READY=1 ends the reload.
func notifyReloading() error {
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &now); err != nil {
		return fmt.Errorf("read monotonic clock: %w", err)
	}
	return notify(fmt.Sprintf("RELOADING=1\nMONOTONIC_USEC=%d", now.Nano()/1000))
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
