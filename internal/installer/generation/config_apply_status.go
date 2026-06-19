package generation

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ConfigApplyStatusKind = "ConfigApplyStatus"
)

const (
	ApplyModeAuto     = "auto"
	ApplyModeLive     = "live"
	ApplyModeNextBoot = "next-boot"
)

const (
	ConfigApplyPhasePlanned     = "planned"
	ConfigApplyPhaseRendered    = "rendered"
	ConfigApplyPhaseStaged      = "staged"
	ConfigApplyPhaseActivating  = "activating"
	ConfigApplyPhaseActive      = "active"
	ConfigApplyPhaseNextBoot    = "next-boot"
	ConfigApplyPhaseRollingBack = "rolling-back"
	ConfigApplyPhaseRolledBack  = "rolled-back"
	ConfigApplyPhaseFailed      = "failed"
)

const (
	ConfigApplyActionPlanned = "planned"
	ConfigApplyActionSkipped = "skipped"
	ConfigApplyActionPassed  = "passed"
	ConfigApplyActionFailed  = "failed"
)

type ConfigApplyStatus struct {
	APIVersion          string                    `json:"apiVersion"`
	Kind                string                    `json:"kind"`
	GenerationID        string                    `json:"generationID"`
	PreviousGeneration  string                    `json:"previousGenerationID"`
	RequestedApplyMode  string                    `json:"requestedApplyMode"`
	AcceptedApplyMode   string                    `json:"acceptedApplyMode"`
	ChangedDomains      []string                  `json:"changedDomains"`
	Phase               string                    `json:"phase"`
	HealthState         string                    `json:"healthState"`
	DomainActions       []ConfigApplyDomainAction `json:"domainActions,omitempty"`
	DiagnosticArtifacts []DiagnosticArtifact      `json:"diagnosticArtifacts,omitempty"`
	Rollback            *ConfigApplyRollback      `json:"rollback,omitempty"`
	Kubeadm             KubeadmActionRequired     `json:"kubeadm"`
	FailureReason       string                    `json:"failureReason,omitempty"`
	UpdatedAt           time.Time                 `json:"updatedAt"`
}

type ConfigApplyStatusRequest struct {
	GenerationID       string
	PreviousGeneration string
	RequestedApplyMode string
	AcceptedApplyMode  string
	ChangedDomains     []string
	HealthState        string
	Kubeadm            KubeadmActionRequired
	UpdatedAt          time.Time
}

