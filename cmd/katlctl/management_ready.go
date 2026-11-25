package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// managementReadyWait bounds how long a katlctl call waits for a node's
// management endpoint to start accepting connections. A node that has just
// booted or restarted its agent can answer on the network before katlc-agent
// listens.
var managementReadyWait = struct{ bound, interval time.Duration }{bound: 30 * time.Second, interval: time.Second}

// managementWaitProgress receives a line for each retry of a refused
// connection.
var managementWaitProgress io.Writer = os.Stderr

type noManagementWaitKey struct{}

// withoutManagementWait makes connections dialed with ctx report a refused
// management connection at once. Callers whose purpose is to observe a node
// going down or coming back poll for themselves.
func withoutManagementWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, noManagementWaitKey{}, true)
}

// managementRefused reports a connection that never reached the agent because
// nothing was listening, which is what a starting agent produces. Handshake and
// identity failures and errors on an established connection are excluded:
// waiting does not change their outcome, and retrying a call that may have
// reached the agent is not safe. gRPC reports dial failures as Unavailable with
// the cause only in the message.
func managementRefused(err error) bool {
	if grpcstatus.Code(err) != codes.Unavailable {
		return false
	}
	message := grpcstatus.Convert(err).Message()
	return strings.HasPrefix(message, "connection error: desc = ") && strings.Contains(message, "Error while dialing") &&
		(strings.Contains(message, "connection refused") || strings.Contains(message, "connection reset"))
}

// managementDialOptions returns the connection options shared by every katlctl
// call to a management endpoint. Calls retry a refused connection for at most
// managementReadyWait.bound, reporting progress, and then fail with guidance.
func managementDialOptions(node, endpoint string, wait bool) []grpc.DialOption {
	if !wait {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoff.Config{BaseDelay: 500 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 2 * time.Second}}),
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return awaitManagementEndpoint(ctx, node, endpoint, func() error {
				return invoker(ctx, method, req, reply, cc, opts...)
			})
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			var stream grpc.ClientStream
			err := awaitManagementEndpoint(ctx, node, endpoint, func() (err error) {
				stream, err = streamer(ctx, desc, cc, method, opts...)
				return err
			})
			return stream, err
		}),
	}
}

// awaitManagementEndpoint runs attempt until it stops failing with a refused
// connection. Any other error is returned at once, unchanged.
func awaitManagementEndpoint(ctx context.Context, node, endpoint string, attempt func() error) error {
	start := time.Now()
	for {
		err := attempt()
		if err == nil || !managementRefused(err) {
			return err
		}
		waited := time.Since(start).Round(time.Second)
		if time.Since(start)+managementReadyWait.interval > managementReadyWait.bound {
			return fmt.Errorf("node %s management endpoint %s refused connections for %s: katlc-agent is not listening; the node may still be booting, so wait and rerun this command, and if it persists check the address and run 'systemctl status katlc-agent' on the node console: %w", node, endpoint, waited, err)
		}
		_, _ = fmt.Fprintf(managementWaitProgress, "Waiting for node %s management endpoint %s to accept connections (%s elapsed)...\n", node, endpoint, waited)
		timer := time.NewTimer(managementReadyWait.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}
