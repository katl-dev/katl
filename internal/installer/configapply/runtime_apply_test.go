package configapply

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/confext"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/kubeadmconfig"
	"github.com/zariel/katl/internal/installer/manifest"
)

func TestApplyTrustedBundleRendersAndExecutesLiveNetworkd(t *testing.T) {
	root := t.TempDir()
	activator := &fakeActivator{}
	runner := &fakeCommandRunner{}
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeLive,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "20-uplink.network",
				Content: "[Match]\nName=ens3\n[Network]\nDHCP=yes\n",
			}}},
			LivePreflight: map[string]bool{DomainNetworkd: true},
		},
		Executor: &Executor{Runner: runner, Activator: activator, Now: fixedNow},
	}))
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	if result.Plan.Decision.AcceptedMode != generation.ApplyModeLive || result.Status.Phase != generation.ConfigApplyPhaseActive {
		t.Fatalf("plan/status = %#v %#v", result.Plan.Decision, result.Status)
	}
	if activator.activated != "2026.06.05-002" {
		t.Fatalf("activated generation = %q", activator.activated)
	}
	networkdPath := filepath.Join(result.Tree.ConfextDir, "etc/systemd/network/20-uplink.network")
	data, err := os.ReadFile(networkdPath)
	if err != nil {
		t.Fatalf("read networkd file: %v", err)
	}
	if !strings.Contains(string(data), "DHCP=yes") {
		t.Fatalf("networkd content = %q", data)
	}
	persisted, err := generation.ReadConfigApplyStatus(result.StatusPath)
	if err != nil {
		t.Fatalf("ReadConfigApplyStatus() error = %v", err)
	}
	if persisted.Phase != generation.ConfigApplyPhaseActive {
		t.Fatalf("persisted phase = %q", persisted.Phase)
	}
	if result.Audit.SourceID != "operator" || result.Audit.DesiredVersion != "2" || result.Audit.Decision != DecisionAccepted {
		t.Fatalf("audit = %#v", result.Audit)
	}
}

func TestApplyTrustedBundleResolvesRoleAndNodeOverlaysForNextBoot(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "10-common.network",
				Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
			}}},
		},
		SystemRoleOverrides: map[string]NodeOverlay{
			"control-plane": {
				Identity: &IdentityOverlay{AuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestRoleKey katl"}},
			},
		},
		NodeOverrides: map[string]NodeOverlay{
			"cp-1": {
				SystemRole: "worker",
				Identity:   &IdentityOverlay{Hostname: "worker-1"},
			},
		},
	}))
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	if result.Manifest.Node.SystemRole != "worker" || result.Manifest.Node.Identity.Hostname != "worker-1" {
		t.Fatalf("merged node = %#v", result.Manifest.Node)
	}
	for _, domain := range []string{DomainNetworkd, DomainSSHOperatorAccess, DomainSystemRole, DomainNodeIdentity} {
		if !containsDomain(result.Plan.Decision.ChangedDomains, domain) {
			t.Fatalf("changed domains = %#v, missing %s", result.Plan.Decision.ChangedDomains, domain)
		}
	}
	if result.Status.Phase != generation.ConfigApplyPhaseNextBoot {
		t.Fatalf("status phase = %q", result.Status.Phase)
	}
}

func TestApplyTrustedBundleStagesKubeadmDesiredInputWithoutClusterMutation(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			KubeadmChanged: true,
		},
		CurrentManifest: manifestWithKubeadm(),
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane": kubeadmPlan("control-plane"),
		},
	}))
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	kubeadmPath := filepath.Join(result.Tree.ConfextDir, "etc/katl/kubeadm/control-plane/config.yaml")
	data, err := os.ReadFile(kubeadmPath)
	if err != nil {
		t.Fatalf("read kubeadm desired input: %v", err)
	}
	if !strings.Contains(string(data), "InitConfiguration") {
		t.Fatalf("kubeadm content = %q", data)
	}
	if !result.Plan.GenerationRecord.ConfigApply.Kubeadm.Required || !result.Status.Kubeadm.Required {
		t.Fatalf("kubeadm action required missing: %#v %#v", result.Plan.GenerationRecord.ConfigApply.Kubeadm, result.Status.Kubeadm)
	}
	if result.Plan.Decision.AcceptedMode != generation.ApplyModeNextBoot {
		t.Fatalf("decision = %#v", result.Plan.Decision)
	}
}

