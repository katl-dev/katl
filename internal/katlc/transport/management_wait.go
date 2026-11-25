package transport

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"
)

// ManagementReadyWait is how long a call to a node's management endpoint
// waits for the endpoint to start accepting connections. A node that has just
// booted or restarted its agent can answer on the network before katlc-agent
// listens. Commands that apply their own request timeout add it to that
// timeout.
const ManagementReadyWait = 30 * time.Second

var managementReady = struct{ bound, interval time.Duration }{bound: ManagementReadyWait, interval: time.Second}

type (
	noManagementWaitKey   struct{}
	managementProgressKey struct{}
)

// WithoutManagementWait makes connections dialed with ctx report a refused
// management connection at once. Callers whose purpose is to observe a node
// going down or coming back poll for themselves.
func WithoutManagementWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, noManagementWaitKey{}, true)
}

// WithManagementProgress sends the progress lines of connections dialed with
// ctx to w. Without it, waiting is silent.
func WithManagementProgress(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, managementProgressKey{}, w)
}

// ManagementDialOptions returns the connection options every dial of a
// management endpoint shares. Unless ctx came from WithoutManagementWait,
// calls retry a refused connection for at most ManagementReadyWait, reporting
// progress, and then fail with guidance.
func ManagementDialOptions(ctx context.Context, node, endpoint string) []grpc.DialOption {
	if _, noWait := ctx.Value(noManagementWaitKey{}).(bool); noWait {
		return nil
	}
	var progress io.Writer = io.Discard
	if w, ok := ctx.Value(managementProgressKey{}).(io.Writer); ok && w != nil {
		progress = w
	}
	return []grpc.DialOption{
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 500 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 2 * time.Second},
			MinConnectTimeout: 20 * time.Second,
		}),
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return awaitManagementEndpoint(ctx, progress, node, endpoint, func(p *peer.Peer) error {
				return invoker(ctx, method, req, reply, cc, append(opts[:len(opts):len(opts)], grpc.Peer(p))...)
			})
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			var stream grpc.ClientStream
			err := awaitManagementEndpoint(ctx, progress, node, endpoint, func(p *peer.Peer) (err error) {
				stream, err = streamer(ctx, desc, cc, method, append(opts[:len(opts):len(opts)], grpc.Peer(p))...)
				return err
			})
			return stream, err
		}),
	}
}

// refusedBeforeConnect reports a call that never reached the agent because
// nothing was listening, which is what a starting agent produces. A call that
// had a connection (p.Addr set) may have reached the agent, and retrying it is
// not safe. Handshake and identity failures are excluded because waiting does
// not change their outcome. gRPC reports dial failures as Unavailable with the
// cause only in the message.
func refusedBeforeConnect(err error, p *peer.Peer) bool {
	return p.Addr == nil && grpcstatus.Code(err) == codes.Unavailable &&
		strings.Contains(grpcstatus.Convert(err).Message(), "connect: connection refused")
}

// awaitManagementEndpoint runs attempt until it stops failing with a refused
// connection. Any other error is returned at once, unchanged, except that a
// context which ends after a refusal reports the refusal with its guidance.
func awaitManagementEndpoint(ctx context.Context, progress io.Writer, node, endpoint string, attempt func(*peer.Peer) error) error {
	start := time.Now()
	var refused error
	giveUp := func() error {
		cause := ""
		if ctx.Err() != nil {
			cause = " (" + ctx.Err().Error() + ")"
		}
		return fmt.Errorf("node %s management endpoint %s refused connections for %s%s: katlc-agent is not listening; the node may still be booting, so wait and rerun this command, and if it persists check the address and run 'systemctl status katlc-agent' on the node console: %w", node, endpoint, time.Since(start).Round(time.Second), cause, refused)
	}
	for {
		var p peer.Peer
		err := attempt(&p)
		switch {
		case err == nil:
			return nil
		case refusedBeforeConnect(err, &p):
			refused = err
		case refused != nil && ctx.Err() != nil:
			return giveUp()
		default:
			return err
		}
		if time.Since(start)+managementReady.interval > managementReady.bound {
			return giveUp()
		}
		_, _ = fmt.Fprintf(progress, "Waiting for node %s management endpoint %s to accept connections (%s elapsed)...\n", node, endpoint, time.Since(start).Round(time.Second))
		timer := time.NewTimer(managementReady.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return giveUp()
		case <-timer.C:
		}
	}
}
