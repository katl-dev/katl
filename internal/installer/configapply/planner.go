package configapply

import (
	"fmt"
	"strings"
	"time"

	"github.com/zariel/katl/internal/installer/generation"
)

const (
	NodeConfigurationChangeKind = "NodeConfigurationChange"
)

type NodeConfigurationChange struct {
	APIVersion       string
	Kind             string
	GenerationID     string
	SourceDigest     string
	Apply            Apply
	Changes          []Change
	Sysexts          []generation.ExtensionRef
	GeneratedConfext generation.GeneratedConfext
	Kubeadm          generation.KubeadmActionRequired
	RequestedAt      time.Time
}

type Apply struct {
	Mode string
}

type Result struct {
	Decision         Decision
	GenerationRecord generation.Record
	Status           generation.ConfigApplyStatus
}

func PlanChange(current generation.Record, request NodeConfigurationChange) (Result, error) {
	if strings.TrimSpace(request.APIVersion) != generation.APIVersion {
		return Result{}, fmt.Errorf("node configuration change apiVersion = %q, want %q", request.APIVersion, generation.APIVersion)
	}
	if strings.TrimSpace(request.Kind) != NodeConfigurationChangeKind {
		return Result{}, fmt.Errorf("node configuration change kind = %q, want %q", request.Kind, NodeConfigurationChangeKind)
	}
	if strings.TrimSpace(request.GenerationID) == "" {
		return Result{}, fmt.Errorf("generation id is required")
	}
	if strings.TrimSpace(request.SourceDigest) == "" {
		return Result{}, fmt.Errorf("configuration source digest is required")
	}
	requestedMode, err := normalizeRequestedMode(request.Apply.Mode)
	if err != nil {
		return Result{}, err
	}
	request.Apply.Mode = requestedMode
	if diagnostic, changed := selectedKubernetesSysextChange(current.Sysexts, request.Sysexts); changed {
		decision := Decision{
			RequestedMode:  request.Apply.Mode,
			ChangedDomains: changedDomainsWith(request.Changes, DomainSelectedKubernetesSysext),
			Diagnostics:    []Diagnostic{diagnostic},
		}
		return Result{Decision: decision}, fmt.Errorf("normal runtime configuration cannot change the selected Kubernetes sysext before target kubeadm access and kubelet activation gate are implemented")
	}

	decision, err := Plan(request.Apply.Mode, request.Changes)
	if err != nil {
		return Result{Decision: decision}, err
	}

	record, err := generation.NewRuntimeConfigRecord(generation.RuntimeConfigRequest{
		GenerationID:       request.GenerationID,
		Previous:           current,
		SourceDigest:       request.SourceDigest,
		Sysexts:            request.Sysexts,
		GeneratedConfext:   request.GeneratedConfext,
		ChangedDomains:     decision.ChangedDomains,
		RequestedApplyMode: decision.RequestedMode,
		AcceptedApplyMode:  decision.AcceptedMode,
		Kubeadm:            request.Kubeadm,
		CreatedAt:          request.RequestedAt,
	})
	if err != nil {
		return Result{Decision: decision}, err
	}
	status, err := generation.NewConfigApplyStatus(generation.ConfigApplyStatusRequest{
		GenerationID:       record.GenerationID,
		PreviousGeneration: current.GenerationID,
		RequestedApplyMode: decision.RequestedMode,
		AcceptedApplyMode:  decision.AcceptedMode,
		ChangedDomains:     decision.ChangedDomains,
		HealthState:        record.HealthState,
		Kubeadm:            request.Kubeadm,
		UpdatedAt:          request.RequestedAt,
	})
	if err != nil {
		return Result{Decision: decision}, err
	}
	status.DomainActions = domainActions(decision.AcceptedMode, decision.ChangedDomains)
	if err := generation.ValidateConfigApplyStatus(status); err != nil {
		return Result{Decision: decision}, err
	}

	return Result{
		Decision:         decision,
		GenerationRecord: record,
		Status:           status,
	}, nil
}

func selectedKubernetesSysextChange(current []generation.ExtensionRef, candidate []generation.ExtensionRef) (Diagnostic, bool) {
	if len(candidate) == 0 {
		return Diagnostic{}, false
	}
	currentRef, currentOK := kubernetesSysext(current)
	candidateRef, candidateOK := kubernetesSysext(candidate)
	if !currentOK && !candidateOK {
		return Diagnostic{}, false
	}
	if currentOK && candidateOK && strings.EqualFold(currentRef.SHA256, candidateRef.SHA256) && currentRef.PayloadVersion == candidateRef.PayloadVersion {
		return Diagnostic{}, false
	}
	return Diagnostic{
		Domain:            DomainSelectedKubernetesSysext,
		Classification:    ClassificationOperationOnly,
		Decision:          DecisionRejected,
		RequiredOperation: "kubernetes-upgrade",
		Message:           "selected Kubernetes sysext changes require target kubeadm access mode and kubelet activation gate",
	}, true
}

func kubernetesSysext(refs []generation.ExtensionRef) (generation.ExtensionRef, bool) {
	for _, ref := range refs {
		if ref.Name == "kubernetes" {
			return ref, true
		}
	}
	return generation.ExtensionRef{}, false
}

func changedDomainsWith(changes []Change, domain string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(changes)+1)
	for _, change := range changes {
		name := strings.TrimSpace(change.Domain)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if _, ok := seen[domain]; !ok {
		out = append(out, domain)
	}
	return out
}

func domainActions(acceptedMode string, domains []string) []generation.ConfigApplyDomainAction {
	actions := make([]generation.ConfigApplyDomainAction, 0, len(domains))
	for _, domain := range domains {
		action := generation.ConfigApplyDomainAction{
			Domain: domain,
		}
		if acceptedMode == generation.ApplyModeNextBoot {
			action.Action = "stage-next-boot"
			action.Status = generation.ConfigApplyActionSkipped
			action.Diagnostic = "domain staged into next boot generation"
		} else {
			action.Action = liveAction(domain)
			action.Status = generation.ConfigApplyActionPlanned
		}
		actions = append(actions, action)
	}
	return actions
}

func liveAction(domain string) string {
	switch domain {
	case DomainResolved:
		return "systemd-resolved-reload"
	case DomainSysctl:
		return "systemd-sysctl"
	case DomainTmpfiles:
		return "systemd-tmpfiles"
	case DomainNetworkd:
		return "networkctl-reload"
	case DomainBootstrapNodeMetadata:
		return "node-metadata-refresh"
	default:
		return "none"
	}
}
