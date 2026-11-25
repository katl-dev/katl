package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// managementReadyWait bounds how long katlctl waits for a node's management
// endpoint to start accepting connections. A node that has just booted can
// answer on the network before katlc-agent listens.
var managementReadyWait = struct{ bound, interval time.Duration }{bound: 30 * time.Second, interval: 2 * time.Second}

// managementRefused reports a refused or reset connection, the failures a
// starting agent produces. Handshake and identity failures are not included:
// waiting does not change their outcome. gRPC reports dial failures as
// Unavailable with the cause only in the message.
func managementRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	if grpcstatus.Code(err) != codes.Unavailable {
		return false
	}
	message := grpcstatus.Convert(err).Message()
	return strings.Contains(message, "connection refused") || strings.Contains(message, "connection reset")
}

// awaitManagementEndpoint runs attempt until it stops failing with a refused
// connection, for at most managementReadyWait.bound, and reports progress to
// progress. Any other error is returned at once, unchanged.
func awaitManagementEndpoint(ctx context.Context, progress io.Writer, node, endpoint string, attempt func() error) error {
	start := time.Now()
	for {
		err := attempt()
		if err == nil || !managementRefused(err) {
			return err
		}
		waited := time.Since(start).Round(time.Second)
		if time.Since(start)+managementReadyWait.interval > managementReadyWait.bound {
			return fmt.Errorf("%s refused connections for %s: katlc-agent is not listening; the node may still be booting, so wait and rerun this command, and if it persists check the address and run 'systemctl status katlc-agent' on the node console: %w", endpoint, waited, err)
		}
		_, _ = fmt.Fprintf(progress, "Waiting for node %s management endpoint %s to accept connections (%s elapsed)...\n", node, endpoint, waited)
		timer := time.NewTimer(managementReadyWait.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
