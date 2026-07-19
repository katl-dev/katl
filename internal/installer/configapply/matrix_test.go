package configapply

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/generation"
)

func TestDomainClassificationMatrix(t *testing.T) {
	tests := []struct {
		domain string
		want   string
	}{
		{DomainResolved, ClassificationStagedOnly},
		{DomainSysctl, ClassificationOnlineApplicable},
		{DomainTmpfiles, ClassificationStagedOnly},
		{DomainNetworkd, ClassificationStagedOnly},
		{DomainBootstrapNodeMetadata, ClassificationStagedOnly},
		{DomainNodeIdentity, ClassificationStagedOnly},
		{DomainModulesLoad, ClassificationStagedOnly},
		{DomainMountUnits, ClassificationStagedOnly},
		{DomainExtraDisks, ClassificationStagedOnly},
		{DomainSSHOperatorAccess, ClassificationStagedOnly},
		{DomainKubeadmConfig, ClassificationOperationOnly},
		{DomainSystemRole, ClassificationOperationOnly},
		{DomainSelectedKubeadmConfig, ClassificationOperationOnly},
		{DomainSelectedKubernetesSysext, ClassificationOperationOnly},
		{DomainKubeletNodeIdentity, ClassificationOperationOnly},
		{DomainHostAccountPolicy, ClassificationRejected},
		{DomainEtcKubernetes, ClassificationRejected},
		{DomainArbitraryEtc, ClassificationRejected},
		{DomainRootSelection, ClassificationOperationOnly},
		{DomainSysextSelection, ClassificationOperationOnly},
		{DomainControlPlaneEndpointBootstrap, ClassificationStagedOnly},
		{DomainControlPlaneEndpointIdentity, ClassificationOperationOnly},
		{DomainControlPlaneEndpointRouting, ClassificationOnlineApplicable},
		{"unknown-domain", ClassificationRejected},
	}
	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			if got := DomainClassification(tt.domain); got != tt.want {
				t.Fatalf("DomainClassification(%q) = %q, want %q", tt.domain, got, tt.want)
			}
		})
	}
}

func TestPlanDefaultsOmittedModeToAutoAndAcceptsSysctlLive(t *testing.T) {
	decision, err := Plan("", []Change{{Domain: DomainSysctl}})
	if err != nil {
		t.Fatalf("Plan() error = %v, diagnostics = %#v", err, decision.Diagnostics)
	}
	if decision.RequestedMode != generation.ApplyModeAuto || decision.AcceptedMode != generation.ApplyModeLive || len(decision.Diagnostics) != 0 {
		t.Fatalf("decision = %#v", decision)
	}
	if got, want := strings.Join(decision.ChangedDomains, ","), "sysctl"; got != want {
		t.Fatalf("changed domains = %q, want %q", got, want)
	}
}

func TestPlanAutoFallsBackToNextBootForStagedDomains(t *testing.T) {
	decision, err := Plan(generation.ApplyModeAuto, []Change{
		{Domain: DomainSysctl},
		{Domain: DomainNetworkd},
	})
	if err != nil {
		t.Fatalf("Plan() error = %v, diagnostics = %#v", err, decision.Diagnostics)
	}
	if decision.RequestedMode != generation.ApplyModeAuto || decision.AcceptedMode != generation.ApplyModeNextBoot || len(decision.Diagnostics) != 0 {
		t.Fatalf("decision = %#v", decision)
	}
	if got, want := strings.Join(decision.ChangedDomains, ","), "sysctl,networkd"; got != want {
		t.Fatalf("changed domains = %q, want %q", got, want)
	}
}

