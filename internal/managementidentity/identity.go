package managementidentity

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "katl.dev/v1alpha1"
	Kind       = "ManagementIdentity"
	MaxSize    = 1 << 20
)

type CertificateKey struct {
	Certificate string `yaml:"certificate" json:"certificate"`
	PrivateKey  string `yaml:"privateKey" json:"privateKey"`
}

// Bundle is the operator-held authority for one cluster's management plane.
// It is a secret backup artifact and must never be installed on a node.
type Bundle struct {
	APIVersion           string                    `yaml:"apiVersion" json:"apiVersion"`
	Kind                 string                    `yaml:"kind" json:"kind"`
	ClusterName          string                    `yaml:"clusterName" json:"clusterName"`
	CreatedAt            time.Time                 `yaml:"createdAt" json:"createdAt"`
	CertificateAuthority CertificateKey            `yaml:"certificateAuthority" json:"certificateAuthority"`
	Operator             CertificateKey            `yaml:"operator" json:"operator"`
	Nodes                map[string]CertificateKey `yaml:"nodes,omitempty" json:"nodes,omitempty"`
}

// NodeCredentials are the only management secrets installed on one node.
// The server leaf is not a CA and cannot mint callers or other node identities.
type NodeCredentials struct {
	CACertificate     string `json:"caCertificate" yaml:"caCertificate"`
	ServerCertificate string `json:"serverCertificate" yaml:"serverCertificate"`
	ServerPrivateKey  string `json:"serverPrivateKey" yaml:"serverPrivateKey"`
}

// ClientCredentials are copied to the mode-0600 katlctl workstation context.
type ClientCredentials struct {
	CACertificate     string `json:"caCertificate" yaml:"caCertificate"`
	ClientCertificate string `json:"clientCertificate" yaml:"clientCertificate"`
	ClientPrivateKey  string `json:"clientPrivateKey" yaml:"clientPrivateKey"`
}

type Info struct {
	ClusterName string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Fingerprint string
}

type GenerateOptions struct {
	ClusterName string
	Now         time.Time
	Random      io.Reader
}

func Generate(options GenerateOptions) (Bundle, error) {
	clusterName := strings.TrimSpace(options.ClusterName)
	if clusterName == "" {
		return Bundle{}, fmt.Errorf("cluster name is required")
	}
	now := options.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	random := options.Random
	if random == nil {
		random = rand.Reader
	}
	ca, caCert, caKey, err := generateCA(clusterName, now, random)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate management CA: %w", err)
	}
	operator, err := issueCertificate(ca, caKey, "katlctl", nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now, random)
	if err != nil {
		return Bundle{}, fmt.Errorf("issue operator certificate: %w", err)
	}
	bundle := Bundle{
		APIVersion:           APIVersion,
		Kind:                 Kind,
		ClusterName:          clusterName,
		CreatedAt:            now,
		CertificateAuthority: CertificateKey{Certificate: caCert, PrivateKey: encodePrivateKey(caKey)},
		Operator:             operator,
	}
	if _, err := Validate(bundle, now); err != nil {
		return Bundle{}, fmt.Errorf("validate generated management identity: %w", err)
	}
	return bundle, nil
}

func IssueNode(bundle Bundle, nodeName string, now time.Time, random io.Reader) (NodeCredentials, error) {
	if _, err := Validate(bundle, now); err != nil {
		return NodeCredentials{}, err
	}
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return NodeCredentials{}, fmt.Errorf("node name is required")
	}
	ca, err := parseCertificate("certificateAuthority.certificate", bundle.CertificateAuthority.Certificate)
	if err != nil {
		return NodeCredentials{}, err
	}
	caKey, err := parsePrivateKey("certificateAuthority.privateKey", bundle.CertificateAuthority.PrivateKey)
	if err != nil {
		return NodeCredentials{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if random == nil {
		random = rand.Reader
	}
	server, err := issueCertificate(ca, caKey, nodeName, []string{nodeName}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.UTC(), random)
	if err != nil {
		return NodeCredentials{}, fmt.Errorf("issue node %q server certificate: %w", nodeName, err)
	}
	credentials := NodeCredentials{
		CACertificate:     bundle.CertificateAuthority.Certificate,
		ServerCertificate: server.Certificate,
		ServerPrivateKey:  server.PrivateKey,
	}
	if err := ValidateNode(credentials, nodeName, now); err != nil {
		return NodeCredentials{}, err
	}
	return credentials, nil
}

// EnsureNode returns stable per-node server material, adding it only when the
// node has not been issued before.
func EnsureNode(bundle *Bundle, nodeName string, now time.Time, random io.Reader) (NodeCredentials, bool, error) {
	if bundle == nil {
		return NodeCredentials{}, false, fmt.Errorf("management identity is required")
	}
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return NodeCredentials{}, false, fmt.Errorf("node name is required")
	}
	if pair, ok := bundle.Nodes[nodeName]; ok {
		credentials := NodeCredentials{CACertificate: bundle.CertificateAuthority.Certificate, ServerCertificate: pair.Certificate, ServerPrivateKey: pair.PrivateKey}
		if err := ValidateNode(credentials, nodeName, now); err != nil {
			return NodeCredentials{}, false, fmt.Errorf("stored node %q identity: %w", nodeName, err)
		}
		return credentials, false, nil
	}
	credentials, err := IssueNode(*bundle, nodeName, now, random)
	if err != nil {
		return NodeCredentials{}, false, err
	}
	if bundle.Nodes == nil {
		bundle.Nodes = make(map[string]CertificateKey)
	}
	bundle.Nodes[nodeName] = CertificateKey{Certificate: credentials.ServerCertificate, PrivateKey: credentials.ServerPrivateKey}
	return credentials, true, nil
}

