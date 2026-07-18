package configapply

import (
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
)

func TestPlanChangeProducesLiveRecordAndStatus(t *testing.T) {
	current := currentRecord()
	result, err := PlanChange(current, NodeConfigurationChange{
		APIVersion:   generation.APIVersion,
		Kind:         NodeConfigurationChangeKind,
		GenerationID: "2026.06.05-002",
		SourceDigest: strings.Repeat("d", 64),
		Apply:        Apply{},
		Changes: []Change{
			{Domain: DomainSysctl},
		},
		GeneratedConfext: candidateConfext("2026.06.05-002"),
		RequestedAt:      time.Date(2026, 6, 5, 16, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("PlanChange() error = %v, diagnostics = %#v", err, result.Decision.Diagnostics)
	}
	if result.Decision.RequestedMode != generation.ApplyModeAuto || result.Decision.AcceptedMode != generation.ApplyModeLive || len(result.Decision.Diagnostics) != 0 {
		t.Fatalf("decision = %#v", result.Decision)
	}
	if result.GenerationRecord.ConfigApply == nil {
		t.Fatalf("runtime config metadata was not recorded: %#v", result.GenerationRecord)
	}
	metadata := result.GenerationRecord.ConfigApply
	if metadata.SourceDigest != strings.Repeat("d", 64) || metadata.PreviousGeneration != current.GenerationID {
		t.Fatalf("config apply metadata = %#v", metadata)
	}
	if metadata.RequestedApplyMode != generation.ApplyModeAuto || metadata.AcceptedApplyMode != generation.ApplyModeLive {
		t.Fatalf("config apply modes = %#v", metadata)
	}
	if got, want := strings.Join(metadata.ChangedDomains, ","), "sysctl"; got != want {
		t.Fatalf("metadata changed domains = %q, want %q", got, want)
	}
	if result.GenerationRecord.Root != current.Root || result.GenerationRecord.Boot.UKIPath != current.Boot.UKIPath || result.GenerationRecord.Boot.LoaderEntryPath != "loader/entries/katl-2026.06.05-002.conf" || result.GenerationRecord.Sysexts[0].Path != current.Sysexts[0].Path {
		t.Fatalf("runtime config record did not reuse root/UKI/sysext and select a new loader entry: %#v", result.GenerationRecord)
	}
	if result.Status.GenerationID != "2026.06.05-002" || result.Status.Phase != generation.ConfigApplyPhasePlanned {
		t.Fatalf("status = %#v", result.Status)
	}
	if len(result.Status.DomainActions) != 1 || result.Status.DomainActions[0].Action != "systemd-sysctl" || result.Status.DomainActions[0].Status != generation.ConfigApplyActionPlanned {
		t.Fatalf("domain actions = %#v", result.Status.DomainActions)
	}
}

func TestPlanChangeProducesNextBootRecordAndStatus(t *testing.T) {
	result, err := PlanChange(currentRecord(), NodeConfigurationChange{
		APIVersion:       generation.APIVersion,
		Kind:             NodeConfigurationChangeKind,
		GenerationID:     "2026.06.05-002",
		SourceDigest:     strings.Repeat("d", 64),
		Apply:            Apply{Mode: generation.ApplyModeNextBoot},
		Changes:          []Change{{Domain: DomainNodeIdentity}, {Domain: DomainModulesLoad}},
		GeneratedConfext: candidateConfext("2026.06.05-002"),
		RequestedAt:      time.Date(2026, 6, 5, 16, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("PlanChange() error = %v, diagnostics = %#v", err, result.Decision.Diagnostics)
	}
	if result.GenerationRecord.ConfigApply.AcceptedApplyMode != generation.ApplyModeNextBoot {
		t.Fatalf("config apply metadata = %#v", result.GenerationRecord.ConfigApply)
	}
	if len(result.Status.DomainActions) != 2 || result.Status.DomainActions[0].Action != "stage-next-boot" || result.Status.DomainActions[0].Status != generation.ConfigApplyActionSkipped {
		t.Fatalf("domain actions = %#v", result.Status.DomainActions)
	}
	if result.GenerationRecord.BootState != "pending" || result.GenerationRecord.HealthState != "unknown" {
		t.Fatalf("record state = %s/%s", result.GenerationRecord.BootState, result.GenerationRecord.HealthState)
	}
}

func TestPlanChangeRejectsUnsupportedLiveChangeBeforeCandidateRecord(t *testing.T) {
	result, err := PlanChange(currentRecord(), NodeConfigurationChange{
		APIVersion:   generation.APIVersion,
		Kind:         NodeConfigurationChangeKind,
		GenerationID: "2026.06.05-002",
		SourceDigest: strings.Repeat("d", 64),
		Apply:        Apply{Mode: generation.ApplyModeLive},
		Changes: []Change{
			{Domain: DomainResolved},
			{Domain: DomainKubeadmConfig},
			{Domain: DomainEtcKubernetes},
		},
		GeneratedConfext: generation.GeneratedConfext{},
	})
	if err == nil {
		t.Fatalf("PlanChange() error = nil, result = %#v", result)
	}
	if result.Decision.AcceptedMode != "" || len(result.Decision.Diagnostics) != 3 {
		t.Fatalf("decision = %#v", result.Decision)
	}
	if result.GenerationRecord.Kind != "" || result.Status.Kind != "" {
		t.Fatalf("rejected plan constructed candidate metadata or status: %#v %#v", result.GenerationRecord, result.Status)
	}
	if !strings.Contains(err.Error(), "live request rejected") {
		t.Fatalf("error = %q, want live rejection", err)
	}
}

func TestPlanChangeStagesKubeadmInputWithoutLiveAction(t *testing.T) {
	live, err := PlanChange(currentRecord(), NodeConfigurationChange{
		APIVersion:       generation.APIVersion,
		Kind:             NodeConfigurationChangeKind,
		GenerationID:     "2026.06.05-002",
		SourceDigest:     strings.Repeat("d", 64),
		Apply:            Apply{Mode: generation.ApplyModeLive},
		Changes:          []Change{{Domain: DomainKubeadmConfig}},
		GeneratedConfext: candidateConfext("2026.06.05-002"),
	})
	if err == nil {
		t.Fatalf("PlanChange(live kubeadm) error = nil, result = %#v", live)
	}
	if len(live.Decision.Diagnostics) != 1 || live.Decision.Diagnostics[0].Decision != DecisionRejected || live.Decision.Diagnostics[0].RequiredOperation != "kubeadm-aware operation" {
		t.Fatalf("live kubeadm diagnostics = %#v", live.Decision.Diagnostics)
	}

	next, err := PlanChange(currentRecord(), NodeConfigurationChange{
		APIVersion:       generation.APIVersion,
		Kind:             NodeConfigurationChangeKind,
		GenerationID:     "2026.06.05-002",
		SourceDigest:     strings.Repeat("d", 64),
		Apply:            Apply{Mode: generation.ApplyModeNextBoot},
		Changes:          []Change{{Domain: DomainKubeadmConfig}},
		GeneratedConfext: candidateConfext("2026.06.05-002"),
		Kubeadm: generation.KubeadmActionRequired{
			Required:           true,
			PreviousConfigName: "control-plane",
			SelectedConfigName: "control-plane",
			Reason:             "desired kubeadm input changed; join token abcdef.0123456789abcdef requires explicit action",
		},
	})
	if err != nil {
		t.Fatalf("PlanChange(next kubeadm) error = %v, result = %#v", err, next)
	}
	if next.Decision.AcceptedMode != generation.ApplyModeNextBoot || len(next.Decision.Diagnostics) != 1 || next.Decision.Diagnostics[0].Decision != DecisionActionRequired {
		t.Fatalf("next kubeadm decision = %#v", next.Decision)
	}
	if !next.Status.Kubeadm.Required || next.Status.Kubeadm.SelectedConfigName != "control-plane" || next.Status.PreviousGeneration == "" {
		t.Fatalf("next kubeadm status = %#v", next.Status)
	}
	if len(next.Status.DomainActions) != 1 || next.Status.DomainActions[0].Action != "kubeadm-operation-required" || next.Status.DomainActions[0].Status != generation.ConfigApplyActionSkipped {
		t.Fatalf("next kubeadm actions = %#v", next.Status.DomainActions)
	}
}

func TestPlanChangeAllowsUnchangedKubernetesSysext(t *testing.T) {
	current := currentRecord()
	result, err := PlanChange(current, NodeConfigurationChange{
		APIVersion:       generation.APIVersion,
		Kind:             NodeConfigurationChangeKind,
		GenerationID:     "2026.06.05-002",
		SourceDigest:     strings.Repeat("d", 64),
		Apply:            Apply{Mode: generation.ApplyModeNextBoot},
		Changes:          []Change{{Domain: DomainModulesLoad}},
		Sysexts:          append([]generation.ExtensionRef{}, current.Sysexts...),
		GeneratedConfext: candidateConfext("2026.06.05-002"),
		RequestedAt:      time.Date(2026, 6, 5, 16, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("PlanChange() error = %v, diagnostics = %#v", err, result.Decision.Diagnostics)
	}
	if result.GenerationRecord.ConfigApply == nil || result.Status.Kind == "" {
		t.Fatalf("unchanged sysext plan did not produce candidate record/status: %#v %#v", result.GenerationRecord, result.Status)
	}
	if result.GenerationRecord.Sysexts[0].SHA256 != current.Sysexts[0].SHA256 || result.GenerationRecord.Sysexts[0].PayloadVersion != current.Sysexts[0].PayloadVersion {
		t.Fatalf("generation sysext = %#v, want current %#v", result.GenerationRecord.Sysexts[0], current.Sysexts[0])
	}
}

func TestPlanChangeRejectsKubernetesSysextChangeBeforeCandidateRecord(t *testing.T) {
	current := currentRecord()
	nextSysexts := append([]generation.ExtensionRef{}, current.Sysexts...)
	nextSysexts[0].SHA256 = strings.Repeat("e", 64)
	nextSysexts[0].PayloadVersion = "v1.37.0"

	result, err := PlanChange(current, NodeConfigurationChange{
		APIVersion:       generation.APIVersion,
		Kind:             NodeConfigurationChangeKind,
		GenerationID:     "2026.06.05-002",
		SourceDigest:     strings.Repeat("d", 64),
		Apply:            Apply{Mode: generation.ApplyModeNextBoot},
		Changes:          []Change{{Domain: DomainModulesLoad}},
		Sysexts:          nextSysexts,
		GeneratedConfext: candidateConfext("2026.06.05-002"),
		RequestedAt:      time.Date(2026, 6, 5, 16, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatalf("PlanChange() error = nil, result = %#v", result)
	}
	if result.GenerationRecord.Kind != "" || result.Status.Kind != "" {
		t.Fatalf("rejected sysext change built candidate metadata or status: %#v %#v", result.GenerationRecord, result.Status)
	}
	if len(result.Decision.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want one selected-kubernetes-sysext diagnostic", result.Decision.Diagnostics)
	}
	diagnostic := result.Decision.Diagnostics[0]
	if diagnostic.Domain != DomainSelectedKubernetesSysext || diagnostic.Decision != DecisionRejected || diagnostic.Classification != ClassificationOperationOnly || diagnostic.RequiredOperation != "kubernetes-upgrade" {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
	if !strings.Contains(diagnostic.Message, "target kubeadm access") || !strings.Contains(diagnostic.Message, "kubelet activation gate") {
		t.Fatalf("diagnostic message = %q, want missing upgrade gates", diagnostic.Message)
	}
}

func TestPlanChangeRejectsBadEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(NodeConfigurationChange) NodeConfigurationChange
		wantErr string
	}{
		{
			name: "api version",
			mutate: func(request NodeConfigurationChange) NodeConfigurationChange {
				request.APIVersion = "katl.dev/v1beta1"
				return request
			},
			wantErr: "apiVersion",
		},
		{
			name: "kind",
			mutate: func(request NodeConfigurationChange) NodeConfigurationChange {
				request.Kind = "ConfigMap"
				return request
			},
			wantErr: "kind",
		},
		{
			name: "apply mode",
			mutate: func(request NodeConfigurationChange) NodeConfigurationChange {
				request.Apply.Mode = "immediate"
				return request
			},
			wantErr: "apply mode",
		},
		{
			name: "source digest",
			mutate: func(request NodeConfigurationChange) NodeConfigurationChange {
				request.SourceDigest = ""
				return request
			},
			wantErr: "source digest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := NodeConfigurationChange{
				APIVersion:       generation.APIVersion,
				Kind:             NodeConfigurationChangeKind,
				GenerationID:     "2026.06.05-002",
				SourceDigest:     strings.Repeat("d", 64),
				Apply:            Apply{Mode: generation.ApplyModeLive},
				Changes:          []Change{{Domain: DomainTmpfiles}},
				GeneratedConfext: candidateConfext("2026.06.05-002"),
			}
			_, err := PlanChange(currentRecord(), tt.mutate(request))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("PlanChange() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func currentRecord() generation.Record {
	return generation.Record{
		APIVersion:     generation.APIVersion,
		Kind:           generation.Kind,
		GenerationID:   "2026.06.05-001",
		RuntimeVersion: "0.1.0",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeVersion:        "0.1.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: strings.Repeat("a", 64),
		},
		Boot: generation.BootSelection{UKIPath: "/efi/EFI/Linux/katl-2026.06.05-001.efi"},
		Sysexts: []generation.ExtensionRef{{
			Name:            "kubernetes",
			Path:            "/var/lib/katl/generations/2026.06.05-001/sysext/kubernetes.raw",
			ActivationPath:  "/run/extensions/kubernetes.raw",
			SHA256:          strings.Repeat("b", 64),
			ArtifactVersion: "k8s-v1.36.1",
			PayloadVersion:  "v1.36.1",
			Architecture:    "x86_64",
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: []string{"katl-runtime-1"},
			},
		}},
		Confexts: []generation.GeneratedConfext{{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/2026.06.05-001/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("c", 64),
			Compatibility: generation.ConfextCompatibility{
				ID:           "katlos",
				VersionID:    "0.1.0",
				ConfextLevel: 1,
			},
		}},
		KernelCommandLine: []string{"root=PARTUUID=11111111-2222-3333-4444-555555555555", "rootfstype=squashfs", "ro"},
		CreatedAt:         time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC),
		BootState:         "good",
		HealthState:       "healthy",
	}
}

func candidateConfext(id string) generation.GeneratedConfext {
	return generation.GeneratedConfext{
		Name:           "katl-node",
		Path:           "/var/lib/katl/generations/" + id + "/confext",
		ActivationPath: "/run/confexts/katl-node",
		SHA256:         strings.Repeat("d", 64),
		Compatibility: generation.ConfextCompatibility{
			ID:           "katlos",
			VersionID:    "0.1.0",
			ConfextLevel: 1,
		},
	}
}
