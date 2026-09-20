package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
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

func (c *clientCredentials) ClientHandshake(ctx context.Context, authority string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	secure, info, err := c.TransportCredentials.ClientHandshake(ctx, authority, conn)
	var unknownCA x509.UnknownAuthorityError
	var wrongName x509.HostnameError
	switch {
	case errors.As(err, &unknownCA):
		return nil, nil, fmt.Errorf("node uses a different management authority; restore the original cluster secrets file or explicitly recover node trust using SSH or console access; generating new workstation keys does not restore access")
	case errors.As(err, &wrongName):
		return nil, nil, fmt.Errorf("management address answered with a certificate for another node; check the node name and management.address in the cluster configuration")
	default:
		return secure, info, err
	}
}

func (c *clientCredentials) Clone() credentials.TransportCredentials {
	return &clientCredentials{TransportCredentials: c.TransportCredentials.Clone()}
}

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
