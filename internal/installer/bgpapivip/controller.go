package bgpapivip

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
}

type HealthChecker interface {
	Check(context.Context, Health) HealthResult
}

type InterfaceChecker interface {
	Ready(context.Context, Config) (bool, error)
}

type VIPOwner interface {
	Owned(context.Context, Config) (bool, error)
	SetOwned(context.Context, Config, bool) error
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
	Owner             VIPOwner
	Writer            StatusWriter
	Clock             func() time.Time

	started           bool
	routeOriginated   bool
	routeInitialized  bool
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
	RouterID           string
	Peers              []PeerRuntimeStatus
	FailureReason      string
	RouteOriginated    bool
}

type PeerRuntimeStatus struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	AddressFamily   string `json:"addressFamily"`
	SessionState    string `json:"sessionState"`
	AdminState      string `json:"adminState"`
	ASN             uint32 `json:"asn,omitempty"`
	LocalAddress    string `json:"localAddress,omitempty"`
	AcceptedRoutes  uint64 `json:"acceptedRoutes,omitempty"`
	ExportedRoutes  uint64 `json:"exportedRoutes,omitempty"`
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
	LocalVIPOwned               bool                `json:"localVIPOwned"`
	LocalVIPOwnedReported       bool                `json:"-"`
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
	if c.Owner == nil {
		return Status{}, fmt.Errorf("VIP owner is required")
	}
	if c.Health == nil {
		c.Health = HTTPHealthChecker{}
	}
	now := c.now()
	var failure string
	var startReleaseFailure string
	if !c.started {
		if err := c.Owner.SetOwned(ctx, config, false); err != nil {
			startReleaseFailure = "start without local VIP: " + inventory.Redact(err.Error())
		}
		c.started = true
	}

	var bird BirdRuntimeStatus
	var birdErr error
	if c.Bird != nil {
		bird, birdErr = c.Bird.Status(ctx)
	}
	if startReleaseFailure != "" && failure == "" {
		failure = startReleaseFailure
	}
	var routingFailure string
	birdReady := bird.ProcessActive && bird.ControlSocketReady
	if birdErr != nil && (bird.RouteOriginated || birdReady) {
		routingFailure = inventory.Redact(birdErr.Error())
	}
	if bird.FailureReason != "" && (bird.RouteOriginated || birdReady) && routingFailure == "" {
		routingFailure = inventory.Redact(bird.FailureReason)
	}
	interfaceReady := true
	if c.Interface != nil {
		var err error
		interfaceReady, err = c.Interface.Ready(ctx, config)
		if err != nil && failure == "" {
			failure = inventory.Redact(err.Error())
		}
	}
	localVIPOwned, ownerErr := c.Owner.Owned(ctx, config)
	ownerReady := ownerErr == nil
	if ownerErr != nil && failure == "" {
		failure = inventory.Redact(ownerErr.Error())
	}
	health := c.Health.Check(ctx, config.Health)
	if health.CheckedAt.IsZero() {
		health.CheckedAt = now
	}
	c.lastHealth = health.CheckedAt.UTC()

	dependenciesReady := startReleaseFailure == "" && interfaceReady && ownerReady
	if health.Healthy && dependenciesReady {
		c.successCount++
		c.failureCount = 0
	} else {
		c.successCount = 0
		c.failureCount++
	}
	healthAllowsAdvertise := c.successCount >= config.Health.SuccessThreshold
	healthRequiresWithdraw := c.failureCount >= config.Health.FailureThreshold
	desiredOwned := localVIPOwned
	withdrawReason := ""
	switch {
	case !dependenciesReady:
		desiredOwned = false
		withdrawReason = "dependency-not-ready"
	case !*config.Advertisement.Enabled:
		desiredOwned = false
		withdrawReason = "advertisement-disabled"
	case healthAllowsAdvertise:
		desiredOwned = true
	case healthRequiresWithdraw:
		desiredOwned = false
		withdrawReason = "local-health-failed"
	case !localVIPOwned:
		withdrawReason = "waiting-for-health-threshold"
	}
	if desiredOwned != localVIPOwned {
		if err := c.Owner.SetOwned(ctx, config, desiredOwned); err != nil {
			if failure == "" {
				failure = inventory.Redact(err.Error())
			}
		} else {
			localVIPOwned = desiredOwned
			if c.Bird != nil {
				refreshed, refreshErr := c.Bird.Status(ctx)
				bird = refreshed
				refreshedReady := refreshed.ProcessActive && refreshed.ControlSocketReady
				if refreshErr != nil && (refreshed.RouteOriginated || refreshedReady) {
					if routingFailure == "" {
						routingFailure = inventory.Redact(refreshErr.Error())
					}
				} else if refreshed.FailureReason != "" && (refreshed.RouteOriginated || refreshedReady) && routingFailure == "" {
					routingFailure = inventory.Redact(refreshed.FailureReason)
				}
			}
		}
	}
	c.observeRouteTransition(bird.RouteOriginated, now)
	if localVIPOwned && !bird.RouteOriginated && withdrawReason == "" {
		withdrawReason = "waiting-for-route-advertisement"
	}
	statusFailure := failure
	if statusFailure == "" {
		statusFailure = routingFailure
	}
	status := c.status(config, health, bird, interfaceReady, localVIPOwned, withdrawReason, statusFailure, now)
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
	if c.Owner == nil {
		return Status{}, fmt.Errorf("VIP owner is required")
	}
	now := c.now()
	var lifecycleFailure string
	c.started = true
	localVIPOwned, ownerErr := c.Owner.Owned(ctx, config)
	if ownerErr != nil {
		lifecycleFailure = inventory.Redact(ownerErr.Error())
	}
	if localVIPOwned {
		if err := c.Owner.SetOwned(ctx, config, false); err != nil {
			if lifecycleFailure == "" {
				lifecycleFailure = inventory.Redact(err.Error())
			}
		} else {
			localVIPOwned = false
		}
	}
	var bird BirdRuntimeStatus
	var routingFailure string
	if c.Bird != nil {
		var err error
		bird, err = c.Bird.Status(ctx)
		if err != nil {
			routingFailure = inventory.Redact(err.Error())
		}
	}
	c.observeRouteTransition(bird.RouteOriginated, now)
	health := HealthResult{Healthy: false, CheckedAt: now}
	statusFailure := lifecycleFailure
	if statusFailure == "" {
		statusFailure = routingFailure
	}
	status := c.status(config, health, bird, true, localVIPOwned, "service-stop", statusFailure, now)
	if err := c.write(ctx, status); err != nil {
		return status, err
	}
	if lifecycleFailure != "" {
		return status, fmt.Errorf("%s", lifecycleFailure)
	}
	return status, nil
}

