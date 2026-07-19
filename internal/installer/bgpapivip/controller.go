package bgpapivip

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
)

const (
	StatusAPIVersion = "status.katl.dev/v1alpha1"
	StatusKind       = "BGPAPIEndpointStatus"

	AdvertisementAdvertised = "advertised"
	AdvertisementWithdrawn  = "withdrawn"

	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
	HealthUnknown   = "unknown"
)

type BirdClient interface {
	Status(context.Context) (BirdRuntimeStatus, error)
	SetAdvertisement(context.Context, bool) error
}

type HealthChecker interface {
	Check(context.Context, Health) HealthResult
}

type InterfaceChecker interface {
	Ready(context.Context, Config) (bool, error)
}

type StatusWriter interface {
	WriteStatus(context.Context, Status) error
}

type Controller struct {
	Config            Config
	GenerationID      string
	AppPayloadVersion string
	Bird              BirdClient
	Health            HealthChecker
	Interface         InterfaceChecker
	Writer            StatusWriter
	Clock             func() time.Time

	started           bool
	advertised        bool
	lastAdvertisement time.Time
	lastHealth        time.Time
	successCount      int
	failureCount      int
}

type BirdRuntimeStatus struct {
	ProcessActive      bool
	ControlSocketReady bool
	ControlSocketPath  string
	ReadinessState     string
	Peers              []PeerRuntimeStatus
	FailureReason      string
}

type PeerRuntimeStatus struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	AddressFamily   string `json:"addressFamily"`
	SessionState    string `json:"sessionState"`
	AdminState      string `json:"adminState"`
	LocalAddress    string `json:"localAddress,omitempty"`
	LastTransition  string `json:"lastTransition,omitempty"`
	AuthConfigured  bool   `json:"authConfigured,omitempty"`
	FailureCategory string `json:"failureCategory,omitempty"`
}

type HealthResult struct {
	Healthy    bool
	StatusCode int
	Error      string
	CheckedAt  time.Time
}

type Status struct {
	APIVersion                  string              `json:"apiVersion"`
	Kind                        string              `json:"kind"`
	EndpointHost                string              `json:"endpointHost"`
	EndpointPort                int                 `json:"endpointPort"`
	VIPPrefix                   string              `json:"vipPrefix"`
	AddressFamily               string              `json:"addressFamily"`
	VIPInterfaceName            string              `json:"vipInterfaceName"`
	VIPInterfaceKind            string              `json:"vipInterfaceKind"`
	VIPInterfaceReady           bool                `json:"vipInterfaceReady"`
	NodeRoleSelected            bool                `json:"nodeRoleSelected"`
	AdvertiseOnRoles            []string            `json:"advertiseOnRoles"`
	HealthState                 string              `json:"healthState"`
	HealthTarget                string              `json:"healthTarget"`
	HealthStatusCode            int                 `json:"healthStatusCode,omitempty"`
	HealthFailure               string              `json:"healthFailure,omitempty"`
	LastHealthTransition        string              `json:"lastHealthTransition,omitempty"`
	AdvertisementState          string              `json:"advertisementState"`
	Withdrawn                   bool                `json:"withdrawn"`
	WithdrawReason              string              `json:"withdrawReason,omitempty"`
	LastAdvertisementTransition string              `json:"lastAdvertisementTransition,omitempty"`
	BirdProcessActive           bool                `json:"birdProcessActive"`
	BirdControlSocketReady      bool                `json:"birdControlSocketReady"`
	BirdControlSocketPath       string              `json:"birdControlSocketPath,omitempty"`
	BirdReadinessState          string              `json:"birdReadinessState"`
	PeerSummary                 []PeerRuntimeStatus `json:"peerSummary,omitempty"`
	RedactionVersion            string              `json:"redactionVersion"`
	RoutePolicyDigest           string              `json:"routePolicyDigest"`
	ConfigDigest                string              `json:"configDigest"`
	LoadedConfigDigest          string              `json:"loadedConfigDigest,omitempty"`
	SelectedGeneration          string              `json:"selectedGeneration,omitempty"`
	AppPayloadVersion           string              `json:"appPayloadVersion,omitempty"`
	FailureReason               string              `json:"failureReason,omitempty"`
	RecoveryRequired            bool                `json:"recoveryRequired,omitempty"`
	UpdatedAt                   string              `json:"updatedAt"`
}

type FileStatusWriter struct {
	LivePath      string
	OperationPath string
}

type HTTPHealthChecker struct {
	Client *http.Client
}

type AlwaysReadyInterface struct{}

