// Command katl-vmtest-release-networks removes the libvirt networks of vmtest
// runs once no domain of the run attaches to them.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/katl-dev/katl/internal/vmtest"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: katl-vmtest-release-networks <libvirt-uri> <network>...")
		os.Exit(2)
	}
	if err := vmtest.ReleaseRunNetworks(context.Background(), os.Args[1], os.Args[2:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "vmtest-clean: %v\n", err)
		os.Exit(1)
	}
}
