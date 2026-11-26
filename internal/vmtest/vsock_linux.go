//go:build linux

package vmtest

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// vsockNotifySocket accepts guest connections on a host vsock port that the
// kernel chooses, so concurrent runs on one host never contend for a port.
type vsockNotifySocket struct {
	file *os.File
	conn syscall.RawConn
}

func listenVSockNotify() (notifySocket, uint32, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	fail := func(err error) (notifySocket, uint32, error) {
		_ = unix.Close(fd)
		return nil, 0, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: unix.VMADDR_PORT_ANY}); err != nil {
		return fail(err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		return fail(err)
	}
	addr, err := unix.Getsockname(fd)
	if err != nil {
		return fail(err)
	}
	vm, ok := addr.(*unix.SockaddrVM)
	if !ok {
		return fail(fmt.Errorf("vsock listener has unexpected address %T", addr))
	}
	// A non-blocking descriptor makes the file pollable, so Close interrupts a
	// pending accept and accepted connections honour read deadlines.
	file := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-notify-%d", vm.Port))
	conn, err := file.SyscallConn()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return &vsockNotifySocket{file: file, conn: conn}, vm.Port, nil
}

func (s *vsockNotifySocket) accept() (notifyConn, string, error) {
	var nfd int
	var peer unix.Sockaddr
	var acceptErr error
	err := s.conn.Read(func(fd uintptr) bool {
		nfd, peer, acceptErr = unix.Accept4(int(fd), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		return acceptErr != unix.EAGAIN && acceptErr != unix.EINTR
	})
	if err != nil {
		return nil, "", err
	}
	if acceptErr != nil {
		return nil, "", acceptErr
	}
	name := "vsock"
	if vm, ok := peer.(*unix.SockaddrVM); ok {
		name = fmt.Sprintf("vsock:%d:%d", vm.CID, vm.Port)
	}
	return os.NewFile(uintptr(nfd), name), name, nil
}

func (s *vsockNotifySocket) Close() error {
	return s.file.Close()
}

func DialVSock(ctx context.Context, cid, port uint32) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-%d-%d", cid, port))
	err = unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port})
	if err == unix.EINPROGRESS || err == unix.EINTR {
		err = waitVSockConnect(ctx, file)
	}
	if ctx.Err() != nil {
		_ = file.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func waitVSockConnect(ctx context.Context, file *os.File) error {
	conn, err := file.SyscallConn()
	if err != nil {
		return err
	}
	// Closing a pollable file wakes RawConn.Write without waiting for the
	// kernel's connection timeout.
	stop := context.AfterFunc(ctx, func() { _ = file.Close() })
	defer stop()
	var connectErr error
	err = conn.Write(func(fd uintptr) bool {
		var status int
		status, connectErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_ERROR)
		if connectErr != nil {
			return true
		}
		if status != 0 {
			connectErr = syscall.Errno(status)
			return true
		}
		// RawConn may call us before the socket becomes writable. SO_ERROR
		// can be zero while the handshake is still pending.
		_, connectErr = unix.Getpeername(int(fd))
		return connectErr != unix.ENOTCONN
	})
	if err != nil {
		return err
	}
	return connectErr
}

func ListenVSock(ctx context.Context, port uint32, ready func() error, handler func(*os.File)) error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		return err
	}
	if err := unix.Listen(fd, 16); err != nil {
		return err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return err
	}
	if err := ready(); err != nil {
		return fmt.Errorf("notify vsock readiness: %w", err)
	}
	pollFDs := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				if _, err := unix.Poll(pollFDs, 100); err != nil && err != unix.EINTR {
					return err
				}
				continue
			}
			return err
		}
		go handler(os.NewFile(uintptr(nfd), "vmtest-agent-conn"))
	}
}
