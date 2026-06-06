package configapply

import (
	"fmt"
	"strings"

	"github.com/zariel/katl/internal/installer/generation"
)

const (
	DomainNodeIdentity             = "node-identity"
	DomainNetworkd                 = "networkd"
	DomainResolved                 = "resolved"
	DomainSysctl                   = "sysctl"
	DomainModulesLoad              = "modules-load"
	DomainTmpfiles                 = "tmpfiles"
	DomainMountUnits               = "mount-units"
	DomainExtraDisks               = "extra-disks"
	DomainKubeadmConfig            = "kubeadm-config"
	DomainBootstrapNodeMetadata    = "bootstrap-node-metadata"
	DomainSSHOperatorAccess        = "ssh-operator-access"
	DomainSystemRole               = "system-role"
	DomainSelectedKubeadmConfig    = "selected-kubeadm-config"
	DomainSelectedKubernetesSysext = "selected-kubernetes-sysext"
	DomainKubeletNodeIdentity      = "kubelet-node-identity"
	DomainHostAccountPolicy        = "host-account-policy"
	DomainEtcKubernetes            = "etc-kubernetes"
	DomainArbitraryEtc             = "arbitrary-etc"
	DomainRootSelection            = "root-selection"
	DomainSysextSelection          = "sysext-selection"
)

const (
	ClassificationOnlineApplicable = "online-applicable"
	ClassificationStagedOnly       = "staged-only"
	ClassificationRejectedLive     = "rejected-live"
)

const (
	DecisionAccepted       = "accepted"
	DecisionStagedRequired = "staged-required"
	DecisionRejected       = "rejected"
)

type Change struct {
	Domain          string
	LivePreflightOK bool
}

type Decision struct {
	RequestedMode  string
	AcceptedMode   string
	ChangedDomains []string
	Diagnostics    []Diagnostic
}

type Diagnostic struct {
	Domain         string
	Classification string
	Decision       string
	Message        string
}

type domainPolicy struct {
	Classification      string
	LivePreflight       bool
	NextBootAllowed     bool
	LiveRejectionReason string
}

func Plan(requestedMode string, changes []Change) (Decision, error) {
	if err := validateMode(requestedMode); err != nil {
		return Decision{}, err
	}
	if len(changes) == 0 {
		return Decision{}, fmt.Errorf("config apply changes are required")
	}
	decision := Decision{RequestedMode: requestedMode, AcceptedMode: requestedMode}
	seen := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		domain := strings.TrimSpace(change.Domain)
		if domain == "" {
			return Decision{}, fmt.Errorf("config apply domain is required")
		}
		if _, ok := seen[domain]; !ok {
			seen[domain] = struct{}{}
			decision.ChangedDomains = append(decision.ChangedDomains, domain)
		}
		policy, ok := domainPolicies[domain]
		if !ok {
			decision.Diagnostics = append(decision.Diagnostics, Diagnostic{
				Domain:         domain,
				Classification: ClassificationRejectedLive,
				Decision:       DecisionRejected,
				Message:        "unknown configuration domain is not supported",
			})
			continue
		}
		diagnostic := diagnosticForChange(requestedMode, change, policy)
		if diagnostic.Decision != DecisionAccepted {
			decision.Diagnostics = append(decision.Diagnostics, diagnostic)
		}
	}
	if len(decision.Diagnostics) > 0 {
		decision.AcceptedMode = ""
		return decision, fmt.Errorf("config apply %s request rejected for %d domain(s)", requestedMode, len(decision.Diagnostics))
	}
	return decision, nil
}

func DomainClassification(domain string) string {
	policy, ok := domainPolicies[strings.TrimSpace(domain)]
	if !ok {
		return ClassificationRejectedLive
	}
	return policy.Classification
}