func TestApplyTrustedBundleReplaysSameRequestWithoutRenderingAgain(t *testing.T) {
	root := t.TempDir()
	request := trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "10-common.network",
				Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
			}}},
		},
	})
	first, err := ApplyTrustedBundle(context.Background(), request)
	if err != nil {
		t.Fatalf("first ApplyTrustedBundle() error = %v", err)
	}
	replay, err := ApplyTrustedBundle(context.Background(), request)
	if err != nil {
		t.Fatalf("replay ApplyTrustedBundle() error = %v", err)
	}
	if replay.AuditPath != first.AuditPath || replay.Audit.RequestDigest != first.Audit.RequestDigest {
		t.Fatalf("replay audit = %#v, want %#v", replay.Audit, first.Audit)
	}
	if replay.MetadataPath != first.MetadataPath || replay.StatusPath != first.StatusPath {
		t.Fatalf("replay paths = %s %s, want %s %s", replay.MetadataPath, replay.StatusPath, first.MetadataPath, first.StatusPath)
	}
	if replay.Plan.GenerationRecord.GenerationID != first.Plan.GenerationRecord.GenerationID || replay.Status.GenerationID != first.Status.GenerationID {
		t.Fatalf("replay record/status = %#v %#v", replay.Plan.GenerationRecord, replay.Status)
	}
	if replay.Tree.ConfextDir != "" {
		t.Fatalf("replay rendered a new tree: %#v", replay.Tree)
	}
}

func TestApplyTrustedBundleRejectsSameVersionDigestConflictBeforeRender(t *testing.T) {
	root := t.TempDir()
	first, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "10-common.network",
				Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
			}}},
		},
	}))
	if err != nil {
		t.Fatalf("first ApplyTrustedBundle() error = %v", err)
	}
	conflict, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-003",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "10-common.network",
				Content: "[Match]\nName=*\n[Network]\nAddress=192.0.2.10/24\n",
			}}},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "different request digest") {
		t.Fatalf("conflict ApplyTrustedBundle() error = %v, result = %#v", err, conflict)
	}
	if conflict.AuditPath != first.AuditPath || conflict.Audit.RequestDigest != first.Audit.RequestDigest {
		t.Fatalf("conflict audit = %#v, want existing audit %#v", conflict.Audit, first.Audit)
	}
	if _, statErr := os.Stat(filepath.Join(root, "var/lib/katl/generations/2026.06.05-003")); !os.IsNotExist(statErr) {
		t.Fatalf("conflicting request created generation dir: %v", statErr)
	}
}

func TestApplyTrustedBundleRejectsStaleVersionBeforeRender(t *testing.T) {
	root := t.TempDir()
	_, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		DesiredVersion: "10",
		ApplyMode:      generation.ApplyModeNextBoot,
		GenerationID:   "2026.06.05-010",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "10-common.network",
				Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
			}}},
		},
	}))
	if err != nil {
		t.Fatalf("newer ApplyTrustedBundle() error = %v", err)
	}
	stale, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		DesiredVersion: "2",
		ApplyMode:      generation.ApplyModeNextBoot,
		GenerationID:   "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "10-common.network",
				Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
			}}},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "older than recorded version 10") {
		t.Fatalf("stale ApplyTrustedBundle() error = %v, result = %#v", err, stale)
	}
	if stale.Audit.Decision != DecisionRejected || stale.Audit.FailureReason == "" {
		t.Fatalf("stale audit = %#v", stale.Audit)
	}
	if _, statErr := os.Stat(filepath.Join(root, "var/lib/katl/generations/2026.06.05-002")); !os.IsNotExist(statErr) {
		t.Fatalf("stale request created generation dir: %v", statErr)
	}
}