func (c *Controller) observeRouteTransition(originated bool, now time.Time) {
	if !c.routeInitialized || c.routeOriginated != originated {
		c.routeOriginated = originated
		c.routeInitialized = true
		c.lastAdvertisement = now
	}
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

func DecodeStatus(reader io.Reader) (Status, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return Status{}, fmt.Errorf("read BGP API VIP status: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var status Status
	if err := decoder.Decode(&status); err != nil {
		return Status{}, fmt.Errorf("decode BGP API VIP status: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Status{}, fmt.Errorf("decode BGP API VIP status: multiple JSON values")
		}
		return Status{}, fmt.Errorf("decode BGP API VIP status: %w", err)
	}
	if status.APIVersion != StatusAPIVersion {
		return Status{}, fmt.Errorf("status apiVersion must be %s", StatusAPIVersion)
	}
	if status.Kind != StatusKind {
		return Status{}, fmt.Errorf("status kind must be %s", StatusKind)
	}
	var presence struct {
		LocalVIPOwned *bool `json:"localVIPOwned"`
	}
	if err := json.Unmarshal(data, &presence); err != nil {
		return Status{}, fmt.Errorf("decode BGP API VIP status field presence: %w", err)
	}
	status.LocalVIPOwnedReported = presence.LocalVIPOwned != nil
	return status, nil
}

func (c *Controller) status(config Config, health HealthResult, bird BirdRuntimeStatus, interfaceReady bool, localVIPOwned bool, withdrawReason string, failure string, now time.Time) Status {
	healthState := HealthUnhealthy
	if health.Healthy {
		healthState = HealthHealthy
	} else if health.CheckedAt.IsZero() {
		healthState = HealthUnknown
	}
	advertisement := AdvertisementWithdrawn
	if bird.RouteOriginated {
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
		LocalVIPOwned:               localVIPOwned,
		NodeRoleSelected:            true,
		AdvertiseOnRoles:            append([]string(nil), config.AdvertiseOn.Roles...),
		HealthState:                 healthState,
		HealthTarget:                healthTarget(config.Health),
		HealthStatusCode:            health.StatusCode,
		HealthFailure:               inventory.Redact(health.Error),
		LastHealthTransition:        formatTime(c.lastHealth),
		AdvertisementState:          advertisement,
		Withdrawn:                   !bird.RouteOriginated,
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
	return fmt.Sprintf("%s://%s%s", health.Scheme, net.JoinHostPort(health.Host, strconv.Itoa(health.Port)), health.Path)
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
