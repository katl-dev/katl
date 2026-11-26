package main

import "golang.org/x/sys/unix"

const descriptorDirectory = "/proc/self/fd"

func openChildDirectory(fd int, name string) (int, error) {
	// NO_XDEV also refuses bind mounts with the same device number.
	return unix.Openat2(fd, name, &unix.OpenHow{
		Flags:   directoryFlags,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
}

func renameEntry(from *directory, name string, to *directory, destination string) error {
	return unix.Renameat2(from.fd(), name, to.fd(), destination, unix.RENAME_NOREPLACE)
}
