package apivip

import (
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "apps.katl.dev/v1alpha1"
	Kind       = "APIEndpointVIP"

	AppID = "api-vip"

	ConfigPath           = "/etc/katl/apps/api-vip/config.yaml"
	OwnershipEnabledPath = "/etc/katl/apps/api-vip/ownership-enabled"
	LiveStatusPath       = "/run/katl/apps/api-vip/status.json"
	OperationStatus      = "/var/lib/katl/operations/<operation-id>/apps/api-vip/status.json"
	AppDropInPath        = "/etc/systemd/system/katl-app-api-vip.service.d/10-katl-config.conf"
	KubeletDropInPath    = "/etc/systemd/system/kubelet.service.d/20-katl-control-plane-endpoint.conf"
	NetworkPath          = "/etc/systemd/network/05-katl-api-vip.network"
	DummyNetDevPath      = "/etc/systemd/network/05-katl-api-vip.netdev"
	defaultHealthPath    = "/readyz"
	defaultHealthTimeout = "5s"
	defaultInterfaceName = "katl-api"
	defaultEndpointPort  = 6443
)

var interfaceNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,14}$`)

type Object struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`
	Kind       string `yaml:"kind" json:"kind"`
	Spec       Config `yaml:"spec" json:"spec"`
}

type Config struct {
	Endpoint     Endpoint     `yaml:"endpoint" json:"endpoint"`
	VIPInterface VIPInterface `yaml:"vipInterface" json:"vipInterface"`
	ActivateOn   ActivateOn   `yaml:"activateOn" json:"activateOn"`
	Ownership    Ownership    `yaml:"ownership" json:"ownership"`
	Health       Health       `yaml:"health" json:"health"`
	Status       StatusConfig `yaml:"status,omitempty" json:"status,omitempty"`
}

type Endpoint struct {
	Host          string `yaml:"host" json:"host"`
	Port          int    `yaml:"port,omitempty" json:"port,omitempty"`
	VIP           string `yaml:"vip" json:"vip"`
	AddressFamily string `yaml:"addressFamily,omitempty" json:"addressFamily,omitempty"`
	TLSServerName string `yaml:"tlsServerName,omitempty" json:"tlsServerName,omitempty"`
	Provenance    string `yaml:"provenance,omitempty" json:"provenance,omitempty"`
}

type VIPInterface struct {
	Kind string `yaml:"kind" json:"kind"`
	Name string `yaml:"name" json:"name"`
	MTU  int    `yaml:"mtu,omitempty" json:"mtu,omitempty"`
}

type ActivateOn struct {
	Roles []string `yaml:"roles,omitempty" json:"roles,omitempty"`
}

type Ownership struct {
	Enabled             *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	StartReleased       *bool `yaml:"startReleased,omitempty" json:"startReleased,omitempty"`
	AcquireAfterHealthy *bool `yaml:"acquireAfterHealthy,omitempty" json:"acquireAfterHealthy,omitempty"`
	ReleaseOnFailure    *bool `yaml:"releaseOnFailure,omitempty" json:"releaseOnFailure,omitempty"`
}

type Health struct {
	Probe            string `yaml:"probe,omitempty" json:"probe,omitempty"`
	Scheme           string `yaml:"scheme,omitempty" json:"scheme,omitempty"`
	Host             string `yaml:"host,omitempty" json:"host,omitempty"`
	Port             int    `yaml:"port,omitempty" json:"port,omitempty"`
	Path             string `yaml:"path,omitempty" json:"path,omitempty"`
	Interval         string `yaml:"interval,omitempty" json:"interval,omitempty"`
	Timeout          string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	SuccessThreshold int    `yaml:"successThreshold,omitempty" json:"successThreshold,omitempty"`
	FailureThreshold int    `yaml:"failureThreshold,omitempty" json:"failureThreshold,omitempty"`
	CARef            string `yaml:"caRef,omitempty" json:"caRef,omitempty"`
	TLSServerName    string `yaml:"tlsServerName,omitempty" json:"tlsServerName,omitempty"`
}