type ConfigApplyDomainAction struct {
	Domain     string `json:"domain"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

type DiagnosticArtifact struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type ConfigApplyRollback struct {
	TargetGenerationID string `json:"targetGenerationID"`
	Result             string `json:"result"`
	Reason             string `json:"reason,omitempty"`
}

type KubeadmActionRequired struct {
	Required bool   `json:"required"`
	Reason   string `json:"reason,omitempty"`
}

func NewConfigApplyStatus(request ConfigApplyStatusRequest) (ConfigApplyStatus, error) {
	if strings.TrimSpace(request.GenerationID) == "" {
		return ConfigApplyStatus{}, fmt.Errorf("generation id is required")
	}
	if strings.TrimSpace(request.PreviousGeneration) == "" {
		return ConfigApplyStatus{}, fmt.Errorf("previous generation id is required")
	}
	if err := validateRequestedApplyMode("requested apply mode", request.RequestedApplyMode); err != nil {
		return ConfigApplyStatus{}, err
	}
	if strings.TrimSpace(request.AcceptedApplyMode) == "" {
		request.AcceptedApplyMode = request.RequestedApplyMode
	}
	if err := validateAcceptedApplyMode("accepted apply mode", request.AcceptedApplyMode); err != nil {
		return ConfigApplyStatus{}, err
	}
	domains, err := cleanChangedDomains(request.ChangedDomains)
	if err != nil {
		return ConfigApplyStatus{}, err
	}
	if strings.TrimSpace(request.HealthState) == "" {
		request.HealthState = "unknown"
	}
	updatedAt := request.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	status := ConfigApplyStatus{
		APIVersion:         APIVersion,
		Kind:               ConfigApplyStatusKind,
		GenerationID:       strings.TrimSpace(request.GenerationID),
		PreviousGeneration: strings.TrimSpace(request.PreviousGeneration),
		RequestedApplyMode: strings.TrimSpace(request.RequestedApplyMode),
		AcceptedApplyMode:  strings.TrimSpace(request.AcceptedApplyMode),
		ChangedDomains:     domains,
		Phase:              ConfigApplyPhasePlanned,
		HealthState:        strings.TrimSpace(request.HealthState),
		Kubeadm:            redactKubeadmActionRequired(request.Kubeadm),
		UpdatedAt:          updatedAt.UTC(),
	}
	if err := ValidateConfigApplyStatus(status); err != nil {
		return ConfigApplyStatus{}, err
	}
	return status, nil
}

func ConfigApplyStatusPath(root string, generationID string) (string, error) {
	generationID, err := cleanSegment("generation id", generationID)
	if err != nil {
		return "", err
	}
	return rootedPath(root, filepath.Join(GenerationRecordsDir, generationID, "config-apply-status.json"))
}

func MarshalConfigApplyStatus(status ConfigApplyStatus) ([]byte, error) {
	if err := ValidateConfigApplyStatus(status); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config apply status: %w", err)
	}
	return append(data, '\n'), nil
}

func WriteConfigApplyStatus(path string, status ConfigApplyStatus) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("config apply status path is required")
	}
	data, err := MarshalConfigApplyStatus(status)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config apply status directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write config apply status: %w", err)
	}
	return nil
}

func ReadConfigApplyStatus(path string) (ConfigApplyStatus, error) {
	if strings.TrimSpace(path) == "" {
		return ConfigApplyStatus{}, fmt.Errorf("config apply status path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ConfigApplyStatus{}, fmt.Errorf("read config apply status: %w", err)
	}
	var status ConfigApplyStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return ConfigApplyStatus{}, fmt.Errorf("decode config apply status: %w", err)
	}
	if err := ValidateConfigApplyStatus(status); err != nil {
		return ConfigApplyStatus{}, err
	}
	return status, nil
}

func MarkConfigApplyPhase(status ConfigApplyStatus, phase string, now time.Time) (ConfigApplyStatus, error) {
	if err := validateConfigApplyPhase(phase); err != nil {
		return ConfigApplyStatus{}, err
	}
	status.Phase = phase
	status.UpdatedAt = nonzeroUTC(now)
	return status, ValidateConfigApplyStatus(status)
}

func MarkConfigApplyFailed(status ConfigApplyStatus, cause error, now time.Time) (ConfigApplyStatus, error) {
	status.Phase = ConfigApplyPhaseFailed
	status.FailureReason = RedactConfigApplyMessage(errorString(cause))
	status.UpdatedAt = nonzeroUTC(now)
	return status, ValidateConfigApplyStatus(status)
}

func MarkConfigApplyRollback(status ConfigApplyStatus, targetGenerationID, result, reason string, now time.Time) (ConfigApplyStatus, error) {
	if strings.TrimSpace(targetGenerationID) == "" {
		return ConfigApplyStatus{}, fmt.Errorf("rollback target generation id is required")
	}
	if strings.TrimSpace(result) == "" {
		return ConfigApplyStatus{}, fmt.Errorf("rollback result is required")
	}
	status.Phase = ConfigApplyPhaseRolledBack
	status.Rollback = &ConfigApplyRollback{
		TargetGenerationID: strings.TrimSpace(targetGenerationID),
		Result:             strings.TrimSpace(result),
		Reason:             RedactConfigApplyMessage(reason),
	}
	status.UpdatedAt = nonzeroUTC(now)
	return status, ValidateConfigApplyStatus(status)
}

func ValidateConfigApplyStatus(status ConfigApplyStatus) error {
	if status.APIVersion != APIVersion {
		return fmt.Errorf("config apply status apiVersion = %q, want %q", status.APIVersion, APIVersion)
	}
	if status.Kind != ConfigApplyStatusKind {
		return fmt.Errorf("config apply status kind = %q, want %q", status.Kind, ConfigApplyStatusKind)
	}
	if strings.TrimSpace(status.GenerationID) == "" {
		return fmt.Errorf("config apply status generation id is required")
	}
	if strings.TrimSpace(status.PreviousGeneration) == "" {
		return fmt.Errorf("config apply status previous generation id is required")
	}
	if err := validateRequestedApplyMode("requested apply mode", status.RequestedApplyMode); err != nil {
		return err
	}
	if err := validateAcceptedApplyMode("accepted apply mode", status.AcceptedApplyMode); err != nil {
		return err
	}
	if len(status.ChangedDomains) == 0 {
		return fmt.Errorf("config apply status changed domains are required")
	}
	for _, domain := range status.ChangedDomains {
		if strings.TrimSpace(domain) == "" {
			return fmt.Errorf("config apply status changed domain is required")
		}
	}
	if err := validateConfigApplyPhase(status.Phase); err != nil {
		return err
	}
	if strings.TrimSpace(status.HealthState) == "" {
		return fmt.Errorf("config apply status health state is required")
	}
	if status.UpdatedAt.IsZero() {
		return fmt.Errorf("config apply status updatedAt is required")
	}
	for _, action := range status.DomainActions {
		if strings.TrimSpace(action.Domain) == "" || strings.TrimSpace(action.Action) == "" || strings.TrimSpace(action.Status) == "" {
			return fmt.Errorf("config apply status domain action requires domain, action, and status")
		}
		if err := validateActionStatus(action.Status); err != nil {
			return err
		}
	}
	for _, artifact := range status.DiagnosticArtifacts {
		if strings.TrimSpace(artifact.Name) == "" || strings.TrimSpace(artifact.Path) == "" {
			return fmt.Errorf("config apply status diagnostic artifacts require name and path")
		}
	}
	if status.Rollback != nil {
		if strings.TrimSpace(status.Rollback.TargetGenerationID) == "" || strings.TrimSpace(status.Rollback.Result) == "" {
			return fmt.Errorf("config apply status rollback requires target generation id and result")
		}
	}
	return nil
}

var (
	configApplyURLPattern                = regexp.MustCompile(`https?://[^\s]+`)
	configApplyBootstrapTokenPattern     = regexp.MustCompile(`\b[a-z0-9]{6}\.[a-z0-9]{16}\b`)
	configApplyDiscoveryTokenHashPattern = regexp.MustCompile(`(?i)\b(discovery-token-ca-cert-hash\s+)?sha256:[a-f0-9]{64}\b`)
	configApplyBearerTokenPattern        = regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9._~+/=-]+`)
	configApplyPrivateKeyPattern         = regexp.MustCompile(`(?is)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
)

