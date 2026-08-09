package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/managementidentity"
)

const minimumTLSVersion = tls.VersionTLS13

func ServerTLSConfig(root string) (*tls.Config, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	ca, err := os.ReadFile(filepath.Join(root, generation.ManagementCACertificatePath))
	if err != nil {
		return nil, fmt.Errorf("read management client CA: %w", err)
	}
	certificate, err := os.ReadFile(filepath.Join(root, generation.ManagementServerCertPath))
	if err != nil {
		return nil, fmt.Errorf("read management server certificate: %w", err)
	}
	privateKey, err := os.ReadFile(filepath.Join(root, generation.ManagementServerPrivateKeyPath))
	if err != nil {
		return nil, fmt.Errorf("read management server private key: %w", err)
	}
	return ServerTLSConfigForNode(managementidentity.NodeCredentials{
		CACertificate: string(ca), ServerCertificate: string(certificate), ServerPrivateKey: string(privateKey),
	})
}

func ServerTLSConfigForNode(identity managementidentity.NodeCredentials) (*tls.Config, error) {
	certificate, err := tls.X509KeyPair([]byte(identity.ServerCertificate), []byte(identity.ServerPrivateKey))
	if err != nil {
		return nil, fmt.Errorf("load management server identity: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM([]byte(identity.CACertificate)) {
		return nil, fmt.Errorf("management client CA contains no PEM certificate")
	}
	return &tls.Config{
		MinVersion:   minimumTLSVersion,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}, nil
}

func ClientTLSConfig(credentials managementidentity.ClientCredentials, nodeName string) (*tls.Config, error) {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return nil, fmt.Errorf("management node name is required")
	}
	if err := managementidentity.ValidateClient(credentials, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("validate management client identity: %w", err)
	}
	certificate, err := tls.X509KeyPair([]byte(credentials.ClientCertificate), []byte(credentials.ClientPrivateKey))
	if err != nil {
		return nil, fmt.Errorf("load management client identity: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(credentials.CACertificate)) {
		return nil, fmt.Errorf("management server CA contains no PEM certificate")
	}
	return &tls.Config{
		MinVersion:   minimumTLSVersion,
		RootCAs:      roots,
		Certificates: []tls.Certificate{certificate},
		ServerName:   nodeName,
	}, nil
}