func TestApplyTrustedBundleRejectsRuntimeSelectionOverridesBeforeRender(t *testing.T) {
	tests := []struct {
		name       string
		override   TrustedBundleRequest
		wantReason string
	}{
		{
			name:       "kubernetes version",
			override:   TrustedBundleRequest{KubernetesVersion: "v1.37.0"},
			wantReason: "sysext version selection",
		},
		{
			name:       "activation path",
			override:   TrustedBundleRequest{KubernetesActivationPath: "/run/extensions/katl-kubernetes.raw"},
			wantReason: "raw Kubernetes sysext activation paths",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			override := tt.override
			override.ApplyMode = generation.ApplyModeNextBoot
			override.GenerationID = "2026.06.05-002"
			override.ClusterDefaults = NodeOverlay{
				Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
					Name:    "10-common.network",
					Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
				}}},
			}
			result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, override))
			if err == nil || !strings.Contains(err.Error(), tt.wantReason) {
				t.Fatalf("ApplyTrustedBundle() error = %v, want %q; result = %#v", err, tt.wantReason, result)
			}
			if result.Audit.Decision != DecisionRejected || result.Audit.FailureReason == "" {
				t.Fatalf("audit = %#v", result.Audit)
			}
			if _, statErr := os.Stat(filepath.Join(root, "var/lib/katl/generations/2026.06.05-002")); !os.IsNotExist(statErr) {
				t.Fatalf("selection override created generation dir: %v", statErr)
			}
		})
	}
}

func TestApplyTrustedBundleRejectsRuntimeSelectionOverrideReplay(t *testing.T) {
	root := t.TempDir()
	request := trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:         generation.ApplyModeNextBoot,
		GenerationID:      "2026.06.05-002",
		KubernetesVersion: "v1.37.0",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "10-common.network",
				Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
			}}},
		},
	})
	audit := request.audit("operator", "2", DecisionAccepted, []Change{{Domain: DomainNetworkd}}, nil, nil, fixedNow())
	if _, err := writeAudit(root, "operator", "2", audit); err != nil {
		t.Fatalf("writeAudit() error = %v", err)
	}
	result, err := ApplyTrustedBundle(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "sysext version selection") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want sysext rejection; result = %#v", err, result)
	}
	if result.Audit.Decision != DecisionRejected {
		t.Fatalf("audit = %#v", result.Audit)
	}
	if _, statErr := os.Stat(filepath.Join(root, "var/lib/katl/generations/2026.06.05-002")); !os.IsNotExist(statErr) {
		t.Fatalf("selection override replay created generation dir: %v", statErr)
	}
}

func TestApplyTrustedBundleRejectsLeadingZeroDesiredVersion(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		DesiredVersion: "02",
		ApplyMode:      generation.ApplyModeNextBoot,
		GenerationID:   "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "10-common.network",
				Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
			}}},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "leading zeroes") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want leading-zero rejection; result = %#v", err, result)
	}
	if _, statErr := os.Stat(filepath.Join(root, "var/lib/katl/generations/2026.06.05-002")); !os.IsNotExist(statErr) {
		t.Fatalf("leading-zero request created generation dir: %v", statErr)
	}
}

func TestApplyTrustedBundleRejectsNoSupportedDomainsBeforeRender(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
	}))
	if err == nil || !strings.Contains(err.Error(), "no supported changed domains") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want no-domain rejection; result = %#v", err, result)
	}
	if result.Audit.Decision != DecisionRejected || result.Audit.FailureReason == "" {
		t.Fatalf("audit = %#v", result.Audit)
	}
	assertGenerationMissing(t, root, "2026.06.05-002")
}

func TestApplyTrustedBundleRejectsWhenRejectionAuditCannotPersist(t *testing.T) {
	root := t.TempDir()
	auditDir := filepath.Join(root, "var/lib/katl/config-requests/operator")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.Chmod(auditDir, 0o555); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	defer func() {
		if err := os.Chmod(auditDir, 0o755); err != nil {
			t.Fatalf("restore audit dir mode: %v", err)
		}
	}()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
	}))
	if err == nil {
		t.Fatalf("ApplyTrustedBundle() error = nil, result = %#v", result)
	}
	if !strings.Contains(err.Error(), "no supported changed domains") || !strings.Contains(err.Error(), "write config request audit") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want rejection and audit persistence failure", err)
	}
	if result.Audit.Decision != DecisionRejected || result.Audit.FailureReason == "" {
		t.Fatalf("audit = %#v", result.Audit)
	}
	assertGenerationMissing(t, root, "2026.06.05-002")
}

