//go:build !linux

package vmtest

import (
	"context"
	"errors"
	"os"
)

func DialVSock(context.Context, uint32, uint32) (*os.File, error) {
	return nil, errors.New("vmtest vsock is only supported on linux")
}

func ListenVSock(context.Context, uint32, func(*os.File)) error {
	return errors.New("vmtest vsock is only supported on linux")
}
