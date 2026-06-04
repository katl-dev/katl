//go:build linux

package vmtest

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func DialVSock(ctx context.Context, cid, port uint32) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		done <- unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port})
	}()
	select {
	case err := <-done:
		if err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		return os.NewFile(uintptr(fd), fmt.Sprintf("vsock-%d-%d", cid, port)), nil
	case <-ctx.Done():
		_ = unix.Close(fd)
		<-done
		return nil, ctx.Err()
	}
}

func ListenVSock(ctx context.Context, port uint32, handler func(*os.File)) error {
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
	go func() {
		<-ctx.Done()
		_ = unix.Close(fd)
	}()
	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go handler(os.NewFile(uintptr(nfd), "vmtest-agent-conn"))
	}
}
