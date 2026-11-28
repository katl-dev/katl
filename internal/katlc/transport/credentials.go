package transport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/managementidentity"
	"golang.org/x/net/http2"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type clientCredentials struct {
	credentials.TransportCredentials
}

// NewClientCredentials keeps TLS verification errors actionable before gRPC
// turns handshake errors into untyped connection-status messages.
func NewClientCredentials(config *tls.Config) *clientCredentials {
	return &clientCredentials{TransportCredentials: credentials.NewTLS(config)}
}

// ManagementAuthorityRemedy is what to do when a node presents a certificate
// that the configured management authority did not sign.
const ManagementAuthorityRemedy = "after rotation use the updated ClusterConfig and refresh saved contexts, otherwise restore the installed authority's secrets or, if they are lost, run katlctl management identity recover --config CLUSTER.yaml"

func (c *clientCredentials) ClientHandshake(ctx context.Context, authority string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	secure, info, err := c.TransportCredentials.ClientHandshake(ctx, authority, conn)
	var wrongName x509.HostnameError
	var plaintext tls.RecordHeaderError
	switch {
	case unknownServerAuthority(err):
		return nil, nil, fmt.Errorf("node uses a different management authority; %s: %w", ManagementAuthorityRemedy, err)
	case errors.As(err, &plaintext) && plaintext.Conn != nil:
		return nil, nil, fmt.Errorf("management endpoint answered without TLS, so the node still serves trusted-network management. Run or rerun `katlctl management enable-mtls` to move it to mTLS, or set `managementAuthentication: trusted-network` to keep managing it that way. Otherwise check that management.address points at the node's katlc agent on %d: %w", inventory.ManagementPort, err)
	case errors.As(err, &wrongName):
		return nil, nil, fmt.Errorf("management address answered with a certificate for another node; check the node name and management.address in the cluster configuration")
	default:
		return secure, info, err
	}
}

// unknownServerAuthority reports a handshake that rejected the server's
// certificate because the configured authority did not sign it.
func unknownServerAuthority(err error) bool {
	var unknownCA x509.UnknownAuthorityError
	return errors.As(err, &unknownCA)
}

func (c *clientCredentials) Clone() credentials.TransportCredentials {
	return &clientCredentials{TransportCredentials: c.TransportCredentials.Clone()}
}

// ObserveUnknownClientAuthority supplies credentials for one probe call.
// It recognizes an unknown_ca alert in that call's result only if the TLS
// handshake authenticated the server and observed the alert within ctx.
func ObserveUnknownClientAuthority(ctx context.Context, config *tls.Config, certificate tls.Certificate) (credentials.TransportCredentials, func(error) bool) {
	tag := "[issuer probe " + rand.Text() + "]"
	probe := &clientCertificateProbe{
		TransportCredentials: credentials.NewTLS(config),
		config:               config.Clone(),
		certificate:          certificate,
		ctx:                  ctx,
		refusalTag:           tag,
		dialed:               new(atomic.Bool),
	}
	// gRPC flattens the handshake error into the RPC status. The per-call
	// tag keeps the evidence on that specific error through the conversion.
	return probe, func(err error) bool {
		return strings.Contains(err.Error(), tag)
	}
}

type clientCertificateProbe struct {
	credentials.TransportCredentials
	config      *tls.Config
	certificate tls.Certificate
	ctx         context.Context
	refusalTag  string
	dialed      *atomic.Bool
}

func (c *clientCertificateProbe) Clone() credentials.TransportCredentials {
	clone := *c
	clone.TransportCredentials = c.TransportCredentials.Clone()
	return &clone
}