func Client(bundle Bundle, now time.Time) (ClientCredentials, error) {
	if _, err := Validate(bundle, now); err != nil {
		return ClientCredentials{}, err
	}
	return ClientCredentials{
		CACertificate:     bundle.CertificateAuthority.Certificate,
		ClientCertificate: bundle.Operator.Certificate,
		ClientPrivateKey:  bundle.Operator.PrivateKey,
	}, nil
}

func Validate(bundle Bundle, now time.Time) (Info, error) {
	if bundle.APIVersion != APIVersion {
		return Info{}, fmt.Errorf("management identity apiVersion must be %q", APIVersion)
	}
	if bundle.Kind != Kind {
		return Info{}, fmt.Errorf("management identity kind must be %q", Kind)
	}
	clusterName := strings.TrimSpace(bundle.ClusterName)
	if clusterName == "" {
		return Info{}, fmt.Errorf("management identity clusterName is required")
	}
	if bundle.CreatedAt.IsZero() {
		return Info{}, fmt.Errorf("management identity createdAt is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ca, err := validatePair("certificateAuthority", bundle.CertificateAuthority, now, true, x509.ExtKeyUsageAny, "")
	if err != nil {
		return Info{}, err
	}
	if _, err := validateLeafPair("operator", bundle.Operator, ca, now, x509.ExtKeyUsageClientAuth, ""); err != nil {
		return Info{}, err
	}
	for nodeName, pair := range bundle.Nodes {
		nodeName = strings.TrimSpace(nodeName)
		if nodeName == "" {
			return Info{}, fmt.Errorf("management identity node name is required")
		}
		if _, err := validateLeafPair("nodes["+nodeName+"]", pair, ca, now, x509.ExtKeyUsageServerAuth, nodeName); err != nil {
			return Info{}, err
		}
	}
	sum := sha256.Sum256(ca.Raw)
	return Info{
		ClusterName: clusterName,
		CreatedAt:   bundle.CreatedAt.UTC(),
		ExpiresAt:   ca.NotAfter.UTC(),
		Fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}

func ValidateNode(credentials NodeCredentials, nodeName string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ca, err := parseCertificate("caCertificate", credentials.CACertificate)
	if err != nil {
		return err
	}
	if !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return fmt.Errorf("caCertificate is not a certificate-signing CA")
	}
	pair := CertificateKey{Certificate: credentials.ServerCertificate, PrivateKey: credentials.ServerPrivateKey}
	_, err = validateLeafPair("server", pair, ca, now, x509.ExtKeyUsageServerAuth, strings.TrimSpace(nodeName))
	return err
}

func ValidateClient(credentials ClientCredentials, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ca, err := parseCertificate("caCertificate", credentials.CACertificate)
	if err != nil {
		return err
	}
	if !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return fmt.Errorf("caCertificate is not a certificate-signing CA")
	}
	pair := CertificateKey{Certificate: credentials.ClientCertificate, PrivateKey: credentials.ClientPrivateKey}
	_, err = validateLeafPair("client", pair, ca, now, x509.ExtKeyUsageClientAuth, "")
	return err
}

func Marshal(bundle Bundle) ([]byte, error) {
	if _, err := Validate(bundle, time.Now().UTC()); err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encode management identity: %w", err)
	}
	return data, nil
}

