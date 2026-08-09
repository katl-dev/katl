package kubernetesidentity

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
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
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "katl.dev/v1alpha1"
	Kind       = "KubernetesIdentity"
	MaxSize    = 1 << 20
)

type Bundle struct {
	APIVersion     string         `yaml:"apiVersion" json:"apiVersion"`
	Kind           string         `yaml:"kind" json:"kind"`
	ClusterName    string         `yaml:"clusterName" json:"clusterName"`
	CreatedAt      time.Time      `yaml:"createdAt" json:"createdAt"`
	KubernetesCA   CertificateKey `yaml:"kubernetesCA" json:"kubernetesCA"`
	FrontProxyCA   CertificateKey `yaml:"frontProxyCA" json:"frontProxyCA"`
	EtcdCA         CertificateKey `yaml:"etcdCA" json:"etcdCA"`
	ServiceAccount SigningKey     `yaml:"serviceAccount" json:"serviceAccount"`
}

type CertificateKey struct {
	Certificate string `yaml:"certificate" json:"certificate"`
	PrivateKey  string `yaml:"privateKey" json:"privateKey"`
}

type SigningKey struct {
	PrivateKey string `yaml:"privateKey" json:"privateKey"`
	PublicKey  string `yaml:"publicKey" json:"publicKey"`
}

type Info struct {
	ClusterName             string
	CreatedAt               time.Time
	Fingerprint             string
	KubernetesCAFingerprint string
	FrontProxyCAFingerprint string
	EtcdCAFingerprint       string
	ServiceAccountKeyID     string
}

type GenerateOptions struct {
	ClusterName string
	Now         time.Time
	Random      io.Reader
}

type ImportOptions struct {
	ClusterName string
	CreatedAt   time.Time
	PKIDir      string
}

type identityMaterial struct {
	kubernetesCA   *x509.Certificate
	frontProxyCA   *x509.Certificate
	etcdCA         *x509.Certificate
	serviceAccount crypto.PublicKey
}

var identityFiles = []struct {
	path string
	mode os.FileMode
	data func(Bundle) string
}{
	{"ca.crt", 0o644, func(bundle Bundle) string { return bundle.KubernetesCA.Certificate }},
	{"ca.key", 0o600, func(bundle Bundle) string { return bundle.KubernetesCA.PrivateKey }},
	{"front-proxy-ca.crt", 0o644, func(bundle Bundle) string { return bundle.FrontProxyCA.Certificate }},
	{"front-proxy-ca.key", 0o600, func(bundle Bundle) string { return bundle.FrontProxyCA.PrivateKey }},
	{"etcd/ca.crt", 0o644, func(bundle Bundle) string { return bundle.EtcdCA.Certificate }},
	{"etcd/ca.key", 0o600, func(bundle Bundle) string { return bundle.EtcdCA.PrivateKey }},
	{"sa.key", 0o600, func(bundle Bundle) string { return bundle.ServiceAccount.PrivateKey }},
	{"sa.pub", 0o644, func(bundle Bundle) string { return bundle.ServiceAccount.PublicKey }},
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
	kubernetesCA, err := generateCA(random, "kubernetes", now)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate Kubernetes CA: %w", err)
	}
	frontProxyCA, err := generateCA(random, "front-proxy-ca", now)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate front-proxy CA: %w", err)
	}
	etcdCA, err := generateCA(random, "etcd-ca", now)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate etcd CA: %w", err)
	}
	serviceAccountKey, err := rsa.GenerateKey(random, 2048)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate service-account key: %w", err)
	}
	serviceAccountPrivate := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serviceAccountKey)})
	serviceAccountPublicDER, err := x509.MarshalPKIXPublicKey(&serviceAccountKey.PublicKey)
	if err != nil {
		return Bundle{}, fmt.Errorf("encode service-account public key: %w", err)
	}
	bundle := Bundle{
		APIVersion:     APIVersion,
		Kind:           Kind,
		ClusterName:    clusterName,
		CreatedAt:      now,
		KubernetesCA:   kubernetesCA,
		FrontProxyCA:   frontProxyCA,
		EtcdCA:         etcdCA,
		ServiceAccount: SigningKey{PrivateKey: string(serviceAccountPrivate), PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: serviceAccountPublicDER}))},
	}
	if _, err := Validate(bundle, now); err != nil {
		return Bundle{}, fmt.Errorf("validate generated identity: %w", err)
	}
	return bundle, nil
}

func generateCA(random io.Reader, commonName string, now time.Time) (CertificateKey, error) {
	key, err := rsa.GenerateKey(random, 2048)
	if err != nil {
		return CertificateKey{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(random, serialLimit)
	if err != nil {
		return CertificateKey{}, err
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certificateDER, err := x509.CreateCertificate(random, template, template, &key.PublicKey, key)
	if err != nil {
		return CertificateKey{}, err
	}
	return CertificateKey{
		Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})),
		PrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	}, nil
}

