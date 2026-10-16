package kernelmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func canMountImages() (bool, error) {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&header, &data[0]); err != nil {
		return false, fmt.Errorf("inspect image mount capability: %w", err)
	}
	return data[unix.CAP_SYS_ADMIN/32].Effective&(1<<(unix.CAP_SYS_ADMIN%32)) != 0, nil
}

// openServiceImage delegates read-only image access to systemd while keeping
// filesystem inspection and module policy in the caller. Mount descriptors can
// be traversed directly; attaching them would require mount privileges.
func openServiceImage(ctx context.Context, address, path string) (view serviceImage, resultErr error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	if err != nil {
		return serviceImage{}, fmt.Errorf("connect to image service (systemd-mountfsd.socket must be available): %w", err)
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	defer func() {
		if ctx.Err() != nil && resultErr != nil {
			resultErr = ctx.Err()
		}
	}()

	image, err := os.Open(path)
	if err != nil {
		return serviceImage{}, err
	}
	defer image.Close()
	request := []byte(`{"method":"io.systemd.MountFileSystem.MountImage","parameters":{"imageFileDescriptor":0,"readOnly":true,"growFileSystems":false}}` + "\x00")
	unixConnection := connection.(*net.UnixConn)
	n, _, err := unixConnection.WriteMsgUnix(request, unix.UnixRights(int(image.Fd())), nil)
	if err != nil {
		return serviceImage{}, err
	}
	if n != len(request) {
		return serviceImage{}, io.ErrShortWrite
	}

	var descriptors []*os.File
	defer func() {
		for _, descriptor := range descriptors {
			if descriptor != view.descriptor {
				_ = descriptor.Close()
			}
		}
	}()
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return serviceImage{}, err
	}
	var message []byte
	buffer, control := make([]byte, 4096), make([]byte, unix.CmsgSpace(256*4))
	for len(message) < 1<<20 {
		var count, controlCount, flags int
		var readErr error
		err := raw.Read(func(fd uintptr) bool {
			// Receive descriptors with CLOEXEC atomically; concurrent children
			// must not retain image mounts after this preparation ends.
			count, controlCount, flags, _, readErr = unix.Recvmsg(int(fd), buffer, control, unix.MSG_CMSG_CLOEXEC)
			return readErr != unix.EAGAIN && readErr != unix.EWOULDBLOCK
		})
		if err != nil {
			return serviceImage{}, err
		}
		if readErr != nil {
			return serviceImage{}, readErr
		}
		messages, err := unix.ParseSocketControlMessage(control[:controlCount])
		if err != nil {
			return serviceImage{}, fmt.Errorf("parse image descriptors: %w", err)
		}
		for _, message := range messages {
			fds, err := unix.ParseUnixRights(&message)
			if err != nil {
				return serviceImage{}, fmt.Errorf("parse image descriptors: %w", err)
			}
			for _, fd := range fds {
				descriptors = append(descriptors, os.NewFile(uintptr(fd), "module-image"))
			}
		}
		if len(descriptors) > 256 {
			return serviceImage{}, fmt.Errorf("image service returned too many descriptors")
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
			return serviceImage{}, fmt.Errorf("truncated image service reply")
		}
		if count == 0 {
			return serviceImage{}, io.ErrUnexpectedEOF
		}
		message = append(message, buffer[:count]...)
		if len(message) > 1<<20 {
			return serviceImage{}, fmt.Errorf("image service reply exceeds 1 MiB")
		}
		end := bytes.IndexByte(message, 0)
		if end < 0 {
			continue
		}
		var reply struct {
			Error      string `json:"error"`
			Parameters struct {
				Partitions []struct {
					Designator          string `json:"designator"`
					Writable            *bool  `json:"writable"`
					MountFileDescriptor *int   `json:"mountFileDescriptor"`
				} `json:"partitions"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(message[:end], &reply); err != nil {
			return serviceImage{}, fmt.Errorf("decode image service reply: %w", err)
		}
		if reply.Error != "" {
			return serviceImage{}, fmt.Errorf("image service refused input: %s", reply.Error)
		}
		if len(reply.Parameters.Partitions) != 1 {
			return serviceImage{}, fmt.Errorf("module input must contain one read-only root filesystem")
		}
		partition := reply.Parameters.Partitions[0]
		index := partition.MountFileDescriptor
		if partition.Designator != "root" || partition.Writable == nil || *partition.Writable || index == nil || *index < 0 || *index >= len(descriptors) {
			return serviceImage{}, fmt.Errorf("image service returned an invalid read-only root descriptor")
		}
		root := descriptors[*index]
		info, err := root.Stat()
		if err != nil || !info.IsDir() {
			return serviceImage{}, fmt.Errorf("image service root descriptor is not a directory")
		}
		// Use the owner's PID so subprocesses such as modinfo can read this
		// descriptor despite CLOEXEC. The final slash makes WalkDir see a
		// directory rather than the procfs descriptor symlink.
		return serviceImage{root: fmt.Sprintf("/proc/%d/fd/%d/", os.Getpid(), root.Fd()), descriptor: root}, nil
	}
	return serviceImage{}, fmt.Errorf("image service reply exceeds 1 MiB")
}
