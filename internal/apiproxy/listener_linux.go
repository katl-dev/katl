package apiproxy

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func configureListener(network, _ string, conn syscall.RawConn) error {
	var socketErr error
	// Addresses may arrive after startup or disappear during network reconfiguration.
	// Free-bind keeps the exact listener address without requiring global nonlocal binding.
	err := conn.Control(func(fd uintptr) {
		if network == "tcp6" {
			socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_FREEBIND, 1)
		} else {
			socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1)
		}
	})
	if err != nil {
		return err
	}
	return socketErr
}