func Import(options ImportOptions) (Bundle, error) {
	clusterName := strings.TrimSpace(options.ClusterName)
	if clusterName == "" {
		return Bundle{}, fmt.Errorf("cluster name is required")
	}
	dir := strings.TrimSpace(options.PKIDir)
	if dir == "" {
		return Bundle{}, fmt.Errorf("kubeadm PKI directory is required")
	}
	read := func(path string) (string, error) {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		return string(data), nil
	}
	bundle := Bundle{APIVersion: APIVersion, Kind: Kind, ClusterName: clusterName, CreatedAt: options.CreatedAt.UTC()}
	if bundle.CreatedAt.IsZero() {
		bundle.CreatedAt = time.Now().UTC()
	}
	var err error
	if bundle.KubernetesCA.Certificate, err = read("ca.crt"); err != nil {
		return Bundle{}, err
	}
	if bundle.KubernetesCA.PrivateKey, err = read("ca.key"); err != nil {
		return Bundle{}, err
	}
	if bundle.FrontProxyCA.Certificate, err = read("front-proxy-ca.crt"); err != nil {
		return Bundle{}, err
	}
	if bundle.FrontProxyCA.PrivateKey, err = read("front-proxy-ca.key"); err != nil {
		return Bundle{}, err
	}
	if bundle.EtcdCA.Certificate, err = read("etcd/ca.crt"); err != nil {
		return Bundle{}, err
	}
	if bundle.EtcdCA.PrivateKey, err = read("etcd/ca.key"); err != nil {
		return Bundle{}, err
	}
	if bundle.ServiceAccount.PrivateKey, err = read("sa.key"); err != nil {
		return Bundle{}, err
	}
	if bundle.ServiceAccount.PublicKey, err = read("sa.pub"); err != nil {
		return Bundle{}, err
	}
	if _, err := Validate(bundle, time.Now().UTC()); err != nil {
		return Bundle{}, fmt.Errorf("validate kubeadm PKI: %w", err)
	}
	return bundle, nil
}

func Marshal(bundle Bundle) ([]byte, error) {
	if _, err := Validate(bundle, time.Now().UTC()); err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encode Kubernetes identity: %w", err)
	}
	return data, nil
}

func Parse(data []byte) (Bundle, error) {
	if len(data) == 0 {
		return Bundle{}, fmt.Errorf("Kubernetes identity is empty")
	}
	if len(data) > MaxSize {
		return Bundle{}, fmt.Errorf("Kubernetes identity is %d bytes; maximum is %d bytes", len(data), MaxSize)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var bundle Bundle
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, fmt.Errorf("decode Kubernetes identity: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Bundle{}, fmt.Errorf("decode Kubernetes identity: multiple YAML documents are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return Bundle{}, fmt.Errorf("decode Kubernetes identity: %w", err)
	}
	if _, err := Validate(bundle, time.Now().UTC()); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func Read(path string) (Bundle, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, nil, fmt.Errorf("read Kubernetes identity %s: %w", path, err)
	}
	bundle, err := Parse(data)
	if err != nil {
		return Bundle{}, nil, fmt.Errorf("Kubernetes identity %s: %w", path, err)
	}
	return bundle, data, nil
}

func Write(path string, bundle Bundle) error {
	data, err := Marshal(bundle)
	if err != nil {
		return err
	}
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return fmt.Errorf("Kubernetes identity output path is required")
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("refusing to overwrite existing Kubernetes identity %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Kubernetes identity output %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create Kubernetes identity directory %s: %w", dir, err)
	}
	temporary, err := os.CreateTemp(dir, ".katl-identity-*")
	if err != nil {
		return fmt.Errorf("create Kubernetes identity output: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure Kubernetes identity output: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write Kubernetes identity output: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync Kubernetes identity output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Kubernetes identity output: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing Kubernetes identity %s", path)
		}
		return fmt.Errorf("publish Kubernetes identity %s: %w", path, err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync Kubernetes identity directory %s: %w", dir, err)
	}
	return nil
}

