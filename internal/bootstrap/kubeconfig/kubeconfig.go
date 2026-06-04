package kubeconfig

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPath = "katl-kubeconfig"
	fileMode    = 0o600
	dirMode     = 0o700
)

var ErrExists = errors.New("kubeconfig already exists with different content")

type EndpointSelection struct {
	InitialEndpoint      string
	ControlPlaneEndpoint string
	StableEndpoint       string
	StableEndpointReady  bool
}

type Request struct {
	Path      string
	Overwrite bool

	Endpoint EndpointSelection

	ClusterName string
	ContextName string
	UserName    string

	CertificateAuthorityData string
	ClientCertificateData    string
	ClientKeyData            string
}

type Result struct {
	Path        string
	Server      string
	Written     bool
	Overwritten bool
	Idempotent  bool
}

func (r Result) KubectlArgs() []string {
	return []string{"kubectl", "--kubeconfig", r.Path, "get", "nodes"}
}

func (r Result) NextStep() string {
	return strings.Join(r.KubectlArgs(), " ")
}

func Write(request Request) (Result, error) {
	path := strings.TrimSpace(request.Path)
	if path == "" {
		path = DefaultPath
	}
	server, err := SelectServer(request.Endpoint)
	if err != nil {
		return Result{}, err
	}
	content, err := Render(RenderRequest{
		Server:                   server,
		ClusterName:              valueOrDefault(request.ClusterName, "katl"),
		ContextName:              valueOrDefault(request.ContextName, "katl"),
		UserName:                 valueOrDefault(request.UserName, "katl-admin"),
		CertificateAuthorityData: request.CertificateAuthorityData,
		ClientCertificateData:    request.ClientCertificateData,
		ClientKeyData:            request.ClientKeyData,
	})
	if err != nil {
		return Result{}, err
	}
	result := Result{Path: path, Server: server}
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, content) {
			if err := os.Chmod(path, fileMode); err != nil {
				return Result{}, fmt.Errorf("chmod existing kubeconfig: %w", err)
			}
			result.Idempotent = true
			return result, nil
		}
		if !request.Overwrite {
			return Result{}, ErrExists
		}
		result.Overwritten = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("read existing kubeconfig: %w", err)
	}
	if err := writeRestricted(path, content); err != nil {
		return Result{}, err
	}
	result.Written = true
	return result, nil
}

func SelectServer(selection EndpointSelection) (string, error) {
	switch {
	case strings.TrimSpace(selection.StableEndpoint) != "" && selection.StableEndpointReady:
		return normalizeServer(selection.StableEndpoint)
	case strings.TrimSpace(selection.ControlPlaneEndpoint) != "":
		return normalizeServer(selection.ControlPlaneEndpoint)
	case strings.TrimSpace(selection.InitialEndpoint) != "":
		return normalizeServer(selection.InitialEndpoint)
	default:
		return "", fmt.Errorf("kubeconfig server endpoint is required")
	}
}

type RenderRequest struct {
	Server string

	ClusterName string
	ContextName string
	UserName    string

	CertificateAuthorityData string
	ClientCertificateData    string
	ClientKeyData            string
}

func Render(request RenderRequest) ([]byte, error) {
	if strings.TrimSpace(request.Server) == "" {
		return nil, fmt.Errorf("server endpoint is required")
	}
	if strings.TrimSpace(request.ClusterName) == "" {
		return nil, fmt.Errorf("cluster name is required")
	}
	if strings.TrimSpace(request.ContextName) == "" {
		return nil, fmt.Errorf("context name is required")
	}
	if strings.TrimSpace(request.UserName) == "" {
		return nil, fmt.Errorf("user name is required")
	}
	if strings.TrimSpace(request.CertificateAuthorityData) == "" {
		return nil, fmt.Errorf("certificate authority data is required")
	}
	if strings.TrimSpace(request.ClientCertificateData) == "" {
		return nil, fmt.Errorf("client certificate data is required")
	}
	if strings.TrimSpace(request.ClientKeyData) == "" {
		return nil, fmt.Errorf("client key data is required")
	}
	object := kubeconfigObject{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: request.ContextName,
		Clusters: []namedCluster{{
			Name: request.ClusterName,
			Cluster: cluster{
				Server:                   request.Server,
				CertificateAuthorityData: request.CertificateAuthorityData,
			},
		}},
		Users: []namedUser{{
			Name: request.UserName,
			User: user{
				ClientCertificateData: request.ClientCertificateData,
				ClientKeyData:         request.ClientKeyData,
			},
		}},
		Contexts: []namedContext{{
			Name: request.ContextName,
			Context: context{
				Cluster: request.ClusterName,
				User:    request.UserName,
			},
		}},
	}
	data, err := yaml.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("marshal kubeconfig: %w", err)
	}
	return data, nil
}

type kubeconfigObject struct {
	APIVersion     string         `yaml:"apiVersion"`
	Kind           string         `yaml:"kind"`
	Clusters       []namedCluster `yaml:"clusters"`
	Users          []namedUser    `yaml:"users"`
	Contexts       []namedContext `yaml:"contexts"`
	CurrentContext string         `yaml:"current-context"`
}

type namedCluster struct {
	Name    string  `yaml:"name"`
	Cluster cluster `yaml:"cluster"`
}

type cluster struct {
	Server                   string `yaml:"server"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
}

type namedUser struct {
	Name string `yaml:"name"`
	User user   `yaml:"user"`
}

type user struct {
	ClientCertificateData string `yaml:"client-certificate-data"`
	ClientKeyData         string `yaml:"client-key-data"`
}

type namedContext struct {
	Name    string  `yaml:"name"`
	Context context `yaml:"context"`
}

type context struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

func writeRestricted(path string, content []byte) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return fmt.Errorf("create kubeconfig directory: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".katl-kubeconfig-*")
	if err != nil {
		return fmt.Errorf("create temporary kubeconfig: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temporary kubeconfig: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary kubeconfig: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary kubeconfig: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install kubeconfig: %w", err)
	}
	return nil
}

func normalizeServer(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse kubeconfig server endpoint: %w", err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("kubeconfig server endpoint must use https")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("kubeconfig server endpoint must not include userinfo")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("kubeconfig server endpoint host is required")
	}
	if parsed.Port() == "" {
		return "", fmt.Errorf("kubeconfig server endpoint port is required")
	}
	return parsed.String(), nil
}

func valueOrDefault(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
