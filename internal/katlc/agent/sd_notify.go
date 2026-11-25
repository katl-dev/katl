package agent

import (
	"fmt"
	"net"
	"os"
)

// notifyReady tells systemd that the service is ready. It does nothing when
// the process is not supervised with Type=notify.
func notifyReady() error {
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
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("notify systemd: %w", err)
	}
	return nil
}
