package transport

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nodeidentity"
)

const minimumTLSVersion = tls.VersionTLS13

func ServerTLSConfig(root string) (*tls.Config, []string, error) {
	mode, err := nodeidentity.ManagementAuthentication(root)
	if err != nil {
		return nil, nil, err
	}
	if mode == managementidentity.TrustedNetwork {
		return nil, nil, nil
	}
	caPath, certPath, keyPath, err := nodeidentity.ManagementCredentialsPaths(root)
	if err != nil {
		return nil, nil, err
	}
	return serverTLSConfigFromFiles(caPath, certPath, keyPath)
}

// TrialServerTLSConfig serves the credentials staged for a management trial's
// authority instead of the installed mode.
func TrialServerTLSConfig(root, authority string) (*tls.Config, []string, error) {
	return serverTLSConfigFromFiles(nodeidentity.ManagementVersionPaths(root, authority))
}

func serverTLSConfigFromFiles(caPath, certPath, keyPath string) (*tls.Config, []string, error) {
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read management client CA: %w", err)
	}
	certificate, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read management server certificate: %w", err)
	}
	privateKey, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read management server private key: %w", err)
	}
	return ServerTLSConfigForNode(managementidentity.NodeCredentials{
		CACertificate: string(ca), ServerCertificate: string(certificate), ServerPrivateKey: string(privateKey),
	})
}

func ServerTLSConfigForNode(identity managementidentity.NodeCredentials) (*tls.Config, []string, error) {
	certificate, err := tls.X509KeyPair([]byte(identity.ServerCertificate), []byte(identity.ServerPrivateKey))
	if err != nil {
		return nil, nil, fmt.Errorf("load management server identity: %w", err)
	}
	clientCAs := x509.NewCertPool()
	var authorities []string
	// Use the same parsed certificates for the TLS pool and the report.
	// Skip unusable blocks as x509.CertPool.AppendCertsFromPEM does.
	for remaining := []byte(identity.CACertificate); len(remaining) > 0; {
		var block *pem.Block
		block, remaining = pem.Decode(remaining)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		clientCAs.AddCert(cert)
		authorities = append(authorities, managementidentity.CertificateFingerprint(cert))
	}
	if len(authorities) == 0 {
		return nil, nil, fmt.Errorf("management client CA contains no PEM certificate")
	}
	slices.Sort(authorities)
	return &tls.Config{
		MinVersion:   minimumTLSVersion,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}, slices.Compact(authorities), nil
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
