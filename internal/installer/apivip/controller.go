package apivip

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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
)

const (
	StatusAPIVersion = "status.katl.dev/v1alpha1"
	StatusKind       = "APIEndpointVIPStatus"

	OwnershipOwned    = "owned"
	OwnershipReleased = "released"

	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

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
	Health            HealthChecker
	Interface         InterfaceChecker
	Owner             VIPOwner
	Writer            StatusWriter
	Clock             func() time.Time

	started           bool
	owned             bool
	ownershipObserved bool
	lastOwnership     time.Time
	lastHealth        time.Time
	successCount      int
	failureCount      int
}

type HealthResult struct {
	Healthy    bool
	StatusCode int
	Error      string
	CheckedAt  time.Time
}

type Status struct {
	APIVersion              string   `json:"apiVersion"`
	Kind                    string   `json:"kind"`
	EndpointHost            string   `json:"endpointHost"`
	EndpointPort            int      `json:"endpointPort"`
	VIPPrefix               string   `json:"vipPrefix"`
	AddressFamily           string   `json:"addressFamily"`
	VIPInterfaceName        string   `json:"vipInterfaceName"`
	VIPInterfaceKind        string   `json:"vipInterfaceKind"`
	VIPInterfaceReady       bool     `json:"vipInterfaceReady"`
	LocalVIPOwned           bool     `json:"localVIPOwned"`
	LocalVIPOwnedReported   bool     `json:"-"`
	NodeRoleSelected        bool     `json:"nodeRoleSelected"`
	ActivateOnRoles         []string `json:"activateOnRoles"`
	HealthState             string   `json:"healthState"`
	HealthTarget            string   `json:"healthTarget"`
	HealthStatusCode        int      `json:"healthStatusCode,omitempty"`
	HealthFailure           string   `json:"healthFailure,omitempty"`
	LastHealthTransition    string   `json:"lastHealthTransition,omitempty"`
	OwnershipState          string   `json:"ownershipState"`
	ReleaseReason           string   `json:"releaseReason,omitempty"`
	LastOwnershipTransition string   `json:"lastOwnershipTransition,omitempty"`
	ConfigDigest            string   `json:"configDigest"`
	SelectedGeneration      string   `json:"selectedGeneration,omitempty"`
	AppPayloadVersion       string   `json:"appPayloadVersion,omitempty"`
	FailureReason           string   `json:"failureReason,omitempty"`
	RecoveryRequired        bool     `json:"recoveryRequired,omitempty"`
	UpdatedAt               string   `json:"updatedAt"`
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
	failure := ""
	if !c.started {
		if err := c.Owner.SetOwned(ctx, config, false); err != nil {
			failure = "start without local VIP: " + inventory.Redact(err.Error())
		}
		c.started = true
	}

	interfaceReady := true
	if c.Interface != nil {
		interfaceReady, err = c.Interface.Ready(ctx, config)
		if err != nil && failure == "" {
			failure = inventory.Redact(err.Error())
		}
	}
	owned, ownerErr := c.Owner.Owned(ctx, config)
	if ownerErr != nil && failure == "" {
		failure = inventory.Redact(ownerErr.Error())
	}
	health := c.Health.Check(ctx, config.Health)
	if health.CheckedAt.IsZero() {
		health.CheckedAt = now
	}
	c.lastHealth = health.CheckedAt.UTC()

	dependenciesReady := failure == "" && interfaceReady && ownerErr == nil
	if health.Healthy && dependenciesReady {
		c.successCount++
		c.failureCount = 0
	} else {
		c.successCount = 0
		c.failureCount++
	}
	desiredOwned := owned
	releaseReason := ""
	switch {
	case !dependenciesReady:
		desiredOwned = false
		releaseReason = "dependency-not-ready"
	case !*config.Ownership.Enabled:
		desiredOwned = false
		releaseReason = "ownership-disabled"
	case c.successCount >= config.Health.SuccessThreshold:
		desiredOwned = true
	case c.failureCount >= config.Health.FailureThreshold:
		desiredOwned = false
		releaseReason = "local-health-failed"
	case !owned:
		releaseReason = "waiting-for-health-threshold"
	}
	if desiredOwned != owned {
		if err := c.Owner.SetOwned(ctx, config, desiredOwned); err != nil {
			if failure == "" {
				failure = inventory.Redact(err.Error())
			}
		} else {
			owned = desiredOwned
		}
	}
	c.observeOwnership(owned, now)
	status := c.status(config, health, interfaceReady, owned, releaseReason, failure, now)
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
	failure := ""
	owned, err := c.Owner.Owned(ctx, config)
	if err != nil {
		failure = inventory.Redact(err.Error())
	}
	if owned {
		if err := c.Owner.SetOwned(ctx, config, false); err != nil {
			if failure == "" {
				failure = inventory.Redact(err.Error())
			}
		} else {
			owned = false
		}
	}
	c.started = true
	c.observeOwnership(owned, now)
	status := c.status(config, HealthResult{CheckedAt: now}, true, owned, "service-stop", failure, now)
	if err := c.write(ctx, status); err != nil {
		return status, err
	}
	if failure != "" {
		return status, fmt.Errorf("%s", failure)
	}
	return status, nil
}

