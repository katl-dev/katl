package configapply

import (
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/installer/generation"
)

const (
	DomainNodeIdentity                  = "node-identity"
	DomainNetworkd                      = "networkd"
	DomainResolved                      = "resolved"
	DomainSysctl                        = "sysctl"
	DomainModulesLoad                   = "modules-load"
	DomainTmpfiles                      = "tmpfiles"
	DomainMountUnits                    = "mount-units"
	DomainExtraDisks                    = "extra-disks"
	DomainKubeadmConfig                 = "kubeadm-config"
	DomainBootstrapNodeMetadata         = "bootstrap-node-metadata"
	DomainSSHOperatorAccess             = "ssh-operator-access"
	DomainSystemRole                    = "system-role"
	DomainSelectedKubeadmConfig         = "selected-kubeadm-config"
	DomainSelectedKubernetesSysext      = "selected-kubernetes-sysext"
	DomainKubeletNodeIdentity           = "kubelet-node-identity"
	DomainHostAccountPolicy             = "host-account-policy"
	DomainEtcKubernetes                 = "etc-kubernetes"
	DomainArbitraryEtc                  = "arbitrary-etc"
	DomainRootSelection                 = "root-selection"
	DomainSysextSelection               = "sysext-selection"
	DomainControlPlaneEndpointBootstrap = "control-plane-endpoint-bootstrap"
	DomainControlPlaneEndpointIdentity  = "control-plane-endpoint-identity"
	DomainControlPlaneEndpointRouting   = "control-plane-endpoint-routing"
)

const (
	ClassificationOnlineApplicable = "online-applicable"
	ClassificationStagedOnly       = "staged-only"
	ClassificationOperationOnly    = "operation-only"
	ClassificationRejected         = "rejected"
)

const (
	DecisionAccepted       = "accepted"
	DecisionStagedRequired = "staged-required"
	DecisionActionRequired = "action-required"
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
	Domain            string
	Classification    string
	Decision          string
	RequiredOperation string
	Message           string
}

type domainPolicy struct {
	Classification      string
	LivePreflight       bool
	NextBootAllowed     bool
	LiveRejectionReason string
	RequiredOperation   string
}

func Plan(requestedMode string, changes []Change) (Decision, error) {
	requestedMode, err := normalizeRequestedMode(requestedMode)
	if err != nil {
		return Decision{}, err
	}
	if len(changes) == 0 {
		return Decision{}, fmt.Errorf("config apply changes are required")
	}
	decision := Decision{RequestedMode: requestedMode}
	seen := make(map[string]struct{}, len(changes))
	needsNextBoot := false
	rejected := false
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
				Classification: ClassificationRejected,
				Decision:       DecisionRejected,
				Message:        "unknown configuration domain is not supported",
			})
			rejected = true
			continue
		}
		diagnostic := diagnosticForChange(requestedMode, change, policy)
		switch diagnostic.Decision {
		case DecisionAccepted:
		case DecisionActionRequired:
			needsNextBoot = true
			decision.Diagnostics = append(decision.Diagnostics, diagnostic)
		case DecisionStagedRequired:
			needsNextBoot = true
			if requestedMode != generation.ApplyModeAuto {
				decision.Diagnostics = append(decision.Diagnostics, diagnostic)
				rejected = true
			}
		default:
			decision.Diagnostics = append(decision.Diagnostics, diagnostic)
			rejected = true
		}
	}
	if rejected {
		decision.AcceptedMode = ""
		return decision, fmt.Errorf("config apply %s request rejected for %d domain(s)", requestedMode, len(decision.Diagnostics))
	}
	if requestedMode == generation.ApplyModeNextBoot || needsNextBoot {
		decision.AcceptedMode = generation.ApplyModeNextBoot
	} else {
		decision.AcceptedMode = generation.ApplyModeLive
	}
	return decision, nil
}

func DomainClassification(domain string) string {
	policy, ok := domainPolicies[strings.TrimSpace(domain)]
	if !ok {
		return ClassificationRejected
	}
	return policy.Classification
}

