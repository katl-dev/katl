package systemdunit

import (
	"net"
	"os"
	"strings"
)

// Notify sends a sd_notify(3) payload, such as READY=1 or STATUS=..., to the
// service manager. It does nothing when the process is not supervised with a
// notification socket.
func Notify(payload string) error {
	socket := strings.TrimSpace(os.Getenv("NOTIFY_SOCKET"))
	if socket == "" {
		return nil
	}
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + strings.TrimPrefix(socket, "@")
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(payload))
	return err
}