func (c *Controller) observeOwnership(owned bool, now time.Time) {
	if !c.ownershipObserved || c.owned != owned {
		c.owned = owned
		c.ownershipObserved = true
		c.lastOwnership = now
	}
}

func (h HTTPHealthChecker) Check(ctx context.Context, health Health) HealthResult {
	client := h.Client
	if client == nil {
		ca, err := os.ReadFile("/etc/kubernetes/pki/ca.crt")
		if err != nil {
			return HealthResult{Error: "waiting for kubeadm API CA", CheckedAt: time.Now().UTC()}
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return HealthResult{Error: "kubeadm API CA is invalid", CheckedAt: time.Now().UTC()}
		}
		timeout, err := time.ParseDuration(health.Timeout)
		if err != nil || timeout <= 0 {
			timeout = time.Second
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthTarget(health), nil)
	if err != nil {
		return HealthResult{Error: err.Error(), CheckedAt: time.Now().UTC()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return HealthResult{Error: err.Error(), CheckedAt: time.Now().UTC()}
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
			return fmt.Errorf("create API VIP status directory: %w", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write API VIP status: %w", err)
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
		return nil, fmt.Errorf("marshal API VIP status: %w", err)
	}
	return append(data, '\n'), nil
}

func DecodeStatus(reader io.Reader) (Status, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return Status{}, fmt.Errorf("read API VIP status: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var status Status
	if err := decoder.Decode(&status); err != nil {
		return Status{}, fmt.Errorf("decode API VIP status: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Status{}, fmt.Errorf("decode API VIP status: multiple JSON values")
		}
		return Status{}, fmt.Errorf("decode API VIP status: %w", err)
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
		return Status{}, fmt.Errorf("decode API VIP status field presence: %w", err)
	}
	status.LocalVIPOwnedReported = presence.LocalVIPOwned != nil
	return status, nil
}

func (c *Controller) status(config Config, health HealthResult, interfaceReady, owned bool, releaseReason, failure string, now time.Time) Status {
	healthState := HealthUnhealthy
	if health.Healthy {
		healthState = HealthHealthy
	}
	ownershipState := OwnershipReleased
	if owned {
		ownershipState = OwnershipOwned
		releaseReason = ""
	}
	return Status{
		APIVersion:              StatusAPIVersion,
		Kind:                    StatusKind,
		EndpointHost:            config.Endpoint.Host,
		EndpointPort:            config.Endpoint.Port,
		VIPPrefix:               config.Endpoint.VIP,
		AddressFamily:           config.Endpoint.AddressFamily,
		VIPInterfaceName:        config.VIPInterface.Name,
		VIPInterfaceKind:        config.VIPInterface.Kind,
		VIPInterfaceReady:       interfaceReady,
		LocalVIPOwned:           owned,
		NodeRoleSelected:        true,
		ActivateOnRoles:         slices.Clone(config.ActivateOn.Roles),
		HealthState:             healthState,
		HealthTarget:            healthTarget(config.Health),
		HealthStatusCode:        health.StatusCode,
		HealthFailure:           inventory.Redact(health.Error),
		LastHealthTransition:    formatTime(c.lastHealth),
		OwnershipState:          ownershipState,
		ReleaseReason:           releaseReason,
		LastOwnershipTransition: formatTime(c.lastOwnership),
		ConfigDigest:            digestString(renderAppConfig(config)),
		SelectedGeneration:      strings.TrimSpace(c.GenerationID),
		AppPayloadVersion:       strings.TrimSpace(c.AppPayloadVersion),
		FailureReason:           inventory.Redact(failure),
		RecoveryRequired:        failure != "",
		UpdatedAt:               formatTime(now),
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
