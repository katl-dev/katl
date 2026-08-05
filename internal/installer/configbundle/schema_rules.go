package configbundle

import "reflect"

type schemaFieldRule struct {
	Required      bool
	Description   string
	Default       any
	Enum          []any
	MinItems      *int
	MaxItems      *int
	MinLength     *int
	MaxLength     *int
	Pattern       string
	Format        string
	Minimum       *int64
	Maximum       *int64
	PropertyNames *schemaObject
}

type schemaTypeRule struct {
	OneOf []schemaObject
}

func applySchemaFieldRule(schema schemaObject, rule schemaFieldRule) schemaObject {
	schema.Description = rule.Description
	schema.Default = rule.Default
	schema.Enum = rule.Enum
	schema.MinItems = rule.MinItems
	schema.MaxItems = rule.MaxItems
	schema.MinLength = rule.MinLength
	schema.MaxLength = rule.MaxLength
	schema.Pattern = rule.Pattern
	schema.Format = rule.Format
	if rule.Minimum != nil {
		schema.Minimum = rule.Minimum
	}
	if rule.Maximum != nil {
		schema.Maximum = rule.Maximum
	}
	schema.PropertyNames = rule.PropertyNames
	return schema
}

func applySchemaTypeRule(schema schemaObject, rule schemaTypeRule) schemaObject {
	schema.OneOf = rule.OneOf
	return schema
}