func TestPlanRejectsStrictLiveForStagedOnlyDomains(t *testing.T) {
	decision, err := Plan(generation.ApplyModeLive, []Change{{Domain: DomainNetworkd}})
	if err == nil {
		t.Fatalf("Plan() error = nil, decision = %#v", decision)
	}
	if len(decision.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v", decision.Diagnostics)
	}
	diagnostic := decision.Diagnostics[0]
	if diagnostic.Domain != DomainNetworkd || diagnostic.Decision != DecisionStagedRequired || diagnostic.Classification != ClassificationStagedOnly {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
	if !strings.Contains(diagnostic.Message, "staged-only") {
		t.Fatalf("diagnostic message = %q", diagnostic.Message)
	}
}

func TestPlanTreatsStagedOnlyDomainsAsNextBoot(t *testing.T) {
	for _, domain := range []string{
		DomainNodeIdentity,
		DomainModulesLoad,
		DomainMountUnits,
		DomainExtraDisks,
		DomainSSHOperatorAccess,
		DomainResolved,
		DomainTmpfiles,
		DomainNetworkd,
		DomainBootstrapNodeMetadata,
		DomainControlPlaneEndpointBootstrap,
	} {
		t.Run(domain, func(t *testing.T) {
			live, err := Plan(generation.ApplyModeLive, []Change{{Domain: domain}})
			if err == nil {
				t.Fatalf("Plan(live) error = nil, decision = %#v", live)
			}
			if len(live.Diagnostics) != 1 || live.Diagnostics[0].Decision != DecisionStagedRequired || live.Diagnostics[0].Classification != ClassificationStagedOnly {
				t.Fatalf("live diagnostics = %#v", live.Diagnostics)
			}

			next, err := Plan(generation.ApplyModeNextBoot, []Change{{Domain: domain}})
			if err != nil {
				t.Fatalf("Plan(next-boot) error = %v, decision = %#v", err, next)
			}
			if next.AcceptedMode != generation.ApplyModeNextBoot || len(next.Diagnostics) != 0 {
				t.Fatalf("next decision = %#v", next)
			}
		})
	}
}

func TestPlanRejectsOperationOnlyAndUnsupportedMutations(t *testing.T) {
	operationOnly := map[string]string{
		DomainSystemRole:                   "wipe-reinstall",
		DomainSelectedKubernetesSysext:     "kubernetes-upgrade",
		DomainKubeletNodeIdentity:          "kubeadm-aware operation",
		DomainRootSelection:                "host-upgrade",
		DomainSysextSelection:              "host-upgrade",
		DomainControlPlaneEndpointIdentity: "control-plane-endpoint-migration (not yet supported)",
	}
	for domain, required := range operationOnly {
		t.Run(domain, func(t *testing.T) {
			decision, err := Plan(generation.ApplyModeAuto, []Change{{Domain: domain}})
			if err == nil {
				t.Fatalf("Plan() error = nil, decision = %#v", decision)
			}
			if len(decision.Diagnostics) != 1 {
				t.Fatalf("diagnostics = %#v", decision.Diagnostics)
			}
			diagnostic := decision.Diagnostics[0]
			if diagnostic.Decision != DecisionRejected || diagnostic.Classification != ClassificationOperationOnly || diagnostic.RequiredOperation != required {
				t.Fatalf("diagnostic = %#v", diagnostic)
			}
			if !strings.Contains(diagnostic.Message, required) {
				t.Fatalf("diagnostic message = %q, want %q", diagnostic.Message, required)
			}
		})
	}

	for _, domain := range []string{
		DomainHostAccountPolicy,
		DomainEtcKubernetes,
		DomainArbitraryEtc,
		"unknown-domain",
	} {
		t.Run(domain, func(t *testing.T) {
			decision, err := Plan(generation.ApplyModeAuto, []Change{{Domain: domain}})
			if err == nil {
				t.Fatalf("Plan() error = nil, decision = %#v", decision)
			}
			if len(decision.Diagnostics) != 1 {
				t.Fatalf("diagnostics = %#v", decision.Diagnostics)
			}
			diagnostic := decision.Diagnostics[0]
			if diagnostic.Decision != DecisionRejected || diagnostic.Classification != ClassificationRejected {
				t.Fatalf("diagnostic = %#v", diagnostic)
			}
		})
	}
}

func TestPlanStagesKubeadmInputAsActionRequired(t *testing.T) {
	for _, domain := range []string{DomainKubeadmConfig, DomainSelectedKubeadmConfig} {
		t.Run(domain, func(t *testing.T) {
			for _, mode := range []string{generation.ApplyModeAuto, generation.ApplyModeNextBoot} {
				decision, err := Plan(mode, []Change{{Domain: domain}})
				if err != nil {
					t.Fatalf("Plan(%s) error = %v, decision = %#v", mode, err, decision)
				}
				if decision.AcceptedMode != generation.ApplyModeNextBoot || len(decision.Diagnostics) != 1 {
					t.Fatalf("Plan(%s) decision = %#v", mode, decision)
				}
				diagnostic := decision.Diagnostics[0]
				if diagnostic.Decision != DecisionActionRequired || diagnostic.Classification != ClassificationOperationOnly || diagnostic.RequiredOperation != "kubeadm-aware operation" {
					t.Fatalf("Plan(%s) diagnostic = %#v", mode, diagnostic)
				}
			}

			decision, err := Plan(generation.ApplyModeLive, []Change{{Domain: domain}})
			if err == nil || len(decision.Diagnostics) != 1 || decision.Diagnostics[0].Decision != DecisionRejected {
				t.Fatalf("Plan(live) error = %v, decision = %#v", err, decision)
			}
		})
	}
}

func TestPlanNextBootAllowsOnlyStagedAndOnlineDomains(t *testing.T) {
	allowed := []string{
		DomainSysctl,
		DomainNetworkd,
		DomainNodeIdentity,
		DomainModulesLoad,
		DomainKubeadmConfig,
		DomainSelectedKubeadmConfig,
		DomainControlPlaneEndpointBootstrap,
		DomainControlPlaneEndpointRouting,
	}
	for _, domain := range allowed {
		t.Run("allowed-"+domain, func(t *testing.T) {
			decision, err := Plan(generation.ApplyModeNextBoot, []Change{{Domain: domain}})
			if err != nil {
				t.Fatalf("Plan(next-boot) error = %v, decision = %#v", err, decision)
			}
		})
	}

	rejected := []string{
		DomainSystemRole,
		DomainKubeletNodeIdentity,
		DomainHostAccountPolicy,
		DomainSelectedKubernetesSysext,
		DomainEtcKubernetes,
		DomainArbitraryEtc,
		DomainRootSelection,
		DomainSysextSelection,
		DomainControlPlaneEndpointIdentity,
		"unknown-domain",
	}
	for _, domain := range rejected {
		t.Run("rejected-"+domain, func(t *testing.T) {
			decision, err := Plan(generation.ApplyModeNextBoot, []Change{{Domain: domain}})
			if err == nil {
				t.Fatalf("Plan(next-boot) error = nil, decision = %#v", decision)
			}
			if len(decision.Diagnostics) != 1 || decision.Diagnostics[0].Decision != DecisionRejected {
				t.Fatalf("diagnostics = %#v", decision.Diagnostics)
			}
		})
	}
}

func TestPlanMixedLiveRequestFailsAtomically(t *testing.T) {
	decision, err := Plan(generation.ApplyModeLive, []Change{
		{Domain: DomainSysctl},
		{Domain: DomainNetworkd},
		{Domain: DomainKubeadmConfig},
		{Domain: DomainEtcKubernetes},
	})
	if err == nil {
		t.Fatalf("Plan() error = nil, decision = %#v", decision)
	}
	if decision.AcceptedMode != "" {
		t.Fatalf("accepted mode = %q, want empty rejected decision", decision.AcceptedMode)
	}
	if len(decision.Diagnostics) != 3 {
		t.Fatalf("diagnostics = %#v, want staged networkd, operation-only kubeadm, and rejected /etc/kubernetes", decision.Diagnostics)
	}
	if decision.Diagnostics[0].Domain != DomainNetworkd || decision.Diagnostics[0].Decision != DecisionStagedRequired {
		t.Fatalf("first diagnostic = %#v", decision.Diagnostics[0])
	}
	if decision.Diagnostics[1].Domain != DomainKubeadmConfig || decision.Diagnostics[1].Decision != DecisionRejected || decision.Diagnostics[1].RequiredOperation != "kubeadm-aware operation" {
		t.Fatalf("second diagnostic = %#v", decision.Diagnostics[1])
	}
	if decision.Diagnostics[2].Domain != DomainEtcKubernetes || decision.Diagnostics[2].Decision != DecisionRejected {
		t.Fatalf("third diagnostic = %#v", decision.Diagnostics[2])
	}
	if got, want := strings.Join(decision.ChangedDomains, ","), "sysctl,networkd,kubeadm-config,etc-kubernetes"; got != want {
		t.Fatalf("changed domains = %q, want %q", got, want)
	}
}

func TestPlanRejectsBadRequestShape(t *testing.T) {
	if _, err := Plan("immediate", []Change{{Domain: DomainResolved}}); err == nil || !strings.Contains(err.Error(), "apply mode") {
		t.Fatalf("Plan() error = %v, want apply mode validation", err)
	}
	if _, err := Plan(generation.ApplyModeLive, nil); err == nil || !strings.Contains(err.Error(), "changes are required") {
		t.Fatalf("Plan() error = %v, want changes validation", err)
	}
	if _, err := Plan(generation.ApplyModeLive, []Change{{Domain: " "}}); err == nil || !strings.Contains(err.Error(), "domain is required") {
		t.Fatalf("Plan() error = %v, want domain validation", err)
	}
}
