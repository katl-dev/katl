//go:build !linux

package kernelmodule

import (
	"context"
	"fmt"
)

func canMountImages() (bool, error) {
	return false, fmt.Errorf("module image preparation requires Linux")
}

func openServiceImage(context.Context, string, string) (serviceImage, error) {
	return serviceImage{}, fmt.Errorf("module image preparation requires Linux")
}
