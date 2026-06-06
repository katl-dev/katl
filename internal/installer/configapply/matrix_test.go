package configapply

import (
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/generation"
)

func TestDomainClassificationMatrix(t *testing.T) {
	tests := []struct {
		domain string
		want   string
	}{
		{DomainResolved, ClassificationOnlineApplicable},
		{DomainSysctl, ClassificationOnlineApplicable},
		{DomainTmpfiles, ClassificationOnlineApplicable},
		{DomainNetworkd, ClassificationOnlineApplicable},
		{DomainBootstrapNodeMetadata, ClassificationOnlineApplicable},
		{DomainNodeIdentity, ClassificationStagedOnly},
		{DomainModulesLoad, ClassificationStagedOnly},
		{DomainMountUnits, ClassificationStagedOnly},
		{DomainExtraDisks, ClassificationStagedOnly},
		{DomainSSHOperatorAccess, ClassificationStagedOnly},
		{DomainKubeadmConfig, ClassificationStagedOnly},
		{DomainSystemRole, ClassificationRejectedLive},
		{DomainSelectedKubeadmConfig, ClassificationRejectedLive},
		{DomainSelectedKubernetesSysext, ClassificationRejectedLive},
		{DomainKubeletNodeIdentity, ClassificationRejectedLive},
		{DomainHostAccountPolicy, ClassificationRejectedLive},
		{DomainEtcKubernetes, ClassificationRejectedLive},
		{DomainArbitraryEtc, ClassificationRejectedLive},
		{DomainRootSelection, ClassificationRejectedLive},
		{DomainSysextSelection, ClassificationRejectedLive},
		{"unknown-domain", ClassificationRejectedLive},
	}
	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			if got := DomainClassification(tt.domain); got != tt.want {
				t.Fatalf("DomainClassification(%q) = %q, want %q", tt.domain, got, tt.want)
			}
		})
	}
}

func TestPlanAcceptsOnlineApplicableLiveWhenPreflightPasses(t *testing.T) {
	decision, err := Plan(generation.ApplyModeLive, []Change{
		{Domain: DomainResolved},
		{Domain: DomainSysctl},
		{Domain: DomainTmpfiles},
		{Domain: DomainNetworkd, LivePreflightOK: true},
		{Domain: DomainBootstrapNodeMetadata, LivePreflightOK: true},
	})
	if err != nil {
		t.Fatalf("Plan() error = %v, diagnostics = %#v", err, decision.Diagnostics)
	}
	if decision.AcceptedMode != generation.ApplyModeLive || len(decision.Diagnostics) != 0 {
		t.Fatalf("decision = %#v", decision)
	}
	if got, want := strings.Join(decision.ChangedDomains, ","), "resolved,sysctl,tmpfiles,networkd,bootstrap-node-metadata"; got != want {
		t.Fatalf("changed domains = %q, want %q", got, want)
	}
}

func TestPlanRejectsOnlineApplicableLiveWithoutRequiredPreflight(t *testing.T) {
	decision, err := Plan(generation.ApplyModeLive, []Change{{Domain: DomainNetworkd}})
	if err == nil {
		t.Fatalf("Plan() error = nil, decision = %#v", decision)
	}
	if len(decision.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v", decision.Diagnostics)
	}
	diagnostic := decision.Diagnostics[0]
	if diagnostic.Domain != DomainNetworkd || diagnostic.Decision != DecisionStagedRequired || diagnostic.Classification != ClassificationOnlineApplicable {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
	if !strings.Contains(diagnostic.Message, "preflight") {
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
		DomainKubeadmConfig,
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

func TestPlanRejectsLiveClusterAndOSSelectionMutations(t *testing.T) {
	for _, domain := range []string{
		DomainSystemRole,
		DomainSelectedKubeadmConfig,
		DomainSelectedKubernetesSysext,
		DomainKubeletNodeIdentity,
		DomainHostAccountPolicy,
		DomainEtcKubernetes,
		DomainArbitraryEtc,
		DomainRootSelection,
		DomainSysextSelection,
		"unknown-domain",
	} {
		t.Run(domain, func(t *testing.T) {
			decision, err := Plan(generation.ApplyModeLive, []Change{{Domain: domain}})
			if err == nil {
				t.Fatalf("Plan() error = nil, decision = %#v", decision)
			}
			if len(decision.Diagnostics) != 1 {
				t.Fatalf("diagnostics = %#v", decision.Diagnostics)
			}
			diagnostic := decision.Diagnostics[0]
			if diagnostic.Decision != DecisionRejected || diagnostic.Classification != ClassificationRejectedLive {
				t.Fatalf("diagnostic = %#v", diagnostic)
			}
		})
	}
}

func TestPlanNextBootAllowsKnownRejectedLiveSelectionChangesOnly(t *testing.T) {
	allowed := []string{
		DomainSystemRole,
		DomainSelectedKubeadmConfig,
		DomainKubeletNodeIdentity,
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
		DomainHostAccountPolicy,
		DomainSelectedKubernetesSysext,
		DomainEtcKubernetes,
		DomainArbitraryEtc,
		DomainRootSelection,
		DomainSysextSelection,
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
		{Domain: DomainResolved},
		{Domain: DomainNetworkd, LivePreflightOK: true},
		{Domain: DomainKubeadmConfig},
		{Domain: DomainEtcKubernetes},
	})
	if err == nil {
		t.Fatalf("Plan() error = nil, decision = %#v", decision)
	}
	if decision.AcceptedMode != "" {
		t.Fatalf("accepted mode = %q, want empty rejected decision", decision.AcceptedMode)
	}
	if len(decision.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %#v, want staged kubeadm and rejected /etc/kubernetes", decision.Diagnostics)
	}
	if decision.Diagnostics[0].Domain != DomainKubeadmConfig || decision.Diagnostics[0].Decision != DecisionStagedRequired {
		t.Fatalf("first diagnostic = %#v", decision.Diagnostics[0])
	}
	if decision.Diagnostics[1].Domain != DomainEtcKubernetes || decision.Diagnostics[1].Decision != DecisionRejected {
		t.Fatalf("second diagnostic = %#v", decision.Diagnostics[1])
	}
	if got, want := strings.Join(decision.ChangedDomains, ","), "resolved,networkd,kubeadm-config,etc-kubernetes"; got != want {
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