func Validate(bundle Bundle, now time.Time) (Info, error) {
	if bundle.APIVersion != APIVersion {
		return Info{}, fmt.Errorf("Kubernetes identity apiVersion must be %q", APIVersion)
	}
	if bundle.Kind != Kind {
		return Info{}, fmt.Errorf("Kubernetes identity kind must be %q", Kind)
	}
	bundle.ClusterName = strings.TrimSpace(bundle.ClusterName)
	if bundle.ClusterName == "" {
		return Info{}, fmt.Errorf("Kubernetes identity clusterName is required")
	}
	if bundle.CreatedAt.IsZero() {
		return Info{}, fmt.Errorf("Kubernetes identity createdAt is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	kubernetesCA, err := validateCA("kubernetesCA", bundle.KubernetesCA, now)
	if err != nil {
		return Info{}, err
	}
	frontProxyCA, err := validateCA("frontProxyCA", bundle.FrontProxyCA, now)
	if err != nil {
		return Info{}, err
	}
	etcdCA, err := validateCA("etcdCA", bundle.EtcdCA, now)
	if err != nil {
		return Info{}, err
	}
	serviceAccountPublic, err := validateSigningKey(bundle.ServiceAccount)
	if err != nil {
		return Info{}, fmt.Errorf("serviceAccount: %w", err)
	}
	material := identityMaterial{kubernetesCA: kubernetesCA, frontProxyCA: frontProxyCA, etcdCA: etcdCA, serviceAccount: serviceAccountPublic}
	fingerprint, err := materialFingerprint(material)
	if err != nil {
		return Info{}, err
	}
	return Info{
		ClusterName:             bundle.ClusterName,
		CreatedAt:               bundle.CreatedAt.UTC(),
		Fingerprint:             fingerprint,
		KubernetesCAFingerprint: certificateFingerprint(kubernetesCA),
		FrontProxyCAFingerprint: certificateFingerprint(frontProxyCA),
		EtcdCAFingerprint:       certificateFingerprint(etcdCA),
		ServiceAccountKeyID:     publicKeyFingerprint(serviceAccountPublic),
	}, nil
}

func validateCA(name string, pair CertificateKey, now time.Time) (*x509.Certificate, error) {
	block, rest := pem.Decode([]byte(pair.Certificate))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("%s.certificate must contain exactly one PEM certificate", name)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s.certificate: %w", name, err)
	}
	if !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("%s.certificate is not a certificate-signing CA", name)
	}
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return nil, fmt.Errorf("%s.certificate is not valid at %s (valid %s to %s)", name, now.Format(time.RFC3339), certificate.NotBefore.Format(time.RFC3339), certificate.NotAfter.Format(time.RFC3339))
	}
	privateKey, err := parsePrivateKey([]byte(pair.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("%s.privateKey: %w", name, err)
	}
	if !publicKeysEqual(certificate.PublicKey, privateKey.Public()) {
		return nil, fmt.Errorf("%s private key does not match its certificate", name)
	}
	if err := validateKeyStrength(privateKey.Public()); err != nil {
		return nil, fmt.Errorf("%s.privateKey: %w", name, err)
	}
	return certificate, nil
}

func validateSigningKey(pair SigningKey) (crypto.PublicKey, error) {
	privateKey, err := parsePrivateKey([]byte(pair.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("privateKey: %w", err)
	}
	block, rest := pem.Decode([]byte(pair.PublicKey))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("publicKey must contain exactly one PEM public key")
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("publicKey: %w", err)
	}
	if !publicKeysEqual(publicKey, privateKey.Public()) {
		return nil, fmt.Errorf("privateKey does not match publicKey")
	}
	if err := validateKeyStrength(publicKey); err != nil {
		return nil, fmt.Errorf("privateKey: %w", err)
	}
	return publicKey, nil
}

func validateKeyStrength(publicKey crypto.PublicKey) error {
	switch key := publicKey.(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 {
			return fmt.Errorf("RSA key is %d bits; kubeadm identity keys must be at least 2048 bits", key.N.BitLen())
		}
	case *ecdsa.PublicKey:
		if key.Curve == nil || key.Curve.Params().BitSize < 256 {
			return fmt.Errorf("ECDSA key must use a curve of at least 256 bits")
		}
	}
	return nil
}

func parsePrivateKey(data []byte) (crypto.Signer, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("must contain exactly one PEM private key")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := key.(crypto.Signer); ok {
			return signer, nil
		}
		return nil, fmt.Errorf("key does not implement crypto.Signer")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("unsupported or invalid private key")
}

func publicKeysEqual(left, right crypto.PublicKey) bool {
	leftDER, leftErr := x509.MarshalPKIXPublicKey(left)
	rightDER, rightErr := x509.MarshalPKIXPublicKey(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftDER, rightDER)
}

func materialFingerprint(material identityMaterial) (string, error) {
	components := []struct {
		name string
		data []byte
	}{
		{"etcd-ca", material.etcdCA.Raw},
		{"front-proxy-ca", material.frontProxyCA.Raw},
		{"kubernetes-ca", material.kubernetesCA.Raw},
	}
	serviceAccountDER, err := x509.MarshalPKIXPublicKey(material.serviceAccount)
	if err != nil {
		return "", fmt.Errorf("encode service-account public key: %w", err)
	}
	components = append(components, struct {
		name string
		data []byte
	}{"service-account", serviceAccountDER})
	sort.Slice(components, func(i, j int) bool { return components[i].name < components[j].name })
	hash := sha256.New()
	for _, component := range components {
		hash.Write([]byte(component.name))
		hash.Write([]byte{0})
		hash.Write(component.data)
		hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func certificateFingerprint(certificate *x509.Certificate) string {
	sum := sha256.Sum256(certificate.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func publicKeyFingerprint(publicKey crypto.PublicKey) string {
	der, _ := x509.MarshalPKIXPublicKey(publicKey)
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func Files(bundle Bundle) (map[string][]byte, error) {
	if _, err := Validate(bundle, time.Now().UTC()); err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(identityFiles))
	for _, file := range identityFiles {
		files[file.path] = []byte(file.data(bundle))
	}
	return files, nil
}