func Parse(data []byte) (Bundle, error) {
	if len(data) == 0 {
		return Bundle{}, fmt.Errorf("management identity is empty")
	}
	if len(data) > MaxSize {
		return Bundle{}, fmt.Errorf("management identity is %d bytes; maximum is %d", len(data), MaxSize)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var bundle Bundle
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, fmt.Errorf("decode management identity: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Bundle{}, fmt.Errorf("decode management identity: multiple YAML documents are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return Bundle{}, fmt.Errorf("decode management identity: %w", err)
	}
	if _, err := Validate(bundle, time.Now().UTC()); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func Read(path string) (Bundle, []byte, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return Bundle{}, nil, fmt.Errorf("read management identity %s: %w", path, err)
	}
	bundle, err := Parse(data)
	if err != nil {
		return Bundle{}, nil, fmt.Errorf("management identity %s: %w", path, err)
	}
	return bundle, data, nil
}

func Write(path string, bundle Bundle) error {
	data, err := Marshal(bundle)
	if err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("management identity output path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create management identity directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".management-identity-*.tmp")
	if err != nil {
		return fmt.Errorf("create management identity temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure management identity output: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write management identity output: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync management identity output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close management identity output: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing management identity %s", path)
		}
		return fmt.Errorf("publish management identity %s: %w", path, err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open management identity directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync management identity directory: %w", err)
	}
	return nil
}

// SaveExisting atomically adds node leaves to an identity Katl already owns.
// A missing file or a different cluster authority is never replaced.
func SaveExisting(path string, bundle Bundle) error {
	path = strings.TrimSpace(path)
	existing, _, err := Read(path)
	if err != nil {
		return err
	}
	before, err := Validate(existing, time.Now().UTC())
	if err != nil {
		return err
	}
	after, err := Validate(bundle, time.Now().UTC())
	if err != nil {
		return err
	}
	if before.ClusterName != after.ClusterName || before.Fingerprint != after.Fingerprint {
		return fmt.Errorf("refusing to replace management identity with a different cluster authority")
	}
	data, err := Marshal(bundle)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".management-identity-*.tmp")
	if err != nil {
		return fmt.Errorf("create management identity temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure management identity output: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write management identity output: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync management identity output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close management identity output: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace management identity %s: %w", path, err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open management identity directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync management identity directory: %w", err)
	}
	return nil
}

func generateCA(clusterName string, now time.Time, random io.Reader) (*x509.Certificate, string, ed25519.PrivateKey, error) {
	public, private, err := ed25519.GenerateKey(random)
	if err != nil {
		return nil, "", nil, err
	}
	serial, err := randomSerial(random)
	if err != nil {
		return nil, "", nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "katl management " + clusterName},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(random, template, template, public, private)
	if err != nil {
		return nil, "", nil, err
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, "", nil, err
	}
	return certificate, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), private, nil
}

func issueCertificate(ca *x509.Certificate, caKey crypto.Signer, commonName string, dnsNames []string, usages []x509.ExtKeyUsage, now time.Time, random io.Reader) (CertificateKey, error) {
	public, private, err := ed25519.GenerateKey(random)
	if err != nil {
		return CertificateKey{}, err
	}
	serial, err := randomSerial(random)
	if err != nil {
		return CertificateKey{}, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		DNSNames:              dnsNames,
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              ca.NotAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           usages,
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(random, template, ca, public, caKey)
	if err != nil {
		return CertificateKey{}, err
	}
	return CertificateKey{
		Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		PrivateKey:  encodePrivateKey(private),
	}, nil
}

func randomSerial(random io.Reader) (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(random, limit)
	if err != nil {
		return nil, err
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial, nil
}

func encodePrivateKey(key crypto.PrivateKey) string {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func parseCertificate(name, value string) (*x509.Certificate, error) {
	block, rest := pem.Decode([]byte(value))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("%s must contain exactly one PEM certificate", name)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return certificate, nil
}

func parsePrivateKey(name, value string) (crypto.Signer, error) {
	block, rest := pem.Decode([]byte(value))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("%s must contain exactly one PKCS#8 PEM private key", name)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%s is not a signing key", name)
	}
	return signer, nil
}

func validatePair(name string, pair CertificateKey, now time.Time, requireCA bool, usage x509.ExtKeyUsage, dnsName string) (*x509.Certificate, error) {
	certificate, err := parseCertificate(name+".certificate", pair.Certificate)
	if err != nil {
		return nil, err
	}
	private, err := parsePrivateKey(name+".privateKey", pair.PrivateKey)
	if err != nil {
		return nil, err
	}
	if !publicKeysEqual(certificate.PublicKey, private.Public()) {
		return nil, fmt.Errorf("%s private key does not match its certificate", name)
	}
	if requireCA && (!certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0) {
		return nil, fmt.Errorf("%s certificate is not a certificate-signing CA", name)
	}
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return nil, fmt.Errorf("%s certificate is not valid at %s", name, now.Format(time.RFC3339))
	}
	_ = usage
	_ = dnsName
	return certificate, nil
}

func validateLeafPair(name string, pair CertificateKey, ca *x509.Certificate, now time.Time, usage x509.ExtKeyUsage, dnsName string) (*x509.Certificate, error) {
	certificate, err := validatePair(name, pair, now, false, usage, dnsName)
	if err != nil {
		return nil, err
	}
	if certificate.IsCA {
		return nil, fmt.Errorf("%s certificate must not be a CA", name)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	options := x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}}
	if dnsName != "" {
		options.DNSName = dnsName
	}
	if _, err := certificate.Verify(options); err != nil {
		return nil, fmt.Errorf("%s certificate verification: %w", name, err)
	}
	return certificate, nil
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	aDER, err := x509.MarshalPKIXPublicKey(a)
	if err != nil {
		return false
	}
	bDER, err := x509.MarshalPKIXPublicKey(b)
	return err == nil && bytes.Equal(aDER, bDER)
}
