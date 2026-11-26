//go:build !linux

package efiboot

import "errors"

type Reader struct{}

func (Reader) Read(string) (Entries, error) {
	return Entries{}, errors.New("systemd-boot EFI variable observation requires Linux")
}