type StatusConfig struct {
	LiveStatusPath      string `yaml:"liveStatusPath,omitempty" json:"liveStatusPath,omitempty"`
	OperationStatusPath string `yaml:"operationStatusPath,omitempty" json:"operationStatusPath,omitempty"`
}

type RenderRequest struct {
	Config   Config
	NodeRole string
}

type Plan struct {
	Config Config
	Files  []confext.NativeEtcFile
}

func FromControlPlaneEndpoint(plan controlplaneendpoint.Plan) (Config, error) {
	if plan.Config.Advertisement == nil {
		return Config{}, fmt.Errorf("managed control-plane endpoint intent is required")
	}
	return Normalize(Config{
		Endpoint: Endpoint{
			Host:          plan.Config.Host,
			Port:          plan.Config.Port,
			VIP:           plan.VIPPrefix,
			AddressFamily: "ipv4",
			TLSServerName: plan.Config.Host,
			Provenance:    "platform-host",
		},
		VIPInterface: VIPInterface{Kind: "dummy", Name: defaultInterfaceName},
		ActivateOn:   ActivateOn{Roles: []string{"control-plane"}},
		Ownership: Ownership{
			Enabled:             boolPtr(true),
			StartReleased:       boolPtr(true),
			AcquireAfterHealthy: boolPtr(true),
			ReleaseOnFailure:    boolPtr(true),
		},
	})
}

func (p Plan) NativeEtcFiles() []confext.NativeEtcFile {
	return slices.Clone(p.Files)
}

func Decode(reader io.Reader) (Object, error) {
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	var object Object
	if err := decoder.Decode(&object); err != nil {
		return Object{}, fmt.Errorf("decode APIEndpointVIP: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Object{}, fmt.Errorf("decode APIEndpointVIP: multiple YAML documents")
		}
		return Object{}, fmt.Errorf("decode APIEndpointVIP: %w", err)
	}
	if object.APIVersion != APIVersion {
		return Object{}, fmt.Errorf("APIEndpointVIP apiVersion must be %s", APIVersion)
	}
	if object.Kind != Kind {
		return Object{}, fmt.Errorf("APIEndpointVIP kind must be %s", Kind)
	}
	return object, nil
}

func Normalize(config Config) (Config, error) {
	config.Endpoint.Host = strings.TrimSpace(config.Endpoint.Host)
	config.Endpoint.VIP = strings.TrimSpace(config.Endpoint.VIP)
	config.Endpoint.AddressFamily = strings.TrimSpace(config.Endpoint.AddressFamily)
	config.Endpoint.TLSServerName = strings.TrimSpace(config.Endpoint.TLSServerName)
	config.Endpoint.Provenance = strings.TrimSpace(config.Endpoint.Provenance)
	if config.Endpoint.Port == 0 {
		config.Endpoint.Port = defaultEndpointPort
	}
	if config.Endpoint.TLSServerName == "" {
		config.Endpoint.TLSServerName = config.Endpoint.Host
	}
	if config.Endpoint.Provenance == "" {
		config.Endpoint.Provenance = "platform-host"
	}
	vip, err := validateVIP(config.Endpoint)
	if err != nil {
		return Config{}, err
	}
	if config.Endpoint.AddressFamily == "" {
		config.Endpoint.AddressFamily = "ipv6"
		if vip.Addr().Is4() {
			config.Endpoint.AddressFamily = "ipv4"
		}
	}
	if err := validateEndpoint(config.Endpoint, vip); err != nil {
		return Config{}, err
	}
	if err := normalizeInterface(&config.VIPInterface); err != nil {
		return Config{}, err
	}
	if len(config.ActivateOn.Roles) == 0 {
		config.ActivateOn.Roles = []string{"control-plane"}
	}
	for i, role := range config.ActivateOn.Roles {
		if strings.TrimSpace(role) != "control-plane" {
			return Config{}, fmt.Errorf("activateOn.roles[%d] must be control-plane", i)
		}
	}
	ownership := config.Ownership
	if ownership.Enabled == nil {
		ownership.Enabled = boolPtr(true)
	}
	if ownership.StartReleased == nil {
		ownership.StartReleased = boolPtr(true)
	}
	if ownership.AcquireAfterHealthy == nil {
		ownership.AcquireAfterHealthy = boolPtr(true)
	}
	if ownership.ReleaseOnFailure == nil {
		ownership.ReleaseOnFailure = boolPtr(true)
	}
	if !*ownership.StartReleased {
		return Config{}, fmt.Errorf("ownership.startReleased must be true")
	}
	if !*ownership.AcquireAfterHealthy {
		return Config{}, fmt.Errorf("ownership.acquireAfterHealthy must be true")
	}
	if !*ownership.ReleaseOnFailure {
		return Config{}, fmt.Errorf("ownership.releaseOnFailure must be true")
	}
	config.Ownership = ownership
	config.Health, err = normalizeHealth(config.Health, config.Endpoint, vip)
	if err != nil {
		return Config{}, err
	}
	config.Status.LiveStatusPath = LiveStatusPath
	config.Status.OperationStatusPath = OperationStatus
	return config, nil
}

