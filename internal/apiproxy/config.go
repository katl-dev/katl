package apiproxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	APIVersion         = "katl.dev/v1alpha1"
	Kind               = "APIProxyConfig"
	DefaultPort        = 7445
	DefaultBackendPort = 6443
	DefaultCAFile      = "/etc/kubernetes/pki/ca.crt"
	DefaultInterval    = 2 * time.Second
	DefaultTimeout     = 2 * time.Second
	ConfigPath         = "/etc/katl/api-proxy/config.json"
	StatusPath         = "/run/katl/api-proxy/status.json"
)

const (
	ExposureNodeLocal   = "node-local"
	ExposureWorkstation = "workstation"
)

type Config struct {
	APIVersion        string        `json:"apiVersion" yaml:"apiVersion"`
	Kind              string        `json:"kind" yaml:"kind"`
	TLSName           string        `json:"tlsServerName" yaml:"tlsServerName"`
	CanonicalEndpoint string        `json:"canonicalEndpoint" yaml:"canonicalEndpoint"`
	CAFile            string        `json:"caFile" yaml:"caFile"`
	CheckInterval     time.Duration `json:"-"`
	CheckTimeout      time.Duration `json:"-"`
	Listeners         []Listener    `json:"listeners" yaml:"listeners"`
	Backends          []Backend     `json:"backends" yaml:"backends"`
}

func (c Config) IsZero() bool {
	return c.APIVersion == "" && c.Kind == "" && c.TLSName == "" && c.CanonicalEndpoint == "" && c.CAFile == "" &&
		c.CheckInterval == 0 && c.CheckTimeout == 0 && len(c.Listeners) == 0 && len(c.Backends) == 0
}

type Listener struct {
	Address  string `json:"address" yaml:"address"`
	Exposure string `json:"exposure" yaml:"exposure"`
}

type Backend struct {
	Name    string `json:"name" yaml:"name"`
	Address string `json:"address" yaml:"address"`
	Local   bool   `json:"local,omitempty" yaml:"local,omitempty"`
}

type configJSON struct {
	APIVersion        string     `json:"apiVersion"`
	Kind              string     `json:"kind"`
	TLSName           string     `json:"tlsServerName"`
	CanonicalEndpoint string     `json:"canonicalEndpoint"`
	CAFile            string     `json:"caFile"`
	CheckInterval     string     `json:"checkInterval"`
	CheckTimeout      string     `json:"checkTimeout"`
	Listeners         []Listener `json:"listeners"`
	Backends          []Backend  `json:"backends"`
}

func (c Config) MarshalJSON() ([]byte, error) {
	c, err := Normalize(c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(configJSON{
		APIVersion:        c.APIVersion,
		Kind:              c.Kind,
		TLSName:           c.TLSName,
		CanonicalEndpoint: c.CanonicalEndpoint,
		CAFile:            c.CAFile,
		CheckInterval:     c.CheckInterval.String(),
		CheckTimeout:      c.CheckTimeout.String(),
		Listeners:         c.Listeners,
		Backends:          c.Backends,
	})
}

func (c *Config) UnmarshalJSON(data []byte) error {
	var encoded configJSON
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}
	var interval time.Duration
	if encoded.CheckInterval != "" {
		var err error
		interval, err = time.ParseDuration(encoded.CheckInterval)
		if err != nil {
			return fmt.Errorf("checkInterval: %w", err)
		}
	}
	var timeout time.Duration
	if encoded.CheckTimeout != "" {
		var err error
		timeout, err = time.ParseDuration(encoded.CheckTimeout)
		if err != nil {
			return fmt.Errorf("checkTimeout: %w", err)
		}
	}
	*c = Config{
		APIVersion: encoded.APIVersion, Kind: encoded.Kind,
		TLSName: encoded.TLSName, CanonicalEndpoint: encoded.CanonicalEndpoint, CAFile: encoded.CAFile,
		CheckInterval: interval, CheckTimeout: timeout,
		Listeners: encoded.Listeners, Backends: encoded.Backends,
	}
	return nil
}