func sourceSchemaFieldRule(t reflect.Type, field string) schemaFieldRule {
	key := schemaTypeName(t) + "." + field
	switch key {
	case "configbundle.SourceConfig.apiVersion":
		return description("API version of this ClusterConfig document.")
	case "configbundle.SourceConfig.kind":
		return description("Document kind; always ClusterConfig.")
	case "configbundle.SourceConfig.metadata":
		return description("Cluster identity.")
	case "configbundle.SourceConfig.spec":
		return description("Cluster-wide defaults and explicit node intent.")
	case "configbundle.Metadata.name":
		return stringRule("Stable cluster name.", "", 1, 0)
	case "configbundle.SourceSpec.controlPlaneEndpoint":
		return description("Stable Kubernetes API endpoint and optional Katl-managed advertisement.")
	case "configbundle.SourceSpec.kubernetes":
		return schemaFieldRule{Required: true, Description: "Cluster-wide Kubernetes version and optional native kubeadm input."}
	case "configbundle.SourceSpec.defaults":
		return description("Non-identifying values inherited by every node unless overridden.")
	case "configbundle.SourceSpec.nodes":
		return arrayRule("Explicit nodes managed as members of this cluster.", 1)
	case "configbundle.SourceNode.name":
		return stringRule("Stable node name and default hostname.", dnsLabelPattern, 1, 63)
	case "configbundle.SourceNode.controlPlane":
		return schemaFieldRule{Description: "Whether this node is a Kubernetes control-plane member.", Default: false}
	case "configbundle.SourceNode.access", "configbundle.SourceNodeLayer.access":
		return description("Operator SSH access installed on the node.")
	case "configbundle.SourceNode.kernel", "configbundle.SourceNodeLayer.kernel":
		return description("Kernel command-line options owned by the operator.")
	case "configbundle.SourceNode.hostConfiguration", "configbundle.SourceNodeLayer.hostConfiguration":
		return description("Native host sysfs settings and named file sets.")
	case "configbundle.SourceNode.systemExtensions", "configbundle.SourceNodeLayer.systemExtensions":
		return description("Named system-extension bundles and their bounded configuration.")
	case "configbundle.SourceNode.install", "configbundle.SourceNodeLayer.install":
		return description("System disk selection constraints.")
	case "configbundle.SourceNode.storage", "configbundle.SourceNodeLayer.storage":
		return description("Persistent data volumes layered by name.")
	case "configbundle.SourceNode.kubernetes", "configbundle.SourceNodeLayer.kubernetes":
		return description("Per-node Kubernetes address, labels, and taints.")
	case "configbundle.SourceNode.management":
		return description("Workstation targeting used by katlctl; not persisted node state.")
	case "configbundle.SourceManagementLayer.address":
		return stringRule("Address used by katlctl to reach this node.", "", 1, 0)
	case "configbundle.SourceAccess.ssh":
		return description("SSH authorized-key configuration.")
	case "configbundle.SourceSSHAccess.authorizedKeys":
		return description("OpenSSH public keys authorized for operator access; an empty node list clears inherited keys.")
	case "configbundle.SourceKernelConfig.commandLine":
		return description("Complete kernel command-line option list; an empty node list clears inherited options.")
	case "configbundle.SourceHostConfiguration.sysfs":
		return description("Complete ordered sysfs setting list; an empty node list clears inherited settings.")
	case "configbundle.SourceHostConfiguration.fileSets":
		return mapRule("Named native /etc file sets; an empty node map clears inherited sets.", dnsLabelPattern)
	case "configbundle.SourceHostConfigurationSysfsSetting.path":
		return stringRule("Normalized writable path below /sys.", `^/sys/`, 6, 0)
	case "configbundle.SourceHostConfigurationSysfsSetting.value":
		return stringRule("Non-empty single-line value written at boot.", `^[^\r\n]+$`, 1, 0)
	case "configbundle.SourceHostConfigurationFileSet.state":
		return enumRule("Whether Katl manages or removes this named file set.", "present", "", "present", "absent")
	case "configbundle.SourceHostConfigurationFileSet.files":
		return description("Files owned by this set when present.")
	case "configbundle.SourceHostConfigurationFileSet.onChange":
		return description("Bounded systemd notifications after this set changes.")
	case "configbundle.SourceSystemExtension.name":
		return stringRule("Stable extension name used for layering and status.", dnsLabelPattern, 1, 63)
	case "configbundle.SourceSystemExtension.state":
		return enumRule("Whether the extension is present or removed.", "present", "", "present", "absent")
	case "configbundle.SourceSystemExtension.bundle":
		return stringRule("OCI bundle reference including registry, repository, and tag.", "", 1, 0)
	case "configbundle.SourceSystemExtension.configuration":
		return description("Files consumed by the selected extension.")
	case "configbundle.SourceSystemExtension.units":
		return description("Systemd units and drop-ins activated with the extension.")
	case "configbundle.SourceInstallLayer.systemDisk":
		return description("System disk selector. Defaults may provide only non-identifying size policy; each effective node requires one stable identity.")
	case "configbundle.SourceStorageLayer.disks":
		return description("Named whole-disk or partition-backed volumes.")
	case "configbundle.SourceStorageDisk.name":
		return stringRule("Volume name used to derive its GPT label and /var/mnt path.", dnsLabelPattern, 1, 63)
	case "configbundle.SourceStorageDisk.state":
		return enumRule("Whether Katl manages or stops managing this volume.", "present", "", "present", "absent")
	case "configbundle.SourceStorageDisk.selector":
		return description("Exactly one whole-disk or partition selector.")
	case "configbundle.SourceStorageDisk.filesystem":
		return enumRule("Filesystem required on the selected target.", nil, "xfs", "ext4", "btrfs")
	case "configbundle.SourceStorageDisk.wipe":
		return schemaFieldRule{Description: "Whether provisioning may format the selected target.", Default: false}
	case "configbundle.SourceVolumeSelector.disk":
		return description("Select a safe whole disk.")
	case "configbundle.SourceVolumeSelector.partition":
		return description("Select an existing partition; an empty object uses the u-{volume-name} GPT label convention.")
	case "configbundle.SourceDiskSelector.byID":
		return stringRule("Absolute stable /dev/disk/by-id path; an empty value clears an inherited identity.", `^(?:|/dev/disk/by-id/[^/]+)$`, 0, 0)
	case "configbundle.SourceDiskSelector.wwn":
		return description("Stable device world-wide name; an empty value clears an inherited identity.")
	case "configbundle.SourceDiskSelector.serial":
		return description("Stable device serial number; an empty value clears an inherited identity.")
	case "configbundle.SourceDiskSelector.minSizeMiB":
		return integerRule("Minimum acceptable device size in MiB.", 0, 0)
	case "configbundle.SourcePartitionSelector.byID":
		return stringRule("Absolute stable partition /dev/disk/by-id path; an empty value clears an inherited identity.", `^(?:|/dev/disk/by-id/[^/]+)$`, 0, 0)
	case "configbundle.SourcePartitionSelector.partUUID":
		return schemaFieldRule{Description: "GPT partition UUID; an empty value clears an inherited identity.", Pattern: `^(?:|[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})$`}
	case "configbundle.SourcePartitionSelector.filesystemUUID":
		return description("Existing filesystem UUID; an empty value clears an inherited identity.")
	case "configbundle.SourceKubernetesCluster.version":
		return schemaFieldRule{Required: true, Description: "Exact Kubernetes patch version compiled into the cluster.", Pattern: `^v[0-9]+\.[0-9]+\.[0-9]+$`, MinLength: intPointer(1)}
	case "configbundle.SourceKubernetesCluster.kubeadm":
		return description("Optional native kubeadm configuration and patches.")
	case "configbundle.SourceKubeadmInput.configFile":
		return schemaFieldRule{Required: true, Description: "Relative path to the multi-document kubeadm configuration file.", MinLength: intPointer(1)}
	case "configbundle.SourceKubeadmInput.patchesDir":
		return stringRule("Optional relative directory containing kubeadm patches.", "", 1, 0)
	case "configbundle.SourceKubernetesLayer.address":
		return stringRule("Literal unicast IP used as the Kubernetes node address.", "", 2, 0)
	case "configbundle.SourceKubernetesLayer.labels":
		return mapRule("Kubernetes labels merged by key; an empty node map clears inherited labels.", kubernetesLabelKeyPattern)
	case "configbundle.SourceKubernetesLayer.taints":
		return description("Complete Kubernetes taint list; an empty node list clears inherited taints.")
	case "configbundle.SourceKubernetesLayer.kubelet":
		return description("Per-node native KubeletConfiguration applied through kubeadm's bounded kubelet patch path.")
	case "configbundle.SourceKubeletConfig.configFile":
		return schemaFieldRule{Required: true, Description: "Relative path to one kubelet.config.k8s.io/v1beta1 KubeletConfiguration document.", MinLength: intPointer(1)}
	case "controlplaneendpoint.Config.host":
		return stringRule("Stable Kubernetes API DNS name or IP address.", "", 1, 253)
	case "controlplaneendpoint.Config.port":
		return integerDefaultRule("Kubernetes API port; zero selects the default.", 0, 65535, 6443)
	case "controlplaneendpoint.Config.advertisement":
		return description("Optional Katl-managed routed API endpoint advertisement.")
	case "controlplaneendpoint.Advertisement.vip":
		return schemaFieldRule{Description: "Bare routed IPv4 API address.", Format: "ipv4"}
	case "controlplaneendpoint.Advertisement.bgp":
		return description("BGP peers and optional route exchanges.")
	case "controlplaneendpoint.BGP.localASN":
		return integerRule("Local BGP autonomous system number.", 1, 4294967294)
	case "controlplaneendpoint.BGP.peers":
		return arrayRule("BGP peers receiving the API route.", 1)
	case "controlplaneendpoint.BGP.routeExchanges":
		return description("Optional bounded route-exchange listeners.")
	case "controlplaneendpoint.Peer.address":
		return schemaFieldRule{Description: "Usable peer IPv4 address.", Format: "ipv4"}
	case "controlplaneendpoint.Peer.asn":
		return integerRule("Peer autonomous system number.", 1, 4294967294)
	case "controlplaneendpoint.RouteExchange.name":
		return stringRule("DNS-label-style route exchange name.", dnsLabelPattern, 1, 63)
	case "controlplaneendpoint.RouteExchange.listenPort":
		return integerDefaultRule("BGP listener port; zero selects 179 when one exchange exists.", 0, 65535, 179)
	case "controlplaneendpoint.RouteExchange.peerASN":
		return integerRule("Expected peer autonomous system number; zero selects localASN.", 0, 4294967294)
	case "controlplaneendpoint.RouteExchange.exportToFabric":
		return description("IPv4 prefix envelopes eligible for export.")
	case "controlplaneendpoint.PrefixEnvelope.cidr":
		return schemaFieldRule{Description: "IPv4 CIDR eligible for export.", Pattern: `^[0-9.]+/[0-9]{1,2}$`}
	case "controlplaneendpoint.PrefixEnvelope.exactPrefixLength":
		return integerRule("Optional exact exported prefix length.", 0, 32)
	case "manifest.HostConfigurationFile.path":
		return stringRule("Normalized file path below /etc.", `^/etc/`, 6, 0)
	case "manifest.HostConfigurationFile.content":
		return description("Inline UTF-8 file content, mutually exclusive with source.")
	case "manifest.HostConfigurationFile.source":
		return stringRule("Relative source file path, mutually exclusive with content.", "", 1, 0)
	case "manifest.HostConfigurationFile.mode":
		return schemaFieldRule{Description: "File mode; zero selects 0644, or set 0600, 0640, or 0644.", Default: 420, Enum: []any{0, 384, 416, 420}}
	case "manifest.HostConfigurationNotifications.systemd":
		return description("Systemd units notified after files change.")
	case "manifest.HostConfigurationSystemdNotification.unit":
		return stringRule("Single systemd unit name.", systemdUnitPattern, 1, 255)
	case "manifest.HostConfigurationSystemdNotification.action":
		return enumRule("Bounded non-disruptive systemd action.", nil, "reload", "try-reload-or-restart", "try-restart")
	case "manifest.SystemExtensionConfiguration.files":
		return description("Files delivered to the extension configuration namespace.")
	case "manifest.SystemExtensionUnit.name":
		return stringRule("Single systemd unit name owned by the extension.", systemdUnitPattern, 1, 255)
	case "manifest.SystemExtensionUnit.enable":
		return schemaFieldRule{Description: "Enable the unit in the generated configuration.", Default: false}
	case "manifest.SystemExtensionUnit.requiredForBootHealth":
		return schemaFieldRule{Description: "Require the unit to be healthy before boot succeeds.", Default: false}
	case "manifest.SystemExtensionUnit.dropIns":
		return description("Named systemd drop-ins for this unit.")
	case "manifest.SystemExtensionUnitDropIn.name":
		return schemaFieldRule{Description: "Safe basename ending in .conf.", Pattern: `^[^/]+\.conf$`, MinLength: intPointer(6)}
	case "manifest.SystemExtensionUnitDropIn.content":
		return description("Inline UTF-8 drop-in content, mutually exclusive with source.")
	case "manifest.SystemExtensionUnitDropIn.source":
		return stringRule("Relative source file path, mutually exclusive with content.", "", 1, 0)
	case "manifest.NodeTaint.key":
		return stringRule("Kubernetes taint key.", kubernetesLabelKeyPattern, 1, 253)
	case "manifest.NodeTaint.value":
		return schemaFieldRule{Description: "Optional Kubernetes taint value.", MaxLength: intPointer(63)}
	case "manifest.NodeTaint.effect":
		return enumRule("Kubernetes scheduling effect.", nil, "NoSchedule", "PreferNoSchedule", "NoExecute")
	default:
		return schemaFieldRule{}
	}
}

