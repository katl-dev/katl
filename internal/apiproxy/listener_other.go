//go:build !linux

package apiproxy

import "syscall"

func configureListener(string, string, syscall.RawConn) error {
	return nil
}
