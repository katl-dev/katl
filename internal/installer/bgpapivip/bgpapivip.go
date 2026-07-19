package bgpapivip

import (
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "apps.katl.dev/v1alpha1"
	Kind       = "BGPAPIEndpoint"

	AppID        = "bgp-api-vip"
	BirdConfigID = "bird"

	ConfigPath        = "/etc/katl/apps/bgp-api-vip/config.yaml"
	BirdConfigPath    = "/etc/katl/apps/bird/bird.conf"
	LiveStatusPath    = "/run/katl/apps/bgp-api-vip/status.json"
	OperationStatus   = "/var/lib/katl/operations/<operation-id>/apps/bgp-api-vip/status.json"
	AppDropInPath     = "/etc/systemd/system/katl-app-bgp-api-vip.service.d/10-katl-config.conf"
	BirdDropInPath    = "/etc/systemd/system/katl-app-bird.service.d/20-katl-bgp-api-vip.conf"
	KubeletDropInPath = "/etc/systemd/system/kubelet.service.d/20-katl-control-plane-endpoint.conf"
	NetworkPath       = "/etc/systemd/network/05-katl-bgp-api-vip.network"
	DummyNetDevPath   = "/etc/systemd/network/05-katl-bgp-api-vip.netdev"
	defaultHealthPath = "/readyz"
)

var (
	safeLabelRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	interfaceNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,14}$`)
	communityRE     = regexp.MustCompile(`^[0-9]{1,10}:[0-9]{1,10}$`)
)

type Object struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`
	Kind       string `yaml:"kind" json:"kind"`
	Spec       Config `yaml:"spec" json:"spec"`
}

type Config struct {
	Endpoint      Endpoint        `yaml:"endpoint" json:"endpoint"`
	VIPInterface  VIPInterface    `yaml:"vipInterface" json:"vipInterface"`
	Routing       Routing         `yaml:"routing" json:"routing"`
	AdvertiseOn   AdvertiseOn     `yaml:"advertiseOn" json:"advertiseOn"`
	FabricPeers   []Peer          `yaml:"fabricPeers,omitempty" json:"fabricPeers,omitempty"`
	DevHostPeers  []Peer          `yaml:"devHostPeers,omitempty" json:"devHostPeers,omitempty"`
	RouteExchange []RouteExchange `yaml:"routeExchange,omitempty" json:"routeExchange,omitempty"`
	Advertisement Advertisement   `yaml:"advertisement" json:"advertisement"`
	Health        Health          `yaml:"health" json:"health"`
	Status        StatusConfig    `yaml:"status,omitempty" json:"status,omitempty"`
}