func TestApplyTrustedBundleRejectsUnsupportedApplyModeBeforeRender(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    "immediate",
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "10-common.network",
				Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
			}}},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "apply mode") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want apply-mode rejection; result = %#v", err, result)
	}
	if result.Audit.Decision != DecisionRejected || result.Audit.FailureReason == "" {
		t.Fatalf("audit = %#v", result.Audit)
	}
	assertGenerationMissing(t, root, "2026.06.05-002")
}

func TestApplyTrustedBundleRejectsUnsupportedKnownDomainFieldsBeforeRender(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "bad name.network",
				Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
			}}},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "networkd") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want known-domain field rejection; result = %#v", err, result)
	}
	if result.Audit.Decision != DecisionRejected || !containsDomain(result.Audit.ChangedDomains, DomainNetworkd) {
		t.Fatalf("audit = %#v", result.Audit)
	}
	assertGenerationMissing(t, root, "2026.06.05-002")
}

func TestApplyTrustedBundleRejectsRawEtcInputBeforeRender(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			UnsafeEtcFiles: confextFiles{{Path: "/etc/motd", Content: "raw\n"}}.native(),
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want arbitrary /etc rejection; result = %#v", err, result)
	}
	if result.Audit.Decision != DecisionRejected || !containsDomain(result.Audit.ChangedDomains, DomainArbitraryEtc) {
		t.Fatalf("audit = %#v", result.Audit)
	}
	assertGenerationMissing(t, root, "2026.06.05-002")
}

func TestApplyTrustedBundleRejectsUnsafeKubeadmRenderPathBeforeRender(t *testing.T) {
	root := t.TempDir()
	plan := kubeadmPlan("control-plane")
	plan.Config.RenderPath = "/etc/kubernetes/admin.conf"
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			KubeadmChanged: true,
		},
		CurrentManifest: manifestWithKubeadm(),
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane": plan,
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "/etc/kubernetes") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want unsafe kubeadm render path rejection; result = %#v", err, result)
	}
	if result.Audit.Decision != DecisionRejected || !containsDomain(result.Audit.ChangedDomains, DomainKubeadmConfig) {
		t.Fatalf("audit = %#v", result.Audit)
	}
	assertGenerationMissing(t, root, "2026.06.05-002")
}

func TestApplyTrustedBundleRejectsUnsafeEtcPathsBeforeRender(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			UnsafeEtcFiles: confextFiles{{Path: "/etc/kubernetes/admin.conf", Content: "secret\n"}}.native(),
		},
	}))
	if err == nil {
		t.Fatalf("ApplyTrustedBundle() error = nil, result = %#v", result)
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("error = %q, want rejection", err)
	}
	if result.Tree.ConfextDir != "" {
		t.Fatalf("unsafe request rendered tree: %#v", result.Tree)
	}
	if result.Audit.Decision != DecisionRejected || !containsDomain(result.Audit.ChangedDomains, DomainArbitraryEtc) {
		t.Fatalf("audit = %#v", result.Audit)
	}
	assertGenerationMissing(t, root, "2026.06.05-002")
}

func TestApplyTrustedBundleRecordsRollbackStatusOnActivationFailure(t *testing.T) {
	root := t.TempDir()
	activator := &fakeActivator{activateErr: os.ErrPermission}
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeLive,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "20-uplink.network",
				Content: "[Match]\nName=ens3\n[Network]\nDHCP=yes\n",
			}}},
			LivePreflight: map[string]bool{DomainNetworkd: true},
		},
		Executor: &Executor{Runner: &fakeCommandRunner{}, Activator: activator, Now: fixedNow},
	}))
	if err == nil {
		t.Fatalf("ApplyTrustedBundle() error = nil, result = %#v", result)
	}
	if result.Status.Phase != generation.ConfigApplyPhaseRolledBack || result.Status.Rollback == nil {
		t.Fatalf("status = %#v, want rollback status", result.Status)
	}
	if result.Status.Rollback.TargetGenerationID != "2026.06.05-001" {
		t.Fatalf("rollback target = %#v", result.Status.Rollback)
	}
	persisted, err := generation.ReadConfigApplyStatus(result.StatusPath)
	if err != nil {
		t.Fatalf("ReadConfigApplyStatus() error = %v", err)
	}
	if persisted.Phase != generation.ConfigApplyPhaseRolledBack {
		t.Fatalf("persisted phase = %q", persisted.Phase)
	}
}