func diagnosticForChange(requestedMode string, change Change, policy domainPolicy) Diagnostic {
	domain := strings.TrimSpace(change.Domain)
	if requestedMode == generation.ApplyModeNextBoot {
		if policy.Classification == ClassificationOperationOnly {
			if policy.NextBootAllowed {
				return Diagnostic{
					Domain:            domain,
					Classification:    policy.Classification,
					Decision:          DecisionActionRequired,
					RequiredOperation: policy.RequiredOperation,
					Message:           requiredOperationMessage(policy),
				}
			}
			return Diagnostic{
				Domain:            domain,
				Classification:    policy.Classification,
				Decision:          DecisionRejected,
				RequiredOperation: policy.RequiredOperation,
				Message:           requiredOperationMessage(policy),
			}
		}
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
	case ClassificationOperationOnly:
		if requestedMode == generation.ApplyModeAuto && policy.NextBootAllowed {
			return Diagnostic{
				Domain:            domain,
				Classification:    policy.Classification,
				Decision:          DecisionActionRequired,
				RequiredOperation: policy.RequiredOperation,
				Message:           requiredOperationMessage(policy),
			}
		}
		return Diagnostic{
			Domain:            domain,
			Classification:    policy.Classification,
			Decision:          DecisionRejected,
			RequiredOperation: policy.RequiredOperation,
			Message:           requiredOperationMessage(policy),
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

func normalizeRequestedMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return generation.ApplyModeAuto, nil
	}
	switch strings.TrimSpace(mode) {
	case generation.ApplyModeAuto, generation.ApplyModeLive, generation.ApplyModeNextBoot:
		return mode, nil
	default:
		return "", fmt.Errorf("apply mode = %q, want %q, %q, or %q", mode, generation.ApplyModeAuto, generation.ApplyModeLive, generation.ApplyModeNextBoot)
	}
}

func requiredOperationMessage(policy domainPolicy) string {
	if strings.TrimSpace(policy.RequiredOperation) != "" {
		return "domain requires " + policy.RequiredOperation
	}
	return policy.LiveRejectionReason
}

var domainPolicies = map[string]domainPolicy{
	DomainResolved: {
		Classification:  ClassificationStagedOnly,
		NextBootAllowed: true,
	},
	DomainSysctl: {
		Classification:  ClassificationOnlineApplicable,
		NextBootAllowed: true,
	},
	DomainTmpfiles: {
		Classification:  ClassificationStagedOnly,
		NextBootAllowed: true,
	},
	DomainNetworkd: {
		Classification:  ClassificationStagedOnly,
		NextBootAllowed: true,
	},
	DomainBootstrapNodeMetadata: {
		Classification:  ClassificationStagedOnly,
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
		Classification:      ClassificationOperationOnly,
		NextBootAllowed:     true,
		LiveRejectionReason: "kubeadm desired state changes require an explicit kubeadm-aware operation",
		RequiredOperation:   "kubeadm-aware operation",
	},
	DomainSystemRole: {
		Classification:      ClassificationOperationOnly,
		LiveRejectionReason: "systemRole changes require wipe-reinstall or an explicit lifecycle operation",
		RequiredOperation:   "wipe-reinstall",
	},
	DomainSelectedKubeadmConfig: {
		Classification:      ClassificationOperationOnly,
		NextBootAllowed:     true,
		LiveRejectionReason: "selected kubeadm config changes require an explicit kubeadm-aware action",
		RequiredOperation:   "kubeadm-aware operation",
	},
	DomainSelectedKubernetesSysext: {
		Classification:      ClassificationOperationOnly,
		LiveRejectionReason: "selected Kubernetes sysext changes require an explicit update action",
		RequiredOperation:   "kubernetes-upgrade",
	},
	DomainKubeletNodeIdentity: {
		Classification:      ClassificationOperationOnly,
		LiveRejectionReason: "kubelet node identity changes are not live-applicable",
		RequiredOperation:   "kubeadm-aware operation",
	},
	DomainHostAccountPolicy: {
		Classification:      ClassificationRejected,
		LiveRejectionReason: "host account policy is not user-owned configuration",
	},
	DomainEtcKubernetes: {
		Classification:      ClassificationRejected,
		LiveRejectionReason: "/etc/kubernetes is kubeadm-owned mutable state",
	},
	DomainArbitraryEtc: {
		Classification:      ClassificationRejected,
		LiveRejectionReason: "arbitrary /etc paths are not supported configuration domains",
	},
	DomainRootSelection: {
		Classification:      ClassificationOperationOnly,
		LiveRejectionReason: "root selection changes require an explicit update action",
		RequiredOperation:   "host-upgrade",
	},
	DomainSysextSelection: {
		Classification:      ClassificationOperationOnly,
		LiveRejectionReason: "sysext selection changes require an explicit update action",
		RequiredOperation:   "host-upgrade",
	},
	DomainControlPlaneEndpointBootstrap: {
		Classification:  ClassificationStagedOnly,
		NextBootAllowed: true,
	},
	DomainControlPlaneEndpointIdentity: {
		Classification:      ClassificationOperationOnly,
		LiveRejectionReason: "initialized control-plane endpoint host, port, VIP, and ownership cannot change without a dedicated endpoint migration",
		RequiredOperation:   "control-plane-endpoint-migration (not yet supported)",
	},
	DomainControlPlaneEndpointRouting: {
		Classification:  ClassificationOnlineApplicable,
		NextBootAllowed: true,
	},
}
