package kubeconfig

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type Credentials struct {
	CertificateAuthorityData string
	ClientCertificateData    string
	ClientKeyData            string
}

// ParseCredentials reads embedded credentials without following file references
// or executing credential plugins supplied by a remote node.
func ParseCredentials(data []byte) (Credentials, error) {
	var config kubeconfigObject
	if err := yaml.Unmarshal(data, &config); err != nil {
		return Credentials{}, fmt.Errorf("parse admin kubeconfig: %w", err)
	}
	var selected context
	if config.CurrentContext == "" && len(config.Clusters) == 1 && len(config.Users) == 1 {
		selected = context{Cluster: config.Clusters[0].Name, User: config.Users[0].Name}
	} else {
		found := false
		for _, entry := range config.Contexts {
			if entry.Name == config.CurrentContext {
				if found {
					return Credentials{}, fmt.Errorf("admin kubeconfig has duplicate current contexts")
				}
				selected, found = entry.Context, true
			}
		}
		if !found {
			return Credentials{}, fmt.Errorf("admin kubeconfig current context was not found")
		}
	}
	var result Credentials
	clusters, users := 0, 0
	for _, entry := range config.Clusters {
		if entry.Name == selected.Cluster {
			clusters++
			result.CertificateAuthorityData = strings.TrimSpace(entry.Cluster.CertificateAuthorityData)
		}
	}
	for _, entry := range config.Users {
		if entry.Name == selected.User {
			users++
			result.ClientCertificateData = strings.TrimSpace(entry.User.ClientCertificateData)
			result.ClientKeyData = strings.TrimSpace(entry.User.ClientKeyData)
		}
	}
	if clusters != 1 || users != 1 || result.CertificateAuthorityData == "" || result.ClientCertificateData == "" || result.ClientKeyData == "" {
		return Credentials{}, fmt.Errorf("admin kubeconfig is missing unambiguous embedded credential data")
	}
	return result, nil
}