func (c *Controller) RunOnce(ctx context.Context) (Status, error) {
	config, err := Normalize(c.Config)
	if err != nil {
		return Status{}, err
	}
	c.Config = config
	if c.Bird == nil {
		return Status{}, fmt.Errorf("bird client is required")
	}
	if c.Health == nil {
		c.Health = HTTPHealthChecker{}
	}
	now := c.now()
	var failure string
	if !c.started {
		if err := c.Bird.SetAdvertisement(ctx, false); err != nil {
			failure = "start withdrawn: " + inventory.Redact(err.Error())
		}
		c.started = true
		c.advertised = false
		c.lastAdvertisement = now
	}

	bird, err := c.Bird.Status(ctx)
	if err != nil && failure == "" {
		failure = inventory.Redact(err.Error())
	}
	if bird.FailureReason != "" && failure == "" {
		failure = inventory.Redact(bird.FailureReason)
	}
	interfaceReady := true
	if c.Interface != nil {
		var err error
		interfaceReady, err = c.Interface.Ready(ctx, config)
		if err != nil && failure == "" {
			failure = inventory.Redact(err.Error())
		}
	}
	health := c.Health.Check(ctx, config.Health)
	if health.CheckedAt.IsZero() {
		health.CheckedAt = now
	}
	c.lastHealth = health.CheckedAt.UTC()

	dependenciesReady := bird.ProcessActive && bird.ControlSocketReady && interfaceReady
	if health.Healthy && dependenciesReady {
		c.successCount++
		c.failureCount = 0
	} else {
		c.successCount = 0
		c.failureCount++
	}
	healthAllowsAdvertise := c.successCount >= config.Health.SuccessThreshold
	healthRequiresWithdraw := c.failureCount >= config.Health.FailureThreshold
	desiredAdvertised := c.advertised
	withdrawReason := ""
	switch {
	case !dependenciesReady:
		desiredAdvertised = false
		withdrawReason = "dependency-not-ready"
	case !*config.Advertisement.Enabled:
		desiredAdvertised = false
		withdrawReason = "advertisement-disabled"
	case healthAllowsAdvertise:
		desiredAdvertised = true
	case healthRequiresWithdraw:
		desiredAdvertised = false
		withdrawReason = "local-health-failed"
	case !c.advertised:
		withdrawReason = "waiting-for-health-threshold"
	}
	if desiredAdvertised != c.advertised {
		if err := c.Bird.SetAdvertisement(ctx, desiredAdvertised); err != nil {
			failure = inventory.Redact(err.Error())
		} else {
			c.advertised = desiredAdvertised
			c.lastAdvertisement = now
		}
	}
	status := c.status(config, health, bird, interfaceReady, withdrawReason, failure, now)
	if err := c.write(ctx, status); err != nil {
		return status, err
	}
	if failure != "" {
		return status, fmt.Errorf("%s", failure)
	}
	return status, nil
}

func (c *Controller) Stop(ctx context.Context) (Status, error) {
	config, err := Normalize(c.Config)
	if err != nil {
		return Status{}, err
	}
	c.Config = config
	if c.Bird == nil {
		return Status{}, fmt.Errorf("bird client is required")
	}
	now := c.now()
	failure := ""
	if err := c.Bird.SetAdvertisement(ctx, false); err != nil {
		failure = inventory.Redact(err.Error())
	}
	c.started = true
	c.advertised = false
	c.lastAdvertisement = now
	bird, err := c.Bird.Status(ctx)
	if err != nil && failure == "" {
		failure = inventory.Redact(err.Error())
	}
	health := HealthResult{Healthy: false, CheckedAt: now}
	status := c.status(config, health, bird, true, "service-stop", failure, now)
	if err := c.write(ctx, status); err != nil {
		return status, err
	}
	if failure != "" {
		return status, fmt.Errorf("%s", failure)
	}
	return status, nil
}

func (h HTTPHealthChecker) Check(ctx context.Context, health Health) HealthResult {
	client := h.Client
	if client == nil {
		ca, err := os.ReadFile("/etc/kubernetes/pki/ca.crt")
		if err != nil {
			return HealthResult{Healthy: false, Error: "waiting for kubeadm API CA", CheckedAt: time.Now().UTC()}
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return HealthResult{Healthy: false, Error: "kubeadm API CA is invalid", CheckedAt: time.Now().UTC()}
		}
		timeout, err := time.ParseDuration(health.Timeout)
		if err != nil || timeout <= 0 {
			timeout = 1 * time.Second
		}
		client = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    roots,
				ServerName: health.TLSServerName,
			}},
		}
	}
	target := healthTarget(health)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return HealthResult{Healthy: false, Error: err.Error(), CheckedAt: time.Now().UTC()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return HealthResult{Healthy: false, Error: err.Error(), CheckedAt: time.Now().UTC()}
	}
	defer resp.Body.Close()
	return HealthResult{Healthy: resp.StatusCode >= 200 && resp.StatusCode < 300, StatusCode: resp.StatusCode, CheckedAt: time.Now().UTC()}
}

