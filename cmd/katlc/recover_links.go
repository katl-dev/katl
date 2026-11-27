package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/katl-dev/katl/internal/linkrecovery"
	"github.com/katl-dev/katl/internal/managementnetwork"
)

func runAgentRecoverLinks(ctx context.Context, events io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	return linkrecovery.Run(ctx, managementnetwork.ReadState, events)
}