func RenderNativeEtcFiles(request RenderRequest) (Plan, error) {
	if strings.TrimSpace(request.NodeRole) != "control-plane" {
		return Plan{}, fmt.Errorf("node role %q cannot own the API VIP; control-plane is required", request.NodeRole)
	}
	config, err := Normalize(request.Config)
	if err != nil {
		return Plan{}, err
	}
	files := []confext.NativeEtcFile{
		{Path: NetworkPath, Content: renderNetwork(config), Mode: 0o644},
		{Path: ConfigPath, Content: renderAppConfig(config), Mode: 0o644},
		{Path: AppDropInPath, Content: renderAppDropIn(config), Mode: 0o644},
		{Path: KubeletDropInPath, Content: renderKubeletDropIn(), Mode: 0o644},
	}
	if *config.Ownership.Enabled {
		files = append(files, confext.NativeEtcFile{Path: OwnershipEnabledPath, Content: "enabled\n", Mode: 0o644})
	}
	if config.VIPInterface.Kind == "dummy" {
		files = append(files, confext.NativeEtcFile{Path: DummyNetDevPath, Content: renderDummyNetDev(config.VIPInterface), Mode: 0o644})
	}
	plans, err := confext.ValidateNativeEtcBundle("", files)
	if err != nil {
		return Plan{}, err
	}
	contentByPath := make(map[string]confext.NativeEtcFile, len(files))
	for _, file := range files {
		contentByPath[filepath.Clean(file.Path)] = file
	}
	normalizedFiles := make([]confext.NativeEtcFile, 0, len(plans))
	for _, plan := range plans {
		file := contentByPath[plan.Path]
		normalizedFiles = append(normalizedFiles, confext.NativeEtcFile{Path: plan.Path, Content: file.Content, Mode: plan.Mode})
	}
	return Plan{Config: config, Files: normalizedFiles}, nil
}

func validateVIP(endpoint Endpoint) (netip.Prefix, error) {
	if endpoint.Host == "" {
		return netip.Prefix{}, fmt.Errorf("endpoint.host is required")
	}
	if endpoint.VIP == "" {
		return netip.Prefix{}, fmt.Errorf("endpoint.vip is required")
	}
	prefix, err := netip.ParsePrefix(endpoint.VIP)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("endpoint.vip %q is invalid: %w", endpoint.VIP, err)
	}
	prefix = prefix.Masked()
	if prefix.Bits() != 32 && prefix.Bits() != 128 {
		return netip.Prefix{}, fmt.Errorf("endpoint.vip must be a /32 or /128")
	}
	address := prefix.Addr()
	if !address.IsValid() || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() {
		return netip.Prefix{}, fmt.Errorf("endpoint.vip %q is not a usable host API VIP", endpoint.VIP)
	}
	return prefix, nil
}

