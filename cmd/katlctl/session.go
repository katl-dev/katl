package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

type managementSession struct {
	ctx    context.Context
	target managementTarget
	client agentapi.KatlcAgentClient
	close  func()
}

func openManagementSession(ctx context.Context, opts managementTargetOptions, timeout time.Duration) (*managementSession, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("--timeout must be positive")
	}
	target, err := resolveManagementTarget(ctx, opts)
	if err != nil {
		return nil, err
	}

	requestCtx, cancel := context.WithTimeout(withManagementTarget(ctx, target), timeout)
	conn, err := dialKatlcAgent(requestCtx, target.endpoint)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connect to %s at %s: %w", hostTargetName(target), target.endpoint, err)
	}

	return &managementSession{
		ctx: requestCtx, target: target, client: conn.Client,
		// Reboot and shutdown release the request before waiting for recovery.
		close: sync.OnceFunc(func() {
			cancel()
			_ = conn.Close()
		}),
	}, nil
}
