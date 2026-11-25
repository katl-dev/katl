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
	var plaintext tls.RecordHeaderError
	switch {
	case errors.As(err, &unknownCA):
		return nil, nil, fmt.Errorf("node uses a different management authority; after rotation use the updated ClusterConfig and refresh saved contexts, otherwise restore the installed authority's secrets or, if they are lost, run katlctl management identity recover --config CLUSTER.yaml")
	case errors.As(err, &plaintext):
		return nil, nil, fmt.Errorf("management endpoint answered without TLS. If this node was installed with trusted-network management, set `managementAuthentication: trusted-network`. Otherwise check that management.address points at the node's katlc agent on 9443. Changing mode requires reprovisioning (docs/operations/management-auth.md#change-the-access-mode): %w", err)
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
