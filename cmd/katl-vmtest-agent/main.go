package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/katl-dev/katl/internal/vmtest"
	"github.com/katl-dev/katl/internal/vmtest/image/kubelethealth"
)

var (
	version = "0.0.0-dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == kubelethealth.Command {
		if err := runKubeletHealth(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kubelet-health:", err)
			os.Exit(1)
		}
		return
	}

	port := flag.Uint("port", uint(vmtest.DefaultAgentPort), "vsock port for the vmtest agent")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := vmtest.NewAgentServer(fmt.Sprintf("%s-%s-%s", version, commit, date))
	err := vmtest.ListenVSock(ctx, uint32(*port), func(conn *os.File) {
		if err := server.Serve(ctx, conn); err != nil {
			fmt.Fprintf(os.Stderr, "katl-vmtest-agent: %v\n", err)
		}
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "katl-vmtest-agent: listen failed: %v\n", err)
		os.Exit(1)
	}
}
