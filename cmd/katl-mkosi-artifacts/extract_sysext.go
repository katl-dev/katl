package main

import (
	"context"
	"fmt"

	"github.com/katl-dev/katl/internal/sysextimage"
)

func runExtractSysext(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("extract-sysext requires IMAGE DIR")
	}
	return sysextimage.Extract(context.Background(), args[0], args[1])
}
