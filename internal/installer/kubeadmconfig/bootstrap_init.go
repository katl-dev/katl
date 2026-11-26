package kubeadmconfig

import (
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// loopbackSAN is the address node-local API proxies forward TLS to.
const loopbackSAN = "127.0.0.1"

const waitControlPlanePhase = "wait-control-plane"

// HasDelegatedInitHealth reports whether the init config skips kubeadm's
// control-plane health check, as Katl's bootstrap init renderer requests.
func HasDelegatedInitHealth(content []byte) (bool, error) {
	documents, err := decodeDocuments(content)
	if err != nil {
		return false, err
	}
	for _, document := range documents {
		if document["kind"] == "InitConfiguration" {
			phases, _ := document["skipPhases"].([]any)
			if slices.Contains(phases, waitControlPlanePhase) {
				return true, nil
			}
		}
	}
	return false, nil
}

// BootstrapCertSANs returns the apiServer.certSANs that bootstrap adds to the
// cluster it initialises: the control-plane endpoint host, the loopback
// address, and the VIP Katl advertises for the endpoint, if any. It is the
// single owner of that set, so rendering init config and comparing a running
// cluster's ClusterConfiguration against desired input agree on it.
func BootstrapCertSANs(endpoint, vip string) ([]string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil || strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("control-plane endpoint must be host:port")
	}
	return certSANs(host, vip), nil
}

// ClusterCertSANs is BootstrapCertSANs for the controlPlaneEndpoint of a
// running cluster's ClusterConfiguration, which kubeadm accepts without a port.
func ClusterCertSANs(endpoint, vip string) []string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(endpoint, "["), "]")
	}
	return certSANs(host, vip)
}

func certSANs(host, vip string) []string {
	sans := []string{host, loopbackSAN}
	if vip = strings.TrimSpace(vip); vip != "" && !slices.Contains(sans, vip) {
		sans = append(sans, vip)
	}
	return sans
}

// RenderInitConfig returns base with the cluster endpoint and the SANs of
// BootstrapCertSANs added to its ClusterConfiguration. An empty endpoint
// leaves base unchanged.
func RenderInitConfig(base []byte, endpoint, vip string) ([]byte, error) {
	endpoint = strings.TrimSpace(endpoint)
	sans, err := BootstrapCertSANs(endpoint, vip)
	if err != nil {
		return nil, err
	}
	if endpoint == "" {
		return base, nil
	}
	docs, err := decodeDocuments(base)
	if err != nil {
		return nil, fmt.Errorf("decode kubeadm init config: %w", err)
	}
	found := false
	for _, doc := range docs {
		if kind, _ := doc["kind"].(string); kind != "ClusterConfiguration" {
			continue
		}
		found = true
		if configured := strings.TrimSpace(fmt.Sprint(doc["controlPlaneEndpoint"])); configured != "" && configured != "<nil>" && configured != endpoint {
			return nil, fmt.Errorf("kubeadm ClusterConfiguration controlPlaneEndpoint %q conflicts with cluster endpoint %q", configured, endpoint)
		}
		doc["controlPlaneEndpoint"] = endpoint
		apiServer, _ := doc["apiServer"].(map[string]any)
		if apiServer == nil {
			apiServer = map[string]any{}
		}
		for _, san := range sans {
			apiServer["certSANs"] = appendUniqueString(apiServer["certSANs"], san)
		}
		doc["apiServer"] = apiServer
	}
	if !found {
		return nil, errors.New("kubeadm init config did not contain ClusterConfiguration")
	}
	return encodeDocuments(docs)
}

func appendUniqueString(value any, item string) []any {
	item = strings.TrimSpace(item)
	var items []any
	switch typed := value.(type) {
	case []any:
		items = append(items, typed...)
	case []string:
		for _, existing := range typed {
			items = append(items, existing)
		}
	}
	for _, existing := range items {
		if text, ok := existing.(string); ok && strings.TrimSpace(text) == item {
			return items
		}
	}
	return append(items, item)
}

// delegateInitHealth makes kubeadm init skip wait-control-plane. Katl verifies
// local API, kubelet, and static-pod health before promotion, and kubeadm's
// wait reads the unpatched shared kubelet base.
func delegateInitHealth(content []byte) ([]byte, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	var documents []map[string]any
	found := false
	for {
		var document map[string]any
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode managed kubeadm init config: %w", err)
		}
		if len(document) == 0 {
			continue
		}
		if document["kind"] == "InitConfiguration" {
			found = true
			phases, ok := document["skipPhases"].([]any)
			if document["skipPhases"] != nil && !ok {
				return nil, fmt.Errorf("InitConfiguration.skipPhases must be a list of strings")
			}
			for _, phase := range phases {
				if _, ok := phase.(string); !ok {
					return nil, fmt.Errorf("InitConfiguration.skipPhases must be a list of strings")
				}
			}
			if !slices.Contains(phases, waitControlPlanePhase) {
				phases = append(phases, waitControlPlanePhase)
			}
			document["skipPhases"] = phases
		}
		documents = append(documents, document)
	}
	if !found {
		documents = append(documents, map[string]any{
			"apiVersion": "kubeadm.k8s.io/v1beta4",
			"kind":       "InitConfiguration",
			"skipPhases": []string{waitControlPlanePhase},
		})
	}
	out, err := encodeDocuments(documents)
	if err != nil {
		return nil, fmt.Errorf("encode managed kubeadm init config: %w", err)
	}
	return out, nil
}