type RouteExchange struct {
	Name           string                                `yaml:"name" json:"name"`
	ListenPort     int                                   `yaml:"listenPort" json:"listenPort"`
	PeerASN        uint32                                `yaml:"peerASN" json:"peerASN"`
	ExportToFabric []controlplaneendpoint.PrefixEnvelope `yaml:"exportToFabric,omitempty" json:"exportToFabric,omitempty"`
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

type Routing struct {
	Daemon           string       `yaml:"daemon,omitempty" json:"daemon,omitempty"`
	ProtocolBoundary string       `yaml:"protocolBoundary,omitempty" json:"protocolBoundary,omitempty"`
	RouterID         string       `yaml:"routerID" json:"routerID"`
	LocalASN         uint32       `yaml:"localASN" json:"localASN"`
	SourceAddress    string       `yaml:"sourceAddress,omitempty" json:"sourceAddress,omitempty"`
	SourceInterface  string       `yaml:"sourceInterface,omitempty" json:"sourceInterface,omitempty"`
	ExportPolicy     ExportPolicy `yaml:"exportPolicy,omitempty" json:"exportPolicy,omitempty"`
}

type ExportPolicy struct {
	Communities     []string `yaml:"communities,omitempty" json:"communities,omitempty"`
	LocalPreference int      `yaml:"localPreference,omitempty" json:"localPreference,omitempty"`
	MED             int      `yaml:"med,omitempty" json:"med,omitempty"`
}

type AdvertiseOn struct {
	Roles []string `yaml:"roles,omitempty" json:"roles,omitempty"`
}

type Peer struct {
	Name                  string   `yaml:"name" json:"name"`
	Address               string   `yaml:"address" json:"address"`
	ASN                   uint32   `yaml:"asn" json:"asn"`
	LocalASN              uint32   `yaml:"localASN,omitempty" json:"localASN,omitempty"`
	Kind                  string   `yaml:"kind,omitempty" json:"kind,omitempty"`
	AuthRef               string   `yaml:"authRef,omitempty" json:"authRef,omitempty"`
	HoldTime              string   `yaml:"holdTime,omitempty" json:"holdTime,omitempty"`
	KeepaliveTime         string   `yaml:"keepaliveTime,omitempty" json:"keepaliveTime,omitempty"`
	AllowedExportPrefixes []string `yaml:"allowedExportPrefixes,omitempty" json:"allowedExportPrefixes,omitempty"`
	SourceAddress         string   `yaml:"sourceAddress,omitempty" json:"sourceAddress,omitempty"`
	SourceInterface       string   `yaml:"sourceInterface,omitempty" json:"sourceInterface,omitempty"`
	Communities           []string `yaml:"communities,omitempty" json:"communities,omitempty"`
	LocalPreference       int      `yaml:"localPreference,omitempty" json:"localPreference,omitempty"`
	MED                   int      `yaml:"med,omitempty" json:"med,omitempty"`
}

type Advertisement struct {
	Enabled               *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	StartWithdrawn        *bool `yaml:"startWithdrawn,omitempty" json:"startWithdrawn,omitempty"`
	AdvertiseAfterHealthy *bool `yaml:"advertiseAfterHealthy,omitempty" json:"advertiseAfterHealthy,omitempty"`
	WithdrawOnFailure     *bool `yaml:"withdrawOnFailure,omitempty" json:"withdrawOnFailure,omitempty"`
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
	Config                  Config
	NodeRole                string
	LocalInterfaceAddresses map[string][]string
}

type Plan struct {
	Config Config
	Files  []confext.NativeEtcFile
}

// FromControlPlaneEndpoint lowers normalized cluster intent into the complete,
// Katl-owned endpoint-advertiser configuration. BIRD, interface and health
// details deliberately remain absent from ClusterConfig.
func FromControlPlaneEndpoint(plan controlplaneendpoint.Plan) (Config, error) {
	if plan.Config.Advertisement == nil || plan.Config.Advertisement.BGP == nil {
		return Config{}, fmt.Errorf("managed control-plane endpoint BGP intent is required")
	}
	bgp := plan.Config.Advertisement.BGP
	config := Config{
		Endpoint: Endpoint{
			Host:          plan.Config.Host,
			Port:          plan.Config.Port,
			VIP:           plan.VIPPrefix,
			AddressFamily: "ipv4",
			TLSServerName: plan.Config.Host,
			Provenance:    "platform-host",
		},
		VIPInterface: VIPInterface{Kind: "dummy", Name: "katl-api"},
		Routing:      Routing{LocalASN: bgp.LocalASN},
		AdvertiseOn:  AdvertiseOn{Roles: []string{"control-plane"}},
		Advertisement: Advertisement{
			Enabled:               boolPtr(true),
			StartWithdrawn:        boolPtr(true),
			AdvertiseAfterHealthy: boolPtr(true),
			WithdrawOnFailure:     boolPtr(true),
		},
	}
	for index, peer := range bgp.Peers {
		config.FabricPeers = append(config.FabricPeers, Peer{
			Name:                  fmt.Sprintf("fabric-%d", index+1),
			Address:               peer.Address,
			ASN:                   peer.ASN,
			AllowedExportPrefixes: []string{plan.VIPPrefix},
		})
	}
	for _, exchange := range bgp.RouteExchange {
		config.RouteExchange = append(config.RouteExchange, RouteExchange{
			Name:           exchange.Name,
			ListenPort:     exchange.ListenPort,
			PeerASN:        exchange.PeerASN,
			ExportToFabric: append([]controlplaneendpoint.PrefixEnvelope(nil), exchange.ExportToFabric...),
		})
	}
	return Normalize(config)
}

func (p Plan) NativeEtcFiles() []confext.NativeEtcFile {
	files := make([]confext.NativeEtcFile, len(p.Files))
	copy(files, p.Files)
	return files
}

func Decode(reader io.Reader) (Object, error) {
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	var object Object
	if err := decoder.Decode(&object); err != nil {
		return Object{}, fmt.Errorf("decode BGPAPIEndpoint: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Object{}, fmt.Errorf("decode BGPAPIEndpoint: multiple YAML documents")
		}
		return Object{}, fmt.Errorf("decode BGPAPIEndpoint: %w", err)
	}
	if object.APIVersion != APIVersion {
		return Object{}, fmt.Errorf("BGPAPIEndpoint apiVersion must be %s", APIVersion)
	}
	if object.Kind != Kind {
		return Object{}, fmt.Errorf("BGPAPIEndpoint kind must be %s", Kind)
	}
	return object, nil
}

func Normalize(config Config) (Config, error) {
	normalized := config
	normalized.Endpoint.Host = strings.TrimSpace(normalized.Endpoint.Host)
	normalized.Endpoint.VIP = strings.TrimSpace(normalized.Endpoint.VIP)
	normalized.Endpoint.AddressFamily = strings.TrimSpace(normalized.Endpoint.AddressFamily)
	normalized.Endpoint.TLSServerName = strings.TrimSpace(normalized.Endpoint.TLSServerName)
	normalized.Endpoint.Provenance = strings.TrimSpace(normalized.Endpoint.Provenance)
	if normalized.Endpoint.Port == 0 {
		normalized.Endpoint.Port = 6443
	}
	if normalized.Endpoint.TLSServerName == "" {
		normalized.Endpoint.TLSServerName = normalized.Endpoint.Host
	}
	if normalized.Endpoint.Provenance == "" {
		normalized.Endpoint.Provenance = "platform-host"
	}
	vip, err := validateVIP(normalized.Endpoint)
	if err != nil {
		return Config{}, err
	}
	if normalized.Endpoint.AddressFamily == "" {
		if vip.Addr().Is4() {
			normalized.Endpoint.AddressFamily = "ipv4"
		} else {
			normalized.Endpoint.AddressFamily = "ipv6"
		}
	}
	if err := validateEndpoint(normalized.Endpoint, vip); err != nil {
		return Config{}, err
	}
	if err := normalizeInterface(&normalized.VIPInterface); err != nil {
		return Config{}, err
	}
	if err := normalizeRouting(&normalized.Routing, vip); err != nil {
		return Config{}, err
	}
	if len(normalized.AdvertiseOn.Roles) == 0 {
		normalized.AdvertiseOn.Roles = []string{"control-plane"}
	}
	if err := validateAdvertiseOn(normalized.AdvertiseOn); err != nil {
		return Config{}, err
	}
	advertisement := normalized.Advertisement
	if advertisement.Enabled == nil {
		advertisement.Enabled = boolPtr(true)
	}
	if advertisement.StartWithdrawn == nil {
		advertisement.StartWithdrawn = boolPtr(true)
	}
	if advertisement.AdvertiseAfterHealthy == nil {
		advertisement.AdvertiseAfterHealthy = boolPtr(true)
	}
	if advertisement.WithdrawOnFailure == nil {
		advertisement.WithdrawOnFailure = boolPtr(true)
	}
	if !*advertisement.StartWithdrawn {
		return Config{}, fmt.Errorf("advertisement.startWithdrawn must be true")
	}
	if !*advertisement.AdvertiseAfterHealthy {
		return Config{}, fmt.Errorf("advertisement.advertiseAfterHealthy must be true")
	}
	if !*advertisement.WithdrawOnFailure {
		return Config{}, fmt.Errorf("advertisement.withdrawOnFailure must be true")
	}
	normalized.Advertisement = advertisement

	health, err := normalizeHealth(normalized.Health, normalized.Endpoint, vip)
	if err != nil {
		return Config{}, err
	}
	normalized.Health = health
	normalized.Status.LiveStatusPath = LiveStatusPath
	normalized.Status.OperationStatusPath = OperationStatus

	peers, err := normalizePeerGroup("fabricPeers", "fabric", normalized.FabricPeers, normalized.Routing, vip)
	if err != nil {
		return Config{}, err
	}
	normalized.FabricPeers = peers
	peers, err = normalizePeerGroup("devHostPeers", "dev-host", normalized.DevHostPeers, normalized.Routing, vip)
	if err != nil {
		return Config{}, err
	}
	normalized.DevHostPeers = peers
	if len(normalized.FabricPeers)+len(normalized.DevHostPeers) == 0 {
		return Config{}, fmt.Errorf("at least one fabricPeers or devHostPeers entry is required")
	}
	return normalized, nil
}

func RenderNativeEtcFiles(request RenderRequest) (Plan, error) {
	if strings.TrimSpace(request.NodeRole) != "control-plane" {
		return Plan{}, fmt.Errorf("node role %q cannot advertise BGP API VIP; v0.1 supports control-plane only", request.NodeRole)
	}
	config, err := Normalize(request.Config)
	if err != nil {
		return Plan{}, err
	}
	if err := validateSourceBindings(config, request.LocalInterfaceAddresses); err != nil {
		return Plan{}, err
	}
	files := []confext.NativeEtcFile{
		{
			Path:    NetworkPath,
			Content: renderNetwork(config),
			Mode:    0o644,
		},
		{
			Path:    ConfigPath,
			Content: renderAppConfig(config),
			Mode:    0o644,
		},
		{
			Path:    BirdConfigPath,
			Content: renderBirdConfig(config),
			Mode:    0o644,
		},
		{
			Path:    AppDropInPath,
			Content: renderAppDropIn(),
			Mode:    0o644,
		},
		{
			Path:    BirdDropInPath,
			Content: renderBirdDropIn(),
			Mode:    0o644,
		},
		{
			Path:    KubeletDropInPath,
			Content: renderKubeletDropIn(),
			Mode:    0o644,
		},
	}
	if config.VIPInterface.Kind == "dummy" {
		files = append(files, confext.NativeEtcFile{
			Path:    DummyNetDevPath,
			Content: renderDummyNetDev(config.VIPInterface),
			Mode:    0o644,
		})
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
		normalizedFiles = append(normalizedFiles, confext.NativeEtcFile{
			Path:    plan.Path,
			Content: file.Content,
			Mode:    plan.Mode,
			UID:     0,
			GID:     0,
		})
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
	addr := prefix.Addr()
	if addr.Is4() {
		if prefix.Bits() != 32 {
			return netip.Prefix{}, fmt.Errorf("endpoint.vip must be a /32 or /128")
		}
	} else if addr.Is6() {
		if prefix.Bits() != 128 {
			return netip.Prefix{}, fmt.Errorf("endpoint.vip must be a /32 or /128")
		}
	} else {
		return netip.Prefix{}, fmt.Errorf("endpoint.vip address family is unsupported")
	}
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() {
		return netip.Prefix{}, fmt.Errorf("endpoint.vip %q is not a usable host API VIP", endpoint.VIP)
	}
	return prefix, nil
}

func validateEndpoint(endpoint Endpoint, vip netip.Prefix) error {
	if endpoint.Port < 1 || endpoint.Port > 65535 {
		return fmt.Errorf("endpoint.port must be between 1 and 65535")
	}
	if endpoint.Provenance != "platform-host" {
		return fmt.Errorf("endpoint.provenance must be platform-host for bgp-api-vip, got %q", endpoint.Provenance)
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
	switch vipInterface.Kind {
	case "dummy", "loopback":
	default:
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

func normalizeRouting(routing *Routing, vip netip.Prefix) error {
	routing.Daemon = strings.TrimSpace(routing.Daemon)
	if routing.Daemon == "" {
		routing.Daemon = "bird"
	}
	if routing.Daemon != "bird" {
		return fmt.Errorf("routing.daemon must be bird")
	}
	routing.ProtocolBoundary = strings.TrimSpace(routing.ProtocolBoundary)
	if routing.ProtocolBoundary == "" {
		routing.ProtocolBoundary = "bgp"
	}
	if routing.ProtocolBoundary != "bgp" {
		return fmt.Errorf("routing.protocolBoundary must be bgp")
	}
	routing.RouterID = strings.TrimSpace(routing.RouterID)
	if routing.RouterID != "" {
		routerID, err := netip.ParseAddr(routing.RouterID)
		if err != nil || !routerID.Is4() || routerID.IsUnspecified() || routerID.IsMulticast() {
			return fmt.Errorf("routing.routerID must be an IPv4 address")
		}
		routing.RouterID = routerID.String()
	}
	if err := validateASN("routing.localASN", routing.LocalASN); err != nil {
		return err
	}
	if routing.SourceAddress != "" {
		source, err := validateSourceAddress("routing.sourceAddress", routing.SourceAddress, vip)
		if err != nil {
			return err
		}
		routing.SourceAddress = source.String()
	}
	if routing.SourceInterface != "" {
		if err := validateInterfaceRef("routing.sourceInterface", routing.SourceInterface); err != nil {
			return err
		}
	}
	if err := validatePolicy("routing.exportPolicy", routing.ExportPolicy); err != nil {
		return err
	}
	return nil
}

func validateAdvertiseOn(advertiseOn AdvertiseOn) error {
	for i, role := range advertiseOn.Roles {
		if strings.TrimSpace(role) != "control-plane" {
			return fmt.Errorf("advertiseOn.roles[%d] must be control-plane", i)
		}
	}
	return nil
}

func normalizeHealth(health Health, endpoint Endpoint, vip netip.Prefix) (Health, error) {
	health.Probe = defaultString(health.Probe, "readyz")
	health.Scheme = defaultString(health.Scheme, "https")
	health.Host = defaultString(health.Host, vip.Addr().String())
	health.Path = defaultString(health.Path, defaultHealthPath)
	health.Interval = defaultString(health.Interval, "2s")
	health.Timeout = defaultString(health.Timeout, "1s")
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
	if health.Probe != "readyz" {
		return Health{}, fmt.Errorf("health.probe must be readyz")
	}
	if health.Scheme != "https" {
		return Health{}, fmt.Errorf("health.scheme must be https")
	}
	if health.Host != vip.Addr().String() {
		return Health{}, fmt.Errorf("health.host must be endpoint.vip address")
	}
	if health.Path != defaultHealthPath {
		return Health{}, fmt.Errorf("health.path must be /readyz")
	}
	if health.Port != endpoint.Port {
		return Health{}, fmt.Errorf("health.port must match endpoint.port")
	}
	if health.SuccessThreshold < 1 || health.FailureThreshold < 1 {
		return Health{}, fmt.Errorf("health thresholds must be positive")
	}
	if err := validateCARef(health.CARef); err != nil {
		return Health{}, err
	}
	return health, nil
}

func normalizePeerGroup(field, defaultKind string, peers []Peer, routing Routing, vip netip.Prefix) ([]Peer, error) {
	out := make([]Peer, 0, len(peers))
	seen := map[string]struct{}{}
	for i, peer := range peers {
		path := fmt.Sprintf("%s[%d]", field, i)
		peer.Name = strings.TrimSpace(peer.Name)
		if !safeLabelRE.MatchString(peer.Name) || len(peer.Name) > 63 {
			return nil, fmt.Errorf("%s.name %q is not a safe peer name", path, peer.Name)
		}
		if _, ok := seen[peer.Name]; ok {
			return nil, fmt.Errorf("%s.name %q duplicates another peer", path, peer.Name)
		}
		seen[peer.Name] = struct{}{}
		peer.Kind = defaultString(peer.Kind, defaultKind)
		if peer.Kind != defaultKind {
			return nil, fmt.Errorf("%s.kind must be %s", path, defaultKind)
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(peer.Address))
		if err != nil || addr.IsUnspecified() || addr.IsMulticast() {
			return nil, fmt.Errorf("%s.address must be an IP address", path)
		}
		peer.Address = addr.String()
		if err := validateASN(path+".asn", peer.ASN); err != nil {
			return nil, err
		}
		if peer.LocalASN == 0 {
			peer.LocalASN = routing.LocalASN
		}
		if err := validateASN(path+".localASN", peer.LocalASN); err != nil {
			return nil, err
		}
		if len(peer.AllowedExportPrefixes) == 0 {
			return nil, fmt.Errorf("%s.allowedExportPrefixes must include endpoint.vip", path)
		}
		for j, allowed := range peer.AllowedExportPrefixes {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(allowed))
			if err != nil || prefix.Masked() != vip {
				return nil, fmt.Errorf("%s.allowedExportPrefixes[%d] must be endpoint.vip", path, j)
			}
			peer.AllowedExportPrefixes[j] = vip.String()
		}
		if peer.SourceAddress == "" {
			peer.SourceAddress = routing.SourceAddress
		}
		if peer.SourceAddress != "" {
			source, err := validateSourceAddress(path+".sourceAddress", peer.SourceAddress, vip)
			if err != nil {
				return nil, err
			}
			peer.SourceAddress = source.String()
		}
		if peer.SourceInterface == "" {
			peer.SourceInterface = routing.SourceInterface
		}
		if peer.SourceInterface != "" {
			if err := validateInterfaceRef(path+".sourceInterface", peer.SourceInterface); err != nil {
				return nil, err
			}
		}
		if peer.AuthRef != "" {
			if err := validateSecretRef(path+".authRef", peer.AuthRef); err != nil {
				return nil, err
			}
		}
		if err := validatePolicy(path, ExportPolicy{Communities: peer.Communities, LocalPreference: peer.LocalPreference, MED: peer.MED}); err != nil {
			return nil, err
		}
		out = append(out, peer)
	}
	return out, nil
}

func validateSourceBindings(config Config, addressesByInterface map[string][]string) error {
	if len(addressesByInterface) == 0 {
		return nil
	}
	for _, peer := range allPeers(config) {
		if peer.SourceInterface == "" || peer.SourceAddress == "" {
			continue
		}
		addresses, ok := addressesByInterface[peer.SourceInterface]
		if !ok {
			return fmt.Errorf("peer %q sourceInterface %q is not present in host inventory", peer.Name, peer.SourceInterface)
		}
		for _, value := range addresses {
			if strings.TrimSpace(value) == peer.SourceAddress {
				goto nextPeer
			}
			prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
			if err == nil && prefix.Addr().String() == peer.SourceAddress {
				goto nextPeer
			}
		}
		return fmt.Errorf("peer %q sourceAddress %q is not assigned to sourceInterface %q", peer.Name, peer.SourceAddress, peer.SourceInterface)
	nextPeer:
	}
	return nil
}

func validateASN(field string, value uint32) error {
	if value == 0 {
		return fmt.Errorf("%s must be a non-zero ASN", field)
	}
	return nil
}

func validateSourceAddress(field, value string, vip netip.Prefix) (netip.Addr, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() {
		return netip.Addr{}, fmt.Errorf("%s must be a local non-VIP IP address", field)
	}
	if addr == vip.Addr() {
		return netip.Addr{}, fmt.Errorf("%s must not be endpoint.vip", field)
	}
	return addr, nil
}

func validateInterfaceRef(field, value string) error {
	if !interfaceNameRE.MatchString(strings.TrimSpace(value)) || strings.Contains(value, "/") {
		return fmt.Errorf("%s %q is not a safe interface name", field, value)
	}
	return nil
}

func validatePolicy(field string, policy ExportPolicy) error {
	for i, community := range policy.Communities {
		if !communityRE.MatchString(strings.TrimSpace(community)) {
			return fmt.Errorf("%s.communities[%d] must be a standard BGP community", field, i)
		}
	}
	if policy.LocalPreference < 0 {
		return fmt.Errorf("%s.localPreference must be non-negative", field)
	}
	if policy.MED < 0 {
		return fmt.Errorf("%s.med must be non-negative", field)
	}
	return nil
}

func validateSecretRef(field, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] != "secret" || !safeLabelRE.MatchString(parts[1]) {
		return fmt.Errorf("%s must be secret/<name>", field)
	}
	return nil
}

func validateCARef(value string) error {
	value = strings.TrimSpace(value)
	if value == "kube-apiserver-ca" {
		return nil
	}
	if err := validateSecretRef("health.caRef", value); err != nil {
		return fmt.Errorf("health.caRef must be kube-apiserver-ca or secret/<name>")
	}
	return nil
}

func renderAppConfig(config Config) string {
	object := Object{
		APIVersion: APIVersion,
		Kind:       Kind,
		Spec:       config,
	}
	data, err := yaml.Marshal(object)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func renderDummyNetDev(vipInterface VIPInterface) string {
	var b strings.Builder
	b.WriteString("[NetDev]\n")
	b.WriteString("Name=" + vipInterface.Name + "\n")
	b.WriteString("Kind=dummy\n")
	return b.String()
}

func renderNetwork(config Config) string {
	var b strings.Builder
	b.WriteString("[Match]\n")
	b.WriteString("Name=" + config.VIPInterface.Name + "\n\n")
	b.WriteString("[Network]\n")
	b.WriteString("Address=" + config.Endpoint.VIP + "\n")
	if config.VIPInterface.MTU != 0 {
		b.WriteString("MTUBytes=" + strconv.Itoa(config.VIPInterface.MTU) + "\n")
	}
	return b.String()
}

func renderBirdConfig(config Config) string {
	family := "ipv6"
	if strings.EqualFold(config.Endpoint.AddressFamily, "ipv4") {
		family = "ipv4"
	}
	var b strings.Builder
	b.WriteString("# Generated by Katl bgp-api-vip. Do not edit.\n")
	if config.Routing.RouterID == "" {
		b.WriteString("router id from \"*\";\n\n")
	} else {
		b.WriteString("router id " + config.Routing.RouterID + ";\n\n")
	}
	b.WriteString(family + " table katl_fabric;\n")
	for _, exchange := range config.RouteExchange {
		b.WriteString(family + " table katl_exchange_" + safeSymbol(exchange.Name) + "_table;\n")
	}
	b.WriteByte('\n')
	b.WriteString("protocol device katl_device {}\n\n")
	b.WriteString("protocol static katl_api {\n")
	b.WriteString("  disabled;\n")
	b.WriteString("  " + family + " { table katl_fabric; };\n")
	b.WriteString("  route " + config.Endpoint.VIP + " blackhole;\n")
	b.WriteString("}\n\n")
	for _, exchange := range config.RouteExchange {
		b.WriteString(renderRouteExchange(config, exchange))
		b.WriteByte('\n')
	}
	for _, peer := range config.FabricPeers {
		b.WriteString(renderPeerFilter(config, peer))
		b.WriteByte('\n')
		b.WriteString(renderBGPPeer(config, family, peer))
		b.WriteByte('\n')
	}
	return b.String()
}

func renderPeerFilter(config Config, peer Peer) string {
	name := protocolName(peer)
	filterName := "katl_export_" + name
	var b strings.Builder
	b.WriteString("filter " + filterName + " {\n")
	b.WriteString("  if source = RTS_STATIC && net = " + config.Endpoint.VIP + " then accept;\n")
	if len(config.RouteExchange) > 0 {
		b.WriteString("  if source = RTS_BGP then accept;\n")
	}
	b.WriteString("  reject;\n")
	b.WriteString("}\n")
	return b.String()
}

func renderBGPPeer(config Config, family string, peer Peer) string {
	name := protocolName(peer)
	var b strings.Builder
	b.WriteString("protocol bgp " + name + " {\n")
	b.WriteString("  local as " + strconv.FormatUint(uint64(peer.LocalASN), 10) + ";\n")
	b.WriteString("  neighbor " + peer.Address + " as " + strconv.FormatUint(uint64(peer.ASN), 10) + ";\n")
	if peer.SourceAddress != "" {
		b.WriteString("  source address " + peer.SourceAddress + ";\n")
	}
	if peer.AuthRef != "" {
		b.WriteString("  # authRef configured and redacted by Katl\n")
	}
	b.WriteString("  " + family + " {\n")
	b.WriteString("    table katl_fabric;\n")
	b.WriteString("    import none;\n")
	b.WriteString("    export filter katl_export_" + name + ";\n")
	b.WriteString("  };\n")
	b.WriteString("}\n")
	return b.String()
}

func renderRouteExchange(config Config, exchange RouteExchange) string {
	name := safeSymbol(exchange.Name)
	var b strings.Builder
	b.WriteString("filter katl_exchange_" + name + "_export {\n")
	for _, envelope := range exchange.ExportToFabric {
		b.WriteString("  if net ~ [ " + birdPrefixPattern(envelope) + " ] then accept;\n")
	}
	b.WriteString("  reject;\n}\n\n")
	b.WriteString("protocol bgp katl_exchange_" + name + " {\n")
	b.WriteString("  passive on;\n")
	b.WriteString("  local 127.0.0.1 port " + strconv.Itoa(exchange.ListenPort) + " as " + strconv.FormatUint(uint64(config.Routing.LocalASN), 10) + ";\n")
	b.WriteString("  neighbor 127.0.0.1 as " + strconv.FormatUint(uint64(exchange.PeerASN), 10) + ";\n")
	b.WriteString("  ipv4 {\n")
	b.WriteString("    table katl_exchange_" + name + "_table;\n")
	b.WriteString("    import all;\n")
	b.WriteString("    export none;\n")
	b.WriteString("  };\n")
	b.WriteString("}\n\n")
	b.WriteString("protocol pipe katl_exchange_" + name + "_to_fabric {\n")
	b.WriteString("  table katl_exchange_" + name + "_table;\n")
	b.WriteString("  peer table katl_fabric;\n")
	b.WriteString("  export filter katl_exchange_" + name + "_export;\n")
	b.WriteString("  import none;\n")
	b.WriteString("}\n")
	return b.String()
}

func birdPrefixPattern(envelope controlplaneendpoint.PrefixEnvelope) string {
	if envelope.PrefixLength == nil {
		return envelope.CIDR + "+"
	}
	return envelope.CIDR + "{" + strconv.Itoa(*envelope.PrefixLength) + "," + strconv.Itoa(*envelope.PrefixLength) + "}"
}

func safeSymbol(value string) string {
	return strings.ReplaceAll(value, "-", "_")
}

func renderAppDropIn() string {
	return "[Service]\n" +
		"Environment=KATL_BGP_API_VIP_CONFIG=" + ConfigPath + "\n" +
		"Environment=KATL_BGP_API_VIP_STATUS=" + LiveStatusPath + "\n"
}

func renderBirdDropIn() string {
	return "[Unit]\n" +
		"After=network-online.target\n\n" +
		"[Service]\n" +
		"Environment=KATL_BIRD_CONFIG=" + BirdConfigPath + "\n"
}

func renderKubeletDropIn() string {
	return "[Unit]\n" +
		"Wants=katl-app-bgp-api-vip.service\n" +
		"After=katl-app-bgp-api-vip.service\n"
}

func allPeers(config Config) []Peer {
	peers := make([]Peer, 0, len(config.FabricPeers)+len(config.DevHostPeers))
	peers = append(peers, config.FabricPeers...)
	peers = append(peers, config.DevHostPeers...)
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Kind != peers[j].Kind {
			return peers[i].Kind < peers[j].Kind
		}
		return peers[i].Name < peers[j].Name
	})
	return peers
}

func protocolName(peer Peer) string {
	replacer := strings.NewReplacer("-", "_", ".", "_", ":", "_")
	return "katl_" + replacer.Replace(peer.Kind) + "_" + replacer.Replace(peer.Name)
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