func diagnosticForChange(requestedMode string, change Change, policy domainPolicy) Diagnostic {
	domain := strings.TrimSpace(change.Domain)
	if requestedMode == generation.ApplyModeNextBoot {
		if policy.NextBootAllowed {
			return Diagnostic{Domain: domain, Classification: policy.Classification, Decision: DecisionAccepted}
		}
		return Diagnostic{
			Domain:         domain,
			Classification: policy.Classification,
			Decision:       DecisionRejected,
			Message:        policy.LiveRejectionReason,
		}
	}
	switch policy.Classification {
	case ClassificationOnlineApplicable:
		if !policy.LivePreflight || change.LivePreflightOK {
			return Diagnostic{Domain: domain, Classification: policy.Classification, Decision: DecisionAccepted}
		}
		return Diagnostic{
			Domain:         domain,
			Classification: policy.Classification,
			Decision:       DecisionStagedRequired,
			Message:        "live preflight is required before this domain can apply online",
		}
	case ClassificationStagedOnly:
		return Diagnostic{
			Domain:         domain,
			Classification: policy.Classification,
			Decision:       DecisionStagedRequired,
			Message:        "domain is staged-only for normal runtime configuration apply",
		}
	default:
		return Diagnostic{
			Domain:         domain,
			Classification: policy.Classification,
			Decision:       DecisionRejected,
			Message:        policy.LiveRejectionReason,
		}
	}
}

func validateMode(mode string) error {
	switch strings.TrimSpace(mode) {
	case generation.ApplyModeLive, generation.ApplyModeNextBoot:
		return nil
	default:
		return fmt.Errorf("apply mode = %q, want %q or %q", mode, generation.ApplyModeLive, generation.ApplyModeNextBoot)
	}
}

var domainPolicies = map[string]domainPolicy{
	DomainResolved: {
		Classification:  ClassificationOnlineApplicable,
		NextBootAllowed: true,
	},
	DomainSysctl: {
		Classification:  ClassificationOnlineApplicable,
		NextBootAllowed: true,
	},
	DomainTmpfiles: {
		Classification:  ClassificationOnlineApplicable,
		NextBootAllowed: true,
	},
	DomainNetworkd: {
		Classification:  ClassificationOnlineApplicable,
		LivePreflight:   true,
		NextBootAllowed: true,
	},
	DomainBootstrapNodeMetadata: {
		Classification:  ClassificationOnlineApplicable,
		LivePreflight:   true,
		NextBootAllowed: true,
	},
	DomainNodeIdentity: {
		Classification:  ClassificationStagedOnly,
		NextBootAllowed: true,
	},
	DomainModulesLoad: {
		Classification:  ClassificationStagedOnly,
		NextBootAllowed: true,
	},
	DomainMountUnits: {
		Classification:  ClassificationStagedOnly,
		NextBootAllowed: true,
	},
	DomainExtraDisks: {
		Classification:  ClassificationStagedOnly,
		NextBootAllowed: true,
	},
	DomainSSHOperatorAccess: {
		Classification:  ClassificationStagedOnly,
		NextBootAllowed: true,
	},
	DomainKubeadmConfig: {
		Classification:  ClassificationStagedOnly,
		NextBootAllowed: true,
	},
	DomainSystemRole: {
		Classification:      ClassificationRejectedLive,
		NextBootAllowed:     true,
		LiveRejectionReason: "systemRole changes are not live-applicable",
	},
	DomainSelectedKubeadmConfig: {
		Classification:      ClassificationRejectedLive,
		NextBootAllowed:     true,
		LiveRejectionReason: "selected kubeadm config changes require an explicit kubeadm-aware action",
	},
	DomainSelectedKubernetesSysext: {
		Classification:      ClassificationRejectedLive,
		LiveRejectionReason: "selected Kubernetes sysext changes require an explicit update action",
	},
	DomainKubeletNodeIdentity: {
		Classification:      ClassificationRejectedLive,
		NextBootAllowed:     true,
		LiveRejectionReason: "kubelet node identity changes are not live-applicable",
	},
	DomainHostAccountPolicy: {
		Classification:      ClassificationRejectedLive,
		LiveRejectionReason: "host account policy is not user-owned configuration",
	},
	DomainEtcKubernetes: {
		Classification:      ClassificationRejectedLive,
		LiveRejectionReason: "/etc/kubernetes is kubeadm-owned mutable state",
	},
	DomainArbitraryEtc: {
		Classification:      ClassificationRejectedLive,
		LiveRejectionReason: "arbitrary /etc paths are not supported configuration domains",
	},
	DomainRootSelection: {
		Classification:      ClassificationRejectedLive,
		LiveRejectionReason: "root selection changes require an explicit update action",
	},
	DomainSysextSelection: {
		Classification:      ClassificationRejectedLive,
		LiveRejectionReason: "sysext selection changes require an explicit update action",
	},
}
