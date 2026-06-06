//go:build !linux

package nspawntest

import "io/fs"

type fileStat struct {
	dev uint64
	ino uint64
}

func fileSys(fs.FileInfo) (fileStat, bool) {
	return fileStat{}, false
}
