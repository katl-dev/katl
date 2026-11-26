package main

import "golang.org/x/sys/unix"

const descriptorDirectory = "/dev/fd"

func openChildDirectory(fd int, name string) (int, error) {
	return unix.Openat(fd, name, directoryFlags, 0)
}

func renameEntry(from *directory, name string, to *directory, destination string) error {
	return unix.RenameatxNp(from.fd(), name, to.fd(), destination, unix.RENAME_EXCL)
}