func RedactConfigApplyMessage(value string) string {
	value = configApplyPrivateKeyPattern.ReplaceAllString(value, "[REDACTED PRIVATE KEY]")
	value = configApplyBootstrapTokenPattern.ReplaceAllString(value, "[REDACTED BOOTSTRAP TOKEN]")
	value = configApplyDiscoveryTokenHashPattern.ReplaceAllString(value, "${1}[REDACTED DISCOVERY TOKEN HASH]")
	value = configApplyBearerTokenPattern.ReplaceAllString(value, "${1}[REDACTED]")
	return configApplyURLPattern.ReplaceAllStringFunc(value, redactConfigApplyURL)
}

func cleanChangedDomains(domains []string) ([]string, error) {
	seen := make(map[string]struct{}, len(domains))
	cleaned := make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = strings.TrimSpace(domain)
		if domain == "" {
			return nil, fmt.Errorf("changed domain is required")
		}
		if _, ok := seen[domain]; ok {
			continue
		}
		seen[domain] = struct{}{}
		cleaned = append(cleaned, domain)
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("changed domains are required")
	}
	return cleaned, nil
}

func redactKubeadmActionRequired(action KubeadmActionRequired) KubeadmActionRequired {
	action.Reason = RedactConfigApplyMessage(action.Reason)
	return action
}

func validateRequestedApplyMode(name, value string) error {
	switch strings.TrimSpace(value) {
	case ApplyModeAuto, ApplyModeLive, ApplyModeNextBoot:
		return nil
	default:
		return fmt.Errorf("%s = %q, want %q, %q, or %q", name, value, ApplyModeAuto, ApplyModeLive, ApplyModeNextBoot)
	}
}

func validateAcceptedApplyMode(name, value string) error {
	switch strings.TrimSpace(value) {
	case ApplyModeLive, ApplyModeNextBoot:
		return nil
	default:
		return fmt.Errorf("%s = %q, want %q or %q", name, value, ApplyModeLive, ApplyModeNextBoot)
	}
}

func validateConfigApplyPhase(phase string) error {
	switch strings.TrimSpace(phase) {
	case ConfigApplyPhasePlanned, ConfigApplyPhaseRendered, ConfigApplyPhaseStaged, ConfigApplyPhaseActivating, ConfigApplyPhaseActive, ConfigApplyPhaseNextBoot, ConfigApplyPhaseRollingBack, ConfigApplyPhaseRolledBack, ConfigApplyPhaseFailed:
		return nil
	default:
		return fmt.Errorf("config apply phase = %q is unsupported", phase)
	}
}

func validateActionStatus(status string) error {
	switch strings.TrimSpace(status) {
	case ConfigApplyActionPlanned, ConfigApplyActionSkipped, ConfigApplyActionPassed, ConfigApplyActionFailed:
		return nil
	default:
		return fmt.Errorf("config apply action status = %q is unsupported", status)
	}
}

func redactConfigApplyURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return value
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func nonzeroUTC(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
