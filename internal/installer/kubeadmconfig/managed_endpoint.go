package kubeadmconfig

import (
	"bytes"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidateManagedEndpoint rejects native kubeadm settings that would prevent
// the API server from accepting traffic for the endpoint Katl advertises.
func ValidateManagedEndpoint(plan Plan, vip string, port int) error {
	address, err := netip.ParseAddr(strings.TrimSpace(vip))
	if err != nil || !address.Is4() {
		return fmt.Errorf("managed endpoint VIP must be an IPv4 address")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("managed endpoint port must be between 1 and 65535")
	}
	if err := validateEndpointDocuments(plan.Config.Content, address.String(), port); err != nil {
		return fmt.Errorf("KubeadmConfig %q: %w", plan.Name, err)
	}
	for _, patch := range plan.Patches {
		if !strings.HasPrefix(filepath.Base(patch.RenderPath), "kube-apiserver") {
			continue
		}
		if err := validateEndpointPatch(patch.Content, address.String(), port); err != nil {
			return fmt.Errorf("KubeadmConfig %q patch %q: %w", plan.Name, filepath.Base(patch.RenderPath), err)
		}
	}
	return nil
}

func validateEndpointDocuments(data []byte, vip string, port int) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("decode native kubeadm input: %w", err)
		}
		if fmt.Sprint(document["kind"]) != "ClusterConfiguration" {
			continue
		}
		apiServer, _ := document["apiServer"].(map[string]any)
		if err := validateEndpointExtraArgs(apiServer["extraArgs"], vip, port); err != nil {
			return err
		}
	}
}

func validateEndpointExtraArgs(value any, vip string, port int) error {
	switch args := value.(type) {
	case []any:
		for _, item := range args {
			arg, _ := item.(map[string]any)
			if err := validateEndpointFlag(fmt.Sprint(arg["name"]), fmt.Sprint(arg["value"]), vip, port); err != nil {
				return err
			}
		}
	case map[string]any:
		for name, value := range args {
			if err := validateEndpointFlag(name, fmt.Sprint(value), vip, port); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateEndpointPatch(data []byte, vip string, port int) error {
	var document any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("decode patch: %w", err)
	}
	var scalars []string
	collectStrings(document, &scalars)
	for index, scalar := range scalars {
		name, value, found := strings.Cut(strings.TrimSpace(scalar), "=")
		if !found {
			if name != "--bind-address" && name != "--secure-port" {
				continue
			}
			if index+1 >= len(scalars) {
				return fmt.Errorf("%s requires an explicit value which Katl can validate", name)
			}
			value = scalars[index+1]
		}
		if err := validateEndpointFlag(strings.TrimPrefix(name, "--"), value, vip, port); err != nil {
			return err
		}
	}
	return nil
}

func collectStrings(value any, out *[]string) {
	switch value := value.(type) {
	case string:
		*out = append(*out, value)
	case []any:
		for _, item := range value {
			collectStrings(item, out)
		}
	case map[string]any:
		for _, item := range value {
			collectStrings(item, out)
		}
	}
}

func validateEndpointFlag(name, value, vip string, port int) error {
	switch strings.TrimPrefix(strings.TrimSpace(name), "--") {
	case "bind-address":
		value = strings.TrimSpace(value)
		if value != "0.0.0.0" && value != vip {
			return fmt.Errorf("apiServer bind-address %q does not accept the managed VIP %s; remove it or use 0.0.0.0", value, vip)
		}
	case "secure-port":
		value = strings.TrimSpace(value)
		configured, err := strconv.Atoi(value)
		if err != nil || configured != port {
			return fmt.Errorf("apiServer secure-port %q conflicts with the managed endpoint port %d", value, port)
		}
	}
	return nil
}
