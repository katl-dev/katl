package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
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

// ErrManagementRPCInconclusive means the call budget ended before its result
// could be used to establish what the node did.
var ErrManagementRPCInconclusive = errors.New("management RPC outcome is inconclusive")

type ManagementEvidence uint8

const (
	ManagementInconclusive ManagementEvidence = iota
	ManagementConfirmed
	ManagementRefusedExplicitly
)

// ClassifyManagementEvidence classifies an RPC result for a trust change.
// An old-client-CA probe requires issuer-specific evidence. A gRPC
// authentication refusal does not identify which CA the server trusts.
func ClassifyManagementEvidence(err error, oldAuthorityRefused func(error) bool) ManagementEvidence {
	switch {
	case err == nil:
		return ManagementConfirmed
	case errors.Is(err, ErrManagementRPCInconclusive):
		return ManagementInconclusive
	case oldAuthorityRefused != nil && grpcstatus.Code(err) == codes.Unavailable && oldAuthorityRefused(err):
		return ManagementRefusedExplicitly
	default:
		return ManagementInconclusive
	}
}

type managementRPCInconclusive struct{ cause error }

func (e managementRPCInconclusive) Error() string {
	return ErrManagementRPCInconclusive.Error() + ": " + e.cause.Error()
}

func (e managementRPCInconclusive) Unwrap() error { return e.cause }
func (e managementRPCInconclusive) Is(target error) bool {
	return target == ErrManagementRPCInconclusive
}

func (e managementRPCInconclusive) GRPCStatus() *grpcstatus.Status {
	return grpcstatus.New(grpcstatus.FromContextError(e.cause).Code(), e.Error())
}

func managementRPCResult(ctx context.Context, result error) error {
	if err := ManagementContextError(ctx); err != nil {
		return managementRPCInconclusive{cause: err}
	}
	return result
}

// ManagementContextError checks whether a command or RPC result arrived within
// its budget, even if the deadline elapsed before cancellation was delivered.
func ManagementContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

type (
	noManagementWaitKey   struct{}
	managementProgressKey struct{}
	noManagementAdviceKey struct{}
)

// WithoutManagementAdvice lets trust-changing commands report uncertainty
// without the transport's normal hints about which credentials to select.
func WithoutManagementAdvice(ctx context.Context) context.Context {
	return context.WithValue(ctx, noManagementAdviceKey{}, true)
}

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
// management endpoint shares, including its transport credentials. A plaintext
// (trusted-network) client that reaches an mTLS node is told to select the
// cluster's configuration. Unless ctx came from WithoutManagementWait, calls
// also retry a refused connection for at most ManagementReadyWait, reporting
// progress, and then fail with guidance.
func ManagementDialOptions(ctx context.Context, node, endpoint string, creds credentials.TransportCredentials) []grpc.DialOption {
	var options []grpc.DialOption
	if probe, ok := creds.(*clientCertificateProbe); ok {
		options = append(options, grpc.WithContextDialer(probe.dialContext))
	}
	plaintext := creds.Info().SecurityProtocol == insecure.NewCredentials().Info().SecurityProtocol
	noAdvice, _ := ctx.Value(noManagementAdviceKey{}).(bool)
	if wrapped, ok := creds.(*clientCredentials); noAdvice && ok {
		creds = wrapped.TransportCredentials
	}
	if observation, ok := ctx.Value(managementObservationKey{}).(*ManagementObservation); ok {
		creds = &observedCredentials{TransportCredentials: creds, observation: observation, plaintext: plaintext}
	}
	options = append(options, grpc.WithTransportCredentials(creds))
	if plaintext && !noAdvice {
		options = append(
			options,
			grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
				return explainMutualTLSNode(invoker(ctx, method, req, reply, cc, opts...))
			}),
			grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
				stream, err := streamer(ctx, desc, cc, method, opts...)
				return stream, explainMutualTLSNode(err)
			}),
		)
	}
	if _, noWait := ctx.Value(noManagementWaitKey{}).(bool); !noWait {
		var progress io.Writer = io.Discard
		if w, ok := ctx.Value(managementProgressKey{}).(io.Writer); ok && w != nil {
			progress = w
		}
		options = append(options, managementReadyDialOptions(progress, node, endpoint)...)
	}
	return append(
		options,
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return managementRPCResult(ctx, invoker(ctx, method, req, reply, cc, opts...))
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			stream, err := streamer(ctx, desc, cc, method, opts...)
			if err := managementRPCResult(ctx, err); err != nil {
				return nil, err
			}
			return managementRPCStream{ClientStream: stream, ctx: ctx}, nil
		}),
	)
}

type managementRPCStream struct {
	grpc.ClientStream
	ctx context.Context
}

func (s managementRPCStream) SendMsg(msg any) error {
	return managementRPCResult(s.ctx, s.ClientStream.SendMsg(msg))
}

func (s managementRPCStream) RecvMsg(msg any) error {
	return managementRPCResult(s.ctx, s.ClientStream.RecvMsg(msg))
}

func (s managementRPCStream) CloseSend() error {
	return managementRPCResult(s.ctx, s.ClientStream.CloseSend())
}

func managementReadyDialOptions(progress io.Writer, node, endpoint string) []grpc.DialOption {
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

// TLSAtPlaintext reports whether a plaintext call failed the way it does
// against a TLS listener. A TLS listener closes the connection before sending
// its HTTP/2 server preface, which gRPC reports only as a generic unavailable
// error. Any endpoint that accepts TCP and then closes or speaks something
// other than HTTP/2 produces the same error, so a true result names a likely
// cause rather than a certainty. Use it only for diagnostics.
func TLSAtPlaintext(err error) bool {
	return grpcstatus.Code(err) == codes.Unavailable && strings.Contains(err.Error(), "server preface")
}

// explainMutualTLSNode maps the failure of a plaintext call to a TLS listener.
func explainMutualTLSNode(err error) error {
	if TLSAtPlaintext(err) {
		return fmt.Errorf("node did not answer plaintext gRPC; it likely serves mTLS management. If `katlctl management enable-mtls` moved it to mTLS without finishing, rerun that command; otherwise select its mTLS configuration (--config or --context), or check that management.address and management.authentication match the node and that the address points at the katlc agent on port %d: %w", inventory.ManagementPort, err)
	}
	return err
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
	giveUp := func(err error) error {
		return fmt.Errorf("node %s management endpoint %s refused connections for %s: katlc-agent is not listening; the node may still be booting, so wait and rerun this command, and if it persists check the address and run 'systemctl status katlc-agent' on the node console: %w", node, endpoint, time.Since(start).Round(time.Second), err)
	}
	for {
		var p peer.Peer
		err := attempt(&p)
		switch {
		case err == nil:
			return nil
		case refusedBeforeConnect(err, &p):
			refused = err
		case refused != nil && errors.Is(err, ErrManagementRPCInconclusive):
			return giveUp(err)
		default:
			return err
		}
		if time.Since(start)+managementReady.interval > managementReady.bound {
			return giveUp(managementRPCResult(ctx, refused))
		}
		_, _ = fmt.Fprintf(progress, "Waiting for node %s management endpoint %s to accept connections (%s elapsed)...\n", node, endpoint, time.Since(start).Round(time.Second))
		timer := time.NewTimer(managementReady.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return giveUp(managementRPCResult(ctx, refused))
		case <-timer.C:
		}
	}
}
