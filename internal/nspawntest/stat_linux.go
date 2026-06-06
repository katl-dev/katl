//go:build linux

package nspawntest

import (
	"io/fs"
	"syscall"
)

type fileStat struct {
	dev uint64
	ino uint64
}

func fileSys(info fs.FileInfo) (fileStat, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStat{}, false
	}
	return fileStat{dev: uint64(stat.Dev), ino: stat.Ino}, true
}
