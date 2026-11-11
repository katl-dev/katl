// Package externalworker defines the native kubelet enrollment contract for
// clusters whose control plane is managed outside Katl.
package externalworker

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Endpoint validates the API URL and returns its host and port.
func Endpoint(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must be an HTTPS API URL with an explicit port and no path, credentials, query, or fragment")
	}
	host, port, err := net.SplitHostPort(u.Host)
	number, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || number < 1 || number > 65535 || strings.ContainsAny(host, " \t\r\n%") {
		return "", fmt.Errorf("must contain an API host and port, such as https://api.lab.example:6443")
	}
	return u.Host, nil
}

type bootstrapConfig struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Clusters   []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
			CA     string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token string `yaml:"token"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	CurrentContext string `yaml:"current-context"`
}

// Bootstrap validates a self-contained, token-only kubeconfig. Restricting its
// shape prevents credentials, workstation paths, and executable plugins from
// acquiring responsibilities on the node. The cluster owns token permissions.
func Bootstrap(data []byte, endpoint string) ([]byte, error) {
	if len(data) > 1024*1024 {
		return nil, fmt.Errorf("bootstrap kubeconfig exceeds 1 MiB")
	}
	var config bootstrapConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("bootstrap kubeconfig must contain only one CA-pinned cluster, token user, and context; exec plugins, client certificates, and file references are not accepted")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("bootstrap kubeconfig must contain one document")
	}
	if config.APIVersion != "v1" || config.Kind != "Config" || len(config.Clusters) != 1 || len(config.Users) != 1 || len(config.Contexts) != 1 {
		return nil, fmt.Errorf("bootstrap kubeconfig must contain one v1 Config cluster, token user, and context")
	}
	cluster, user, context := config.Clusters[0], config.Users[0], config.Contexts[0]
	host, err := Endpoint(cluster.Cluster.Server)
	if err != nil || host != endpoint {
		return nil, fmt.Errorf("bootstrap kubeconfig server must match the configured external endpoint")
	}
	ca, err := base64.StdEncoding.DecodeString(cluster.Cluster.CA)
	if err != nil || !x509.NewCertPool().AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("bootstrap kubeconfig requires embedded PEM certificate-authority-data")
	}
	if cluster.Name == "" || user.Name == "" || context.Name == "" || context.Context.Cluster != cluster.Name || context.Context.User != user.Name || config.CurrentContext != context.Name {
		return nil, fmt.Errorf("bootstrap kubeconfig context must select its cluster and token user")
	}
	if strings.TrimSpace(user.User.Token) == "" || strings.ContainsAny(user.User.Token, " \t\r\n") {
		return nil, fmt.Errorf("bootstrap kubeconfig requires a token without whitespace")
	}
	return ca, nil
}

// KubeletConfig supplies node-local runtime and authentication requirements.
// Cluster DNS and domain remain explicit inputs owned by the external cluster.
func KubeletConfig(data []byte) ([]byte, error) {
	var config map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode KubeletConfiguration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("KubeletConfiguration must contain one document")
	}
	if config["apiVersion"] != "kubelet.config.k8s.io/v1beta1" || config["kind"] != "KubeletConfiguration" {
		return nil, fmt.Errorf("requires kubelet.config.k8s.io/v1beta1 KubeletConfiguration")
	}
	domain, _ := config["clusterDomain"].(string)
	dns, _ := config["clusterDNS"].([]any)
	if strings.TrimSpace(domain) == "" || len(dns) == 0 {
		return nil, fmt.Errorf("external KubeletConfiguration requires clusterDomain and clusterDNS from the existing cluster")
	}
	for _, value := range dns {
		address, ok := value.(string)
		if _, err := netip.ParseAddr(address); !ok || err != nil {
			return nil, fmt.Errorf("clusterDNS must contain IP addresses")
		}
	}
	config["cgroupDriver"] = "systemd"
	config["containerRuntimeEndpoint"] = "unix:///run/containerd/containerd.sock"
	config["rotateCertificates"] = true
	config["authentication"] = map[string]any{"anonymous": map[string]any{"enabled": false}, "webhook": map[string]any{"enabled": true}, "x509": map[string]any{"clientCAFile": "/etc/kubernetes/pki/ca.crt"}}
	config["authorization"] = map[string]any{"mode": "Webhook"}
	return yaml.Marshal(config)
}