func (c *clientCertificateProbe) dialContext(ctx context.Context, address string) (net.Conn, error) {
	// Claim the first TCP attempt, even if it fails before a TLS handshake.
	if !c.dialed.CompareAndSwap(false, true) {
		return nil, errors.New("issuer probe already attempted a connection")
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

func (c *clientCertificateProbe) ClientHandshake(ctx context.Context, address string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	config := c.config.Clone()
	config.ServerName = c.Info().ServerName
	requested := false
	// Offer the leaf even when its issuer is absent from the server's list.
	config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		requested = true
		return &c.certificate, nil
	}
	secure, info, err := credentials.NewTLS(config).ClientHandshake(ctx, address, conn)
	if err != nil {
		return nil, nil, err
	}

	// A TLS 1.3 client can finish its handshake before the server checks
	// its certificate. Read the alert or the start of the server preface
	// before gRPC writes its own preface to a potentially closed connection.
	reader := bufio.NewReader(secure)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	_, err = reader.Peek(1)
	if !stop() {
		return nil, nil, ctx.Err()
	}
	if err == nil {
		return &clientCertificateProbeConn{Conn: secure, reader: reader}, info, nil
	}
	// crypto/tls wraps a TCP peer's alert in a net.OpError with an
	// unexported alert type.
	var remote *net.OpError
	if requested && ManagementContextError(c.ctx) == nil && errors.As(err, &remote) &&
		remote.Op == "remote error" && remote.Err.Error() == "tls: unknown certificate authority" {
		return nil, nil, fmt.Errorf("%w %s", err, c.refusalTag)
	}
	return nil, nil, err
}

type clientCertificateProbeConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *clientCertificateProbeConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// ClientCredentialsForNode selects exactly one configured transport; it never retries without TLS.
func ClientCredentialsForNode(identity managementidentity.ClientCredentials, node string) (*clientCredentials, error) {
	if err := managementidentity.ValidateClient(identity, time.Now().UTC()); err != nil {
		return nil, err
	}
	if identity.Authentication == managementidentity.TrustedNetwork {
		return &clientCredentials{TransportCredentials: insecure.NewCredentials()}, nil
	}
	config, err := ClientTLSConfig(identity, node)
	if err != nil {
		return nil, err
	}
	return NewClientCredentials(config), nil
}

type managementObservationKey struct{}

// ManagementObservation records wire evidence from a management dial. If gRPC
// handshakes on more than one connection, the evidence is inconclusive.
type ManagementObservation struct {
	attempts         atomic.Uint32
	plaintextHTTP2   atomic.Bool
	plaintextSent    atomic.Bool
	plaintextClosed  atomic.Bool
	unknownAuthority atomic.Bool
}

// ObserveManagementConnection enables one dial's wire observation through
// ManagementDialOptions.
func ObserveManagementConnection(ctx context.Context) (context.Context, *ManagementObservation) {
	observation := new(ManagementObservation)
	return context.WithValue(ctx, managementObservationKey{}, observation), observation
}

// PlaintextHTTP2 reports a valid plaintext HTTP/2 server preface received
// during a TLS handshake.
func (o *ManagementObservation) PlaintextHTTP2() bool {
	return o.attempts.Load() == 1 && o.plaintextHTTP2.Load()
}

// PlaintextClosed reports that the HTTP/2 client preface was sent and the
// server closed without sending a response. Status can use this to decide
// whether to try an authenticated read.
func (o *ManagementObservation) PlaintextClosed() bool {
	return o.attempts.Load() == 1 && o.plaintextSent.Load() && o.plaintextClosed.Load()
}

// observedCredentials records every handshake of a management connection.
// gRPC calls ClientHandshake only on an established TCP connection, so
// attempts counts the connections that reached the endpoint.
type observedCredentials struct {
	credentials.TransportCredentials
	observation *ManagementObservation
	plaintext   bool
	// inspectTLS enables the buffered preface inspection of a TLS connection,
	// which only callers of ObserveManagementConnection need.
	inspectTLS bool
}

func (c *observedCredentials) Clone() credentials.TransportCredentials {
	clone := *c
	clone.TransportCredentials = c.TransportCredentials.Clone()
	return &clone
}

func (c *observedCredentials) ClientHandshake(ctx context.Context, authority string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	first := c.observation.attempts.Add(1) == 1
	if first && (c.plaintext || c.inspectTLS) {
		observed := &observedConn{Conn: conn, observation: c.observation}
		if !c.plaintext {
			observed.reader = bufio.NewReaderSize(conn, frameHeaderSize+initialMaxFrameSize)
		}
		conn = observed
	}
	secure, info, err := c.TransportCredentials.ClientHandshake(ctx, authority, conn)
	c.observation.unknownAuthority.Store(unknownServerAuthority(err))
	return secure, info, err
}

const (
	frameHeaderSize     = 9
	initialMaxFrameSize = 16384
)

type observedConn struct {
	net.Conn
	observation *ManagementObservation
	reader      *bufio.Reader
	read        bool
}

func (c *observedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if c.reader == nil && err == nil && n == len(http2.ClientPreface) && string(p[:n]) == http2.ClientPreface {
		c.observation.plaintextSent.Store(true)
	}
	return n, err
}

func (c *observedConn) Read(p []byte) (int, error) {
	first := !c.read
	c.read = true
	if c.reader == nil {
		n, err := c.Conn.Read(p)
		if first && n == 0 && (errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) {
			c.observation.plaintextClosed.Store(true)
		}
		return n, err
	}
	if first {
		// Inspect the frame before TLS can close the connection on a non-TLS
		// record header. Peek preserves the bytes for the ordinary handshake.
		c.observation.plaintextHTTP2.Store(isPlaintextHTTP2(c.reader))
	}
	return c.reader.Read(p)
}

func isPlaintextHTTP2(reader *bufio.Reader) bool {
	header, err := reader.Peek(5)
	if err != nil {
		return false
	}

	length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
	if http2.FrameType(header[3]) != http2.FrameSettings || http2.Flags(header[4]).Has(http2.FlagSettingsAck) || length > initialMaxFrameSize || length%6 != 0 {
		return false
	}
	data, err := reader.Peek(frameHeaderSize + length)
	if err != nil {
		return false
	}
	frame, err := http2.NewFramer(nil, bytes.NewReader(data)).ReadFrame()
	if err != nil {
		return false
	}
	settings := frame.(*http2.SettingsFrame)
	if err := settings.ForeachSetting(func(setting http2.Setting) error {
		// RFC 9113 requires a server to disable push if it sends this setting.
		// RFC 9218 restricts the priority setting to 0 or 1.
		switch {
		case setting.ID == http2.SettingEnablePush && setting.Val != 0,
			setting.ID == http2.SettingNoRFC7540Priorities && setting.Val > 1:
			return http2.ConnectionError(http2.ErrCodeProtocol)
		}
		return setting.Valid()
	}); err != nil {
		return false
	}
	return true
}