func sourceSchemaTypeRule(t reflect.Type) schemaTypeRule {
	switch schemaTypeName(t) {
	case "configbundle.SourceVolumeSelector":
		return schemaTypeRule{OneOf: exactlyOneRequired("disk", "partition")}
	case "configbundle.SourceDiskSelector":
		return schemaTypeRule{OneOf: atMostOneRequired("byID", "wwn", "serial")}
	case "configbundle.SourcePartitionSelector":
		return schemaTypeRule{OneOf: atMostOneRequired("byID", "partUUID", "filesystemUUID")}
	case "manifest.HostConfigurationFile":
		return schemaTypeRule{OneOf: exactlyOneRequired("content", "source")}
	case "manifest.SystemExtensionUnitDropIn":
		return schemaTypeRule{OneOf: exactlyOneRequired("content", "source")}
	default:
		return schemaTypeRule{}
	}
}

func exactlyOneRequired(first, second string) []schemaObject {
	return []schemaObject{
		{Required: []string{first}, Not: &schemaObject{Required: []string{second}}},
		{Required: []string{second}, Not: &schemaObject{Required: []string{first}}},
	}
}

func atMostOneRequired(fields ...string) []schemaObject {
	nonEmpty := make([]schemaObject, len(fields))
	for i, field := range fields {
		nonEmpty[i] = nonEmptyRequired(field)
	}
	branches := []schemaObject{{Not: &schemaObject{AnyOf: nonEmpty}}}
	for i, field := range fields {
		others := make([]schemaObject, 0, len(fields)-1)
		for j := range fields {
			if i != j {
				others = append(others, nonEmpty[j])
			}
		}
		branches = append(branches, schemaObject{
			Required:   []string{field},
			Properties: nonEmpty[i].Properties,
			Not:        &schemaObject{AnyOf: others},
		})
	}
	return branches
}