func validateEndpoint(endpoint Endpoint, vip netip.Prefix) error {
	if endpoint.Port < 1 || endpoint.Port > 65535 {
		return fmt.Errorf("endpoint.port must be between 1 and 65535")
	}
	if endpoint.Provenance != "platform-host" {
		return fmt.Errorf("endpoint.provenance must be platform-host, got %q", endpoint.Provenance)
	}
	family := "ipv6"
	if vip.Addr().Is4() {
		family = "ipv4"
	}
	if endpoint.AddressFamily != family {
		return fmt.Errorf("endpoint.addressFamily %q does not match endpoint.vip family %s", endpoint.AddressFamily, family)
	}
	return nil
}

func normalizeInterface(vipInterface *VIPInterface) error {
	vipInterface.Kind = strings.TrimSpace(vipInterface.Kind)
	vipInterface.Name = strings.TrimSpace(vipInterface.Name)
	if vipInterface.Kind == "" {
		vipInterface.Kind = "dummy"
	}
	if vipInterface.Name == "" {
		vipInterface.Name = defaultInterfaceName
	}
	if vipInterface.Kind != "dummy" && vipInterface.Kind != "loopback" {
		return fmt.Errorf("vipInterface.kind must be dummy or loopback")
	}
	if !interfaceNameRE.MatchString(vipInterface.Name) || strings.Contains(vipInterface.Name, "/") || vipInterface.Name == "." || vipInterface.Name == ".." {
		return fmt.Errorf("vipInterface.name %q is not a safe Linux interface name", vipInterface.Name)
	}
	if vipInterface.MTU != 0 && (vipInterface.MTU < 68 || vipInterface.MTU > 65535) {
		return fmt.Errorf("vipInterface.mtu must be between 68 and 65535")
	}
	return nil
}

func normalizeHealth(health Health, endpoint Endpoint, vip netip.Prefix) (Health, error) {
	health.Probe = defaultString(health.Probe, "readyz")
	health.Scheme = defaultString(health.Scheme, "https")
	localHost := "127.0.0.1"
	if vip.Addr().Is6() {
		localHost = "::1"
	}
	health.Host = defaultString(health.Host, localHost)
	health.Path = defaultString(health.Path, defaultHealthPath)
	health.Interval = defaultString(health.Interval, "2s")
	health.Timeout = defaultString(health.Timeout, defaultHealthTimeout)
	health.CARef = defaultString(health.CARef, "kube-apiserver-ca")
	health.TLSServerName = defaultString(health.TLSServerName, endpoint.TLSServerName)
	if health.Port == 0 {
		health.Port = endpoint.Port
	}
	if health.SuccessThreshold == 0 {
		health.SuccessThreshold = 2
	}
	if health.FailureThreshold == 0 {
		health.FailureThreshold = 3
	}
	if health.Probe != "readyz" || health.Scheme != "https" || health.Host != localHost || health.Path != defaultHealthPath || health.Port != endpoint.Port {
		return Health{}, fmt.Errorf("health must probe the local kube-apiserver readyz endpoint")
	}
	if health.SuccessThreshold < 1 || health.FailureThreshold < 1 {
		return Health{}, fmt.Errorf("health thresholds must be positive")
	}
	if health.CARef != "kube-apiserver-ca" {
		return Health{}, fmt.Errorf("health.caRef must be kube-apiserver-ca")
	}
	return health, nil
}

func renderAppConfig(config Config) string {
	data, _ := yaml.Marshal(Object{APIVersion: APIVersion, Kind: Kind, Spec: config})
	return string(data)
}

func renderDummyNetDev(vipInterface VIPInterface) string {
	return fmt.Sprintf("[NetDev]\nName=%s\nKind=dummy\n", vipInterface.Name)
}

func renderNetwork(config Config) string {
	return fmt.Sprintf("[Match]\nName=%s\n\n[Link]\nRequiredForOnline=no\n", config.VIPInterface.Name)
}

func renderAppDropIn(config Config) string {
	return fmt.Sprintf("[Unit]\nConditionPathExists=%s\n\n[Service]\nExecStart=\nExecStart=/usr/lib/katl/endpoint-advertiser/katl-endpoint-advertiser --config %s\n", OwnershipEnabledPath, ConfigPath)
}

func renderKubeletDropIn() string {
	return "[Unit]\nAfter=katl-app-api-vip.service\n"
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func boolPtr(value bool) *bool {
	return &value
}
