package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/katl-dev/katl/internal/kubeletstartup"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if _, err := kubeletstartup.Prepare(ctx, "/", os.Getenv("INVOCATION_ID")); err != nil {
		fmt.Fprintf(os.Stderr, "katl-kubelet-startup: %v\n", err)
		os.Exit(1)
	}
}
