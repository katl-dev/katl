// Package nettest provides loopback fixtures for tests of network clients.
package nettest

import (
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
)

// Port is a loopback TCP port that refuses connections until Listen is
// called. It is bound from the start, so no other process can take it in the
// meantime, which closing and later re-binding a listener cannot guarantee.
type Port struct {
	Endpoint string
	fd       int
}

// ReservePort binds a loopback port without listening on it.
func ReservePort(t *testing.T) *Port {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	port := &Port{fd: fd}
	t.Cleanup(func() {
		if port.fd >= 0 {
			_ = syscall.Close(port.fd)
		}
	})
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	port.Endpoint = net.JoinHostPort("127.0.0.1", strconv.Itoa(name.(*syscall.SockaddrInet4).Port))
	return port
}

// Listen starts accepting connections on the reserved port. It is safe to call
// from any goroutine; the caller closes the listener.
func (p *Port) Listen() (net.Listener, error) {
	if err := syscall.Listen(p.fd, 128); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(p.fd), p.Endpoint)
	p.fd = -1
	defer file.Close()
	return net.FileListener(file)
}