func Normalize(input Config) (Config, error) {
	config := input
	if config.APIVersion == "" {
		config.APIVersion = APIVersion
	}
	if config.APIVersion != APIVersion {
		return Config{}, fmt.Errorf("apiVersion must be %s", APIVersion)
	}
	if config.Kind == "" {
		config.Kind = Kind
	}
	if config.Kind != Kind {
		return Config{}, fmt.Errorf("kind must be %s", Kind)
	}
	config.TLSName = strings.TrimSpace(config.TLSName)
	if config.TLSName == "" || strings.ContainsAny(config.TLSName, "/\\:") {
		return Config{}, fmt.Errorf("tlsServerName must be a DNS name or IP address")
	}
	config.CanonicalEndpoint = strings.TrimSpace(config.CanonicalEndpoint)
	if config.CanonicalEndpoint == "" {
		config.CanonicalEndpoint = net.JoinHostPort(config.TLSName, strconv.Itoa(DefaultBackendPort))
	}
	canonicalHost, canonicalPort, err := net.SplitHostPort(config.CanonicalEndpoint)
	if err != nil || strings.TrimSpace(canonicalHost) == "" || canonicalPort != strconv.Itoa(DefaultBackendPort) {
		return Config{}, fmt.Errorf("canonicalEndpoint must be a host and port %d", DefaultBackendPort)
	}
	config.CAFile = strings.TrimSpace(config.CAFile)
	if config.CAFile == "" {
		config.CAFile = DefaultCAFile
	}
	if !filepath.IsAbs(config.CAFile) || filepath.Clean(config.CAFile) != config.CAFile {
		return Config{}, fmt.Errorf("caFile must be a clean absolute path")
	}
	if config.CheckInterval == 0 {
		config.CheckInterval = DefaultInterval
	}
	if config.CheckInterval < 250*time.Millisecond || config.CheckInterval > time.Minute {
		return Config{}, fmt.Errorf("checkInterval must be between 250ms and 1m")
	}
	if config.CheckTimeout == 0 {
		config.CheckTimeout = DefaultTimeout
	}
	if config.CheckTimeout < 250*time.Millisecond || config.CheckTimeout > time.Minute {
		return Config{}, fmt.Errorf("checkTimeout must be between 250ms and 1m")
	}

	listeners := slices.Clone(config.Listeners)
	seenListeners := map[string]struct{}{}
	local := false
	for i := range listeners {
		listeners[i].Address = strings.TrimSpace(listeners[i].Address)
		listeners[i].Exposure = strings.TrimSpace(listeners[i].Exposure)
		addr, err := literalEndpoint(listeners[i].Address, DefaultPort)
		if err != nil {
			return Config{}, fmt.Errorf("listeners[%d].address: %w", i, err)
		}
		listeners[i].Address = addr
		host, _, _ := net.SplitHostPort(addr)
		switch listeners[i].Exposure {
		case ExposureNodeLocal:
			if host != "127.0.0.1" {
				return Config{}, fmt.Errorf("listeners[%d] node-local address must be 127.0.0.1", i)
			}
			local = true
		case ExposureWorkstation:
			parsed := netip.MustParseAddr(host)
			if parsed.IsLoopback() || parsed.IsUnspecified() {
				return Config{}, fmt.Errorf("listeners[%d] workstation address must be a declared non-loopback address", i)
			}
		default:
			return Config{}, fmt.Errorf("listeners[%d].exposure must be %q or %q", i, ExposureNodeLocal, ExposureWorkstation)
		}
		if _, exists := seenListeners[addr]; exists {
			return Config{}, fmt.Errorf("listeners[%d].address %q is duplicated", i, addr)
		}
		seenListeners[addr] = struct{}{}
	}
	if !local {
		return Config{}, fmt.Errorf("listeners must include 127.0.0.1:%d node-local access", DefaultPort)
	}
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].Address < listeners[j].Address })

	backends := slices.Clone(config.Backends)
	if len(backends) == 0 {
		return Config{}, fmt.Errorf("backends must not be empty")
	}
	seenNames := map[string]struct{}{}
	seenBackends := map[string]struct{}{}
	localBackends := 0
	for i := range backends {
		backends[i].Name = strings.TrimSpace(backends[i].Name)
		if backends[i].Name == "" {
			return Config{}, fmt.Errorf("backends[%d].name is required", i)
		}
		if _, exists := seenNames[backends[i].Name]; exists {
			return Config{}, fmt.Errorf("backends[%d].name %q is duplicated", i, backends[i].Name)
		}
		seenNames[backends[i].Name] = struct{}{}
		addr, err := literalEndpoint(backends[i].Address, DefaultBackendPort)
		if err != nil {
			return Config{}, fmt.Errorf("backends[%d].address: %w", i, err)
		}
		backends[i].Address = addr
		if _, exists := seenBackends[addr]; exists {
			return Config{}, fmt.Errorf("backends[%d].address %q is duplicated", i, addr)
		}
		seenBackends[addr] = struct{}{}
		if backends[i].Local {
			localBackends++
		}
	}
	if localBackends > 1 {
		return Config{}, fmt.Errorf("backends may contain at most one local API server")
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i].Name < backends[j].Name })
	config.Listeners = listeners
	config.Backends = backends
	return config, nil
}

func Render(config Config) (string, error) {
	normalized, err := Normalize(config)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal API proxy config: %w", err)
	}
	return string(append(data, '\n')), nil
}

func literalEndpoint(value string, requiredPort int) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("must be an IP address and port")
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" || !address.IsGlobalUnicast() && !address.IsLoopback() {
		return "", fmt.Errorf("host must be a literal unicast IP address")
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort != requiredPort {
		return "", fmt.Errorf("port must be %d", requiredPort)
	}
	return net.JoinHostPort(address.String(), strconv.Itoa(requiredPort)), nil
}