func trustedBundleRequest(root string, override TrustedBundleRequest) TrustedBundleRequest {
	request := TrustedBundleRequest{
		Root:            root,
		SourceID:        "operator",
		DesiredVersion:  "2",
		NodeName:        "cp-1",
		ApplyMode:       generation.ApplyModeNextBoot,
		GenerationID:    "2026.06.05-002",
		CurrentManifest: baseManifest(),
		CurrentRecord:   currentRecord(),
		Chown:           func(string, int, int) error { return nil },
		Now:             fixedNow,
	}
	if override.ApplyMode != "" {
		request.ApplyMode = override.ApplyMode
	}
	if override.GenerationID != "" {
		request.GenerationID = override.GenerationID
	}
	if override.SourceID != "" {
		request.SourceID = override.SourceID
	}
	if override.DesiredVersion != "" {
		request.DesiredVersion = override.DesiredVersion
	}
	if override.NodeName != "" {
		request.NodeName = override.NodeName
	}
	if override.CurrentManifest.Kind != "" {
		request.CurrentManifest = override.CurrentManifest
	}
	if override.CurrentRecord.Kind != "" {
		request.CurrentRecord = override.CurrentRecord
	}
	request.ClusterDefaults = override.ClusterDefaults
	request.SystemRoleOverrides = override.SystemRoleOverrides
	request.NodeOverrides = override.NodeOverrides
	request.KubeadmConfigs = override.KubeadmConfigs
	request.KubernetesVersion = override.KubernetesVersion
	request.KubernetesActivationPath = override.KubernetesActivationPath
	request.Executor = override.Executor
	if override.Chown != nil {
		request.Chown = override.Chown
	}
	if override.Now != nil {
		request.Now = override.Now
	}
	return request
}

func baseManifest() manifest.Manifest {
	return manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		Node: manifest.NodeConfig{
			Identity: manifest.NodeIdentity{
				Hostname: "cp-1",
				SSH: manifest.SSHIdentity{
					AuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestBaseKey katl"},
				},
			},
			SystemRole: "control-plane",
		},
		Install: manifest.InstallConfig{
			AllowDestructiveInstall: true,
			TargetDisk:              manifest.DiskSelector{ByID: "disk/by-id/test"},
		},
		KatlosImage: manifest.KatlosImage{
			LocalRef:     "images/katlos.raw",
			SHA256:       strings.Repeat("a", 64),
			SizeBytes:    1024,
			Version:      "0.1.0",
			Architecture: "x86_64",
			Role:         "install",
		},
	}
}

func manifestWithKubeadm() manifest.Manifest {
	m := baseManifest()
	m.Node.Kubernetes.Kubeadm.ConfigRef = "control-plane"
	return m
}

func kubeadmPlan(name string) kubeadmconfig.Plan {
	return kubeadmconfig.Plan{
		Name: name,
		Config: kubeadmconfig.File{
			RenderPath: "/etc/katl/kubeadm/" + name + "/config.yaml",
			Content: []byte(strings.Join([]string{
				"apiVersion: kubeadm.k8s.io/v1beta4",
				"kind: InitConfiguration",
				"---",
				"apiVersion: kubeadm.k8s.io/v1beta4",
				"kind: ClusterConfiguration",
				"kubernetesVersion: v1.36.1",
				"",
			}, "\n")),
			Mode: 0o644,
		},
		Documents: []kubeadmconfig.Document{
			{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "InitConfiguration"},
			{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "ClusterConfiguration", KubernetesVersion: "v1.36.1"},
		},
	}
}

func containsDomain(domains []string, want string) bool {
	for _, domain := range domains {
		if domain == want {
			return true
		}
	}
	return false
}

func assertGenerationMissing(t *testing.T, root string, generationID string) {
	t.Helper()
	if _, statErr := os.Stat(filepath.Join(root, "var/lib/katl/generations", generationID)); !os.IsNotExist(statErr) {
		t.Fatalf("request created generation dir %s: %v", generationID, statErr)
	}
}

type confextFile struct {
	Path    string
	Content string
}

type confextFiles []confextFile

func (files confextFiles) native() []confext.NativeEtcFile {
	out := make([]confext.NativeEtcFile, 0, len(files))
	for _, file := range files {
		out = append(out, confext.NativeEtcFile{Path: file.Path, Content: file.Content})
	}
	return out
}
