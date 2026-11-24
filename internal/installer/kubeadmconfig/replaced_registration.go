package kubeadmconfig

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

// ReplacedRegistration names the registration fields plan sets that ForNode
// replaces with a different value for node, each followed by the ClusterConfig
// field that owns it. Fields plan does not set, or sets to what ForNode
// produces, are not reported, so input rendered by ForNode reports nothing.
func ReplacedRegistration(plan Plan, node manifest.NodeConfig) ([]string, error) {
	input, err := ForNode(plan, node)
	if err != nil {
		return nil, err
	}
	authored, err := registrationFields(plan)
	if err != nil {
		return nil, err
	}
	registered, err := registrationFields(input.plan)
	if err != nil {
		return nil, err
	}
	var replaced []string
	for name, value := range authored {
		if got, ok := registered[name]; !ok || !reflect.DeepEqual(got, value) {
			replaced = append(replaced, name)
		}
	}
	sort.Strings(replaced)
	return replaced, nil
}

const (
	addressOwner = "nodes[].kubernetes.address"
	labelsOwner  = "nodes[].kubernetes.labels"
	taintsOwner  = "nodes[].kubernetes.taints"
)

// registrationFields returns the values of the fields withNodeRegistration
// owns that plan sets, keyed by their display name.
func registrationFields(plan Plan) (map[string]any, error) {
	documents, err := decodeDocuments(plan.Config.Content)
	if err != nil {
		return nil, fmt.Errorf("decode KubeadmConfig %q registration: %w", plan.Name, err)
	}
	fields := map[string]any{}
	for _, document := range documents {
		kind, _ := document["kind"].(string)
		if kind != "InitConfiguration" && kind != "JoinConfiguration" {
			continue
		}
		if registration, ok := document["nodeRegistration"].(map[string]any); ok {
			if taints, ok := registration["taints"]; ok {
				fields[kind+" nodeRegistration.taints ("+taintsOwner+")"] = taints
			}
			extraArgs, _ := registration["kubeletExtraArgs"].([]any)
			for _, raw := range extraArgs {
				argument, _ := raw.(map[string]any)
				name, _ := argument["name"].(string)
				value := strings.TrimSpace(fmt.Sprint(argument["value"]))
				switch strings.TrimSpace(name) {
				case kubeletNodeIPArgument:
					fields[kind+" nodeRegistration.kubeletExtraArgs node-ip ("+addressOwner+")"] = value
				case kubeletNodeLabelsArgument:
					var labels []string
					for _, label := range strings.Split(value, ",") {
						if label = strings.TrimSpace(label); label != "" {
							labels = append(labels, label)
						}
					}
					slices.Sort(labels)
					fields[kind+" nodeRegistration.kubeletExtraArgs node-labels ("+labelsOwner+")"] = labels
				}
			}
		}
		path := "localAPIEndpoint.advertiseAddress"
		endpoint := document
		if kind == "JoinConfiguration" {
			path = "controlPlane." + path
			endpoint, _ = document["controlPlane"].(map[string]any)
		}
		if local, ok := endpoint["localAPIEndpoint"].(map[string]any); ok {
			if address, ok := local["advertiseAddress"]; ok {
				fields[kind+" "+path+" ("+addressOwner+")"] = strings.TrimSpace(fmt.Sprint(address))
			}
		}
	}
	return fields, nil
}