func nonEmptyRequired(field string) schemaObject {
	return schemaObject{
		Required: []string{field},
		Properties: map[string]schemaObject{
			field: {Not: &schemaObject{Const: ""}},
		},
	}
}

const (
	dnsLabelPattern           = `^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`
	kubernetesLabelKeyPattern = `^(?:[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?/)?[A-Za-z0-9](?:[-A-Za-z0-9_.]*[A-Za-z0-9])?$`
	systemdUnitPattern        = `^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.(?:service|socket|target|timer|path|mount|automount|slice|scope|device|swap)$`
)

func description(value string) schemaFieldRule {
	return schemaFieldRule{Description: value}
}

func stringRule(description, pattern string, minLength, maxLength int) schemaFieldRule {
	rule := schemaFieldRule{Description: description, Pattern: pattern}
	if minLength > 0 {
		rule.MinLength = intPointer(minLength)
	}
	if maxLength > 0 {
		rule.MaxLength = intPointer(maxLength)
	}
	return rule
}

func enumRule(description string, defaultValue any, values ...string) schemaFieldRule {
	enums := make([]any, len(values))
	for i, value := range values {
		enums[i] = value
	}
	return schemaFieldRule{Description: description, Default: defaultValue, Enum: enums}
}

func integerRule(description string, minimum, maximum int64) schemaFieldRule {
	rule := schemaFieldRule{Description: description, Minimum: int64Pointer(minimum)}
	if maximum > 0 {
		rule.Maximum = int64Pointer(maximum)
	}
	return rule
}

func integerDefaultRule(description string, minimum, maximum int64, defaultValue int) schemaFieldRule {
	rule := integerRule(description, minimum, maximum)
	rule.Default = defaultValue
	return rule
}

func arrayRule(description string, minItems int) schemaFieldRule {
	return schemaFieldRule{Description: description, MinItems: intPointer(minItems)}
}

func mapRule(description, keyPattern string) schemaFieldRule {
	return schemaFieldRule{Description: description, PropertyNames: &schemaObject{Pattern: keyPattern}}
}

func intPointer(value int) *int {
	return &value
}

func int64Pointer(value int64) *int64 {
	return &value
}