func (AlwaysReadyInterface) Ready(context.Context, Config) (bool, error) {
	return true, nil
}

func (w FileStatusWriter) WriteStatus(_ context.Context, status Status) error {
	data, err := MarshalStatus(status)
	if err != nil {
		return err
	}
	for _, path := range []string{w.LivePath, w.OperationPath} {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create BGP API VIP status directory: %w", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write BGP API VIP status: %w", err)
		}
	}
	return nil
}

func MarshalStatus(status Status) ([]byte, error) {
	if status.APIVersion != StatusAPIVersion {
		return nil, fmt.Errorf("status apiVersion must be %s", StatusAPIVersion)
	}
	if status.Kind != StatusKind {
		return nil, fmt.Errorf("status kind must be %s", StatusKind)
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal BGP API VIP status: %w", err)
	}
	return append(data, '\n'), nil
}

func (c *Controller) status(config Config, health HealthResult, bird BirdRuntimeStatus, interfaceReady bool, withdrawReason string, failure string, now time.Time) Status {
	healthState := HealthUnhealthy
	if health.Healthy {
		healthState = HealthHealthy
	} else if health.CheckedAt.IsZero() {
		healthState = HealthUnknown
	}
	advertisement := AdvertisementWithdrawn
	if c.advertised {
		advertisement = AdvertisementAdvertised
		withdrawReason = ""
	}
	if bird.ControlSocketPath == "" {
		bird.ControlSocketPath = BirdControlSocketPath
	}
	return Status{
		APIVersion:                  StatusAPIVersion,
		Kind:                        StatusKind,
		EndpointHost:                config.Endpoint.Host,
		EndpointPort:                config.Endpoint.Port,
		VIPPrefix:                   config.Endpoint.VIP,
		AddressFamily:               config.Endpoint.AddressFamily,
		VIPInterfaceName:            config.VIPInterface.Name,
		VIPInterfaceKind:            config.VIPInterface.Kind,
		VIPInterfaceReady:           interfaceReady,
		NodeRoleSelected:            true,
		AdvertiseOnRoles:            append([]string(nil), config.AdvertiseOn.Roles...),
		HealthState:                 healthState,
		HealthTarget:                healthTarget(config.Health),
		HealthStatusCode:            health.StatusCode,
		HealthFailure:               inventory.Redact(health.Error),
		LastHealthTransition:        formatTime(c.lastHealth),
		AdvertisementState:          advertisement,
		Withdrawn:                   !c.advertised,
		WithdrawReason:              withdrawReason,
		LastAdvertisementTransition: formatTime(c.lastAdvertisement),
		BirdProcessActive:           bird.ProcessActive,
		BirdControlSocketReady:      bird.ControlSocketReady,
		BirdControlSocketPath:       bird.ControlSocketPath,
		BirdReadinessState:          defaultString(bird.ReadinessState, "unknown"),
		PeerSummary:                 redactPeers(bird.Peers),
		RedactionVersion:            "inventory-v1",
		RoutePolicyDigest:           digestString(renderBirdConfig(config)),
		ConfigDigest:                digestString(renderAppConfig(config)),
		LoadedConfigDigest:          digestString(renderBirdConfig(config)),
		SelectedGeneration:          strings.TrimSpace(c.GenerationID),
		AppPayloadVersion:           strings.TrimSpace(c.AppPayloadVersion),
		FailureReason:               inventory.Redact(failure),
		RecoveryRequired:            failure != "",
		UpdatedAt:                   formatTime(now),
	}
}

func (c *Controller) write(ctx context.Context, status Status) error {
	if c.Writer == nil {
		return nil
	}
	return c.Writer.WriteStatus(ctx, status)
}

func (c *Controller) now() time.Time {
	if c.Clock != nil {
		return c.Clock().UTC()
	}
	return time.Now().UTC()
}

func redactPeers(peers []PeerRuntimeStatus) []PeerRuntimeStatus {
	out := make([]PeerRuntimeStatus, 0, len(peers))
	for _, peer := range peers {
		peer.FailureCategory = inventory.Redact(peer.FailureCategory)
		out = append(out, peer)
	}
	return out
}

func healthTarget(health Health) string {
	return fmt.Sprintf("%s://%s:%d%s", health.Scheme, health.Host, health.Port, health.Path)
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
