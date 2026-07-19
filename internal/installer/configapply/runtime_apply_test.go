package configapply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
)

func TestApplyTrustedBundleRejectsLiveNetworkdBeforeRender(t *testing.T) {
	root := t.TempDir()
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
	}))
	if err == nil || !strings.Contains(err.Error(), "live request rejected") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want live networkd rejection; result = %#v", err, result)
	}
	if result.Audit.SourceID != "operator" || result.Audit.DesiredVersion != "2" || result.Audit.Decision != DecisionRejected {
		t.Fatalf("audit = %#v", result.Audit)
	}
	assertGenerationMissing(t, root, "2026.06.05-002")
}

func TestControlPlaneEndpointApplyClassification(t *testing.T) {
	tests := []struct {
		name        string
		initialized bool
		current     *controlplaneendpoint.Config
		desired     *controlplaneendpoint.Config
		wantDomain  string
		wantMode    string
		wantError   string
	}{
		{
			name:       "routing changes apply live",
			current:    managedEndpoint("192.0.2.1"),
			desired:    managedEndpoint("192.0.2.2"),
			wantDomain: DomainControlPlaneEndpointRouting,
			wantMode:   generation.ApplyModeLive,
		},
		{
			name:        "initialized host is immutable",
			initialized: true,
			current:     managedEndpoint("192.0.2.1"),
			desired: func() *controlplaneendpoint.Config {
				endpoint := managedEndpoint("192.0.2.1")
				endpoint.Host = "192.0.2.11"
				endpoint.Advertisement.VIP = "192.0.2.11"
				return endpoint
			}(),
			wantDomain: DomainControlPlaneEndpointIdentity,
			wantError:  "control-plane-endpoint-migration",
		},
		{
			name:        "initialized ownership is immutable",
			initialized: true,
			current:     managedEndpoint("192.0.2.1"),
			desired:     nil,
			wantDomain:  DomainControlPlaneEndpointIdentity,
			wantError:   "control-plane-endpoint-migration",
		},
		{
			name:    "pre-bootstrap identity stages for next boot",
			current: managedEndpoint("192.0.2.1"),
			desired: func() *controlplaneendpoint.Config {
				endpoint := managedEndpoint("192.0.2.1")
				endpoint.Host = "192.0.2.11"
				endpoint.Advertisement.VIP = "192.0.2.11"
				return endpoint
			}(),
			wantDomain: DomainControlPlaneEndpointBootstrap,
			wantMode:   generation.ApplyModeNextBoot,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := baseManifest()
			current.Node.ControlPlaneEndpoint = tt.current
			request := trustedBundleRequest(t.TempDir(), TrustedBundleRequest{
				ApplyMode:             generation.ApplyModeAuto,
				CurrentManifest:       current,
				KubernetesInitialized: tt.initialized,
				NodeOverrides: map[string]NodeOverlay{
					"cp-1": {ControlPlaneEndpointSet: true, ControlPlaneEndpoint: tt.desired},
				},
			})
			_, changes, _, err := mergeRuntimeConfig(request)
			if err != nil {
				t.Fatalf("mergeRuntimeConfig() error = %v", err)
			}
			if len(changes) != 1 || changes[0].Domain != tt.wantDomain {
				t.Fatalf("changes = %#v, want %q", changes, tt.wantDomain)
			}
			decision, err := Plan(request.ApplyMode, changes)
			if tt.wantError != "" {
				if err == nil || len(decision.Diagnostics) != 1 || !strings.Contains(decision.Diagnostics[0].RequiredOperation, tt.wantError) {
					t.Fatalf("Plan() error = %v, decision = %#v", err, decision)
				}
				return
			}
			if err != nil || decision.AcceptedMode != tt.wantMode {
				t.Fatalf("Plan() error = %v, decision = %#v, want mode %q", err, decision, tt.wantMode)
			}
			if tt.wantDomain == DomainControlPlaneEndpointRouting {
				if len(decision.Diagnostics) != 1 || decision.Diagnostics[0].EndpointRouting == nil {
					t.Fatalf("routing decision = %#v, want bounded impact", decision)
				}
				impact := decision.Diagnostics[0].EndpointRouting
				if got, want := strings.Join(impact.FabricSessionsReset, ","), "192.0.2.1,192.0.2.2"; got != want {
					t.Fatalf("fabric session resets = %q, want %q", got, want)
				}
				if !impact.MayLoseAllFabricPaths {
					t.Fatal("routing plan did not disclose temporary local API route withdrawal")
				}
			}
		})
	}
}

func TestEndpointRoutingImpactNamesExchangeAndExportChanges(t *testing.T) {
	before := managedEndpoint("192.0.2.1")
	before.Advertisement.BGP.RouteExchange = []controlplaneendpoint.RouteExchange{{
		Name: "cilium", ListenPort: 179, PeerASN: 64512,
		ExportToFabric: []controlplaneendpoint.PrefixEnvelope{{CIDR: "10.50.0.0/16"}},
	}}
	after := managedEndpoint("192.0.2.1")
	after.Advertisement.BGP.RouteExchange = []controlplaneendpoint.RouteExchange{{
		Name: "cilium", ListenPort: 1179, PeerASN: 64513,
		ExportToFabric: []controlplaneendpoint.PrefixEnvelope{{CIDR: "10.60.0.0/16"}},
	}}
	current, err := controlplaneendpoint.Normalize(*before)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := controlplaneendpoint.Normalize(*after)
	if err != nil {
		t.Fatal(err)
	}
	impact := endpointRoutingImpact(current.Config, desired.Config)
	if got, want := strings.Join(impact.RouteExchangeSessionsReset, ","), "cilium"; got != want {
		t.Fatalf("route exchange resets = %q, want %q", got, want)
	}
	if got, want := strings.Join(impact.ChangedExportUnions, ","), "cilium"; got != want {
		t.Fatalf("changed export unions = %q, want %q", got, want)
	}
	if len(impact.FabricSessionsReset) != 0 {
		t.Fatalf("unchanged fabric sessions reset = %#v", impact.FabricSessionsReset)
	}
}

func TestApplyTrustedBundleReloadsEnabledEndpointRouting(t *testing.T) {
	root := t.TempDir()
	current := baseManifest()
	current.Node.ControlPlaneEndpoint = managedEndpoint("192.0.2.1")
	currentRecord := currentRecord()
	selectEndpointAdvertiser(t, root, &currentRecord)
	runner := &fakeCommandRunner{}
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:       generation.ApplyModeLive,
		CurrentManifest: current,
		CurrentRecord:   currentRecord,
		NodeOverrides: map[string]NodeOverlay{
			"cp-1": {ControlPlaneEndpointSet: true, ControlPlaneEndpoint: managedEndpoint("192.0.2.2")},
		},
		Executor: &Executor{Runner: runner, Activator: &fakeActivator{}, Now: fixedNow},
	}))
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	if result.Plan.Decision.AcceptedMode != generation.ApplyModeLive || !containsDomain(result.Plan.Decision.ChangedDomains, DomainControlPlaneEndpointRouting) {
		t.Fatalf("decision = %#v", result.Plan.Decision)
	}
	if _, err := os.Stat(filepath.Join(result.Tree.ConfextDir, "etc/katl/apps/bgp-api-vip/advertisement-enabled")); err != nil {
		t.Fatalf("candidate advertisement marker: %v", err)
	}
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-daemon-reload,endpoint-routing-validate,endpoint-withdraw,endpoint-routing-reload,endpoint-resume"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestApplyTrustedBundleRemovesEndpointAdvertiserBeforeBootstrap(t *testing.T) {
	root := t.TempDir()
	current := baseManifest()
	current.Node.ControlPlaneEndpoint = managedEndpoint("192.0.2.1")
	currentRecord := currentRecord()
	selectEndpointAdvertiser(t, root, &currentRecord)
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:       generation.ApplyModeAuto,
		CurrentManifest: current,
		CurrentRecord:   currentRecord,
		NodeOverrides: map[string]NodeOverlay{
			"cp-1": {ControlPlaneEndpointSet: true},
		},
	}))
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	if result.Plan.Decision.AcceptedMode != generation.ApplyModeNextBoot || !containsDomain(result.Plan.Decision.ChangedDomains, DomainControlPlaneEndpointBootstrap) {
		t.Fatalf("decision = %#v", result.Plan.Decision)
	}
	for _, ref := range result.Plan.GenerationRecord.Sysexts {
		if ref.Name == "endpoint-advertiser" {
			t.Fatalf("candidate retained disabled endpoint advertiser: %#v", ref)
		}
	}
	if _, err := os.Stat(filepath.Join(result.Tree.ConfextDir, "etc/katl/apps/bgp-api-vip/advertisement-enabled")); !os.IsNotExist(err) {
		t.Fatalf("disabled endpoint advertisement marker = %v, want absent", err)
	}
}

func TestApplyTrustedBundleStagesEndpointAdvertiserBeforeBootstrap(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:                generation.ApplyModeAuto,
		EndpointAdvertiserSysext: testEndpointAdvertiserArtifact(t, root),
		NodeOverrides: map[string]NodeOverlay{
			"cp-1": {ControlPlaneEndpointSet: true, ControlPlaneEndpoint: managedEndpoint("192.0.2.1")},
		},
	}))
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	if result.Plan.Decision.AcceptedMode != generation.ApplyModeNextBoot || !containsDomain(result.Plan.Decision.ChangedDomains, DomainControlPlaneEndpointBootstrap) {
		t.Fatalf("decision = %#v", result.Plan.Decision)
	}
	var endpoint *generation.ExtensionRef
	for i := range result.Plan.GenerationRecord.Sysexts {
		if result.Plan.GenerationRecord.Sysexts[i].Name == "endpoint-advertiser" {
			endpoint = &result.Plan.GenerationRecord.Sysexts[i]
		}
	}
	if endpoint == nil {
		t.Fatalf("candidate sysexts = %#v, want endpoint advertiser", result.Plan.GenerationRecord.Sysexts)
	}
	if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(endpoint.Path, "/"))); err != nil {
		t.Fatalf("candidate endpoint advertiser sysext: %v", err)
	}
}

func TestApplyTrustedBundleExplainsMissingPreBootstrapEndpointArtifact(t *testing.T) {
	root := t.TempDir()
	_, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode: generation.ApplyModeAuto,
		NodeOverrides: map[string]NodeOverlay{
			"cp-1": {ControlPlaneEndpointSet: true, ControlPlaneEndpoint: managedEndpoint("192.0.2.1")},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "artifact retained during installation") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want retained artifact recovery guidance", err)
	}
}

func TestApplyTrustedBundleRendersAndExecutesLiveSysctl(t *testing.T) {
	root := t.TempDir()
	activator := &fakeActivator{}
	runner := &fakeCommandRunner{}
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeLive,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Sysctl: &manifest.SysctlConfig{Settings: map[string]string{
				"net.ipv4.ip_forward": "1",
			}},
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
	sysctlPath := filepath.Join(result.Tree.ConfextDir, "etc/sysctl.d/90-katl.conf")
	data, err := os.ReadFile(sysctlPath)
	if err != nil {
		t.Fatalf("read sysctl file: %v", err)
	}
	if !strings.Contains(string(data), "net.ipv4.ip_forward = 1") {
		t.Fatalf("sysctl content = %q", data)
	}
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-daemon-reload,systemd-sysctl"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	persisted, err := generation.ReadConfigApplyStatus(result.StatusPath)
	if err != nil {
		t.Fatalf("ReadConfigApplyStatus() error = %v", err)
	}
	if persisted.RequestedApplyMode != generation.ApplyModeLive || persisted.AcceptedApplyMode != generation.ApplyModeLive || persisted.Phase != generation.ConfigApplyPhaseActive {
		t.Fatalf("persisted status = %#v", persisted)
	}
	if result.Audit.SourceID != "operator" || result.Audit.DesiredVersion != "2" || result.Audit.Decision != DecisionAccepted {
		t.Fatalf("audit = %#v", result.Audit)
	}
}

func TestApplyTrustedBundleDefaultsAutoToLiveForSysctl(t *testing.T) {
	root := t.TempDir()
	runner := &fakeCommandRunner{}
	request := trustedBundleRequest(root, TrustedBundleRequest{
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Sysctl: &manifest.SysctlConfig{Settings: map[string]string{
				"net.ipv4.ip_forward": "1",
			}},
		},
		Executor: &Executor{Runner: runner, Activator: &fakeActivator{}, Now: fixedNow},
	})
	request.ApplyMode = ""
	result, err := ApplyTrustedBundle(context.Background(), request)
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	if result.Plan.Decision.RequestedMode != generation.ApplyModeAuto || result.Plan.Decision.AcceptedMode != generation.ApplyModeLive {
		t.Fatalf("decision = %#v", result.Plan.Decision)
	}
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-daemon-reload,systemd-sysctl"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	persisted, err := generation.ReadConfigApplyStatus(result.StatusPath)
	if err != nil {
		t.Fatalf("ReadConfigApplyStatus() error = %v", err)
	}
	if persisted.RequestedApplyMode != generation.ApplyModeAuto || persisted.AcceptedApplyMode != generation.ApplyModeLive {
		t.Fatalf("persisted status = %#v", persisted)
	}
}

func TestApplyTrustedBundleStagesStrictNextBootSysctl(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Sysctl: &manifest.SysctlConfig{Settings: map[string]string{
				"net.ipv4.ip_forward": "1",
			}},
		},
	}))
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	if result.Plan.Decision.AcceptedMode != generation.ApplyModeNextBoot || result.Status.Phase != generation.ConfigApplyPhaseNextBoot {
		t.Fatalf("plan/status = %#v %#v", result.Plan.Decision, result.Status)
	}
	if len(result.Status.DomainActions) != 1 || result.Status.DomainActions[0].Action != "stage-next-boot" || result.Status.DomainActions[0].Status != generation.ConfigApplyActionSkipped {
		t.Fatalf("domain actions = %#v", result.Status.DomainActions)
	}
	if _, err := os.Stat(filepath.Join(result.Tree.ConfextDir, "etc/sysctl.d/90-katl.conf")); err != nil {
		t.Fatalf("stat sysctl file: %v", err)
	}
}

func TestApplyTrustedBundleDefaultsAutoToNextBootForNetworkd(t *testing.T) {
	root := t.TempDir()
	request := trustedBundleRequest(root, TrustedBundleRequest{
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Networkd: &manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{
				Name:    "10-common.network",
				Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
			}}},
		},
	})
	request.ApplyMode = ""
	result, err := ApplyTrustedBundle(context.Background(), request)
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	if result.Plan.Decision.RequestedMode != generation.ApplyModeAuto || result.Plan.Decision.AcceptedMode != generation.ApplyModeNextBoot {
		t.Fatalf("decision = %#v", result.Plan.Decision)
	}
	release, err := os.ReadFile(filepath.Join(result.Tree.ConfextDir, "etc/extension-release.d/extension-release.katl-node"))
	if err != nil {
		t.Fatalf("read confext release: %v", err)
	}
	if got := string(release); got != "ID=katlos\nVERSION_ID=0.1.0\nCONFEXT_LEVEL=1\n" {
		t.Fatalf("confext release = %q", got)
	}
	record, err := generation.ReadRecord(result.MetadataPath)
	if err != nil {
		t.Fatalf("ReadRecord() error = %v", err)
	}
	if record.ConfigApply == nil || record.ConfigApply.RequestedApplyMode != generation.ApplyModeAuto || record.ConfigApply.AcceptedApplyMode != generation.ApplyModeNextBoot {
		t.Fatalf("config apply metadata = %#v", record.ConfigApply)
	}
	if len(record.Confexts) != 1 || record.Confexts[0].Compatibility.ID != "katlos" || record.Confexts[0].Compatibility.VersionID != "0.1.0" || record.Confexts[0].Compatibility.ConfextLevel != 1 {
		t.Fatalf("confext metadata = %#v", record.Confexts)
	}
	persisted, err := generation.ReadConfigApplyStatus(result.StatusPath)
	if err != nil {
		t.Fatalf("ReadConfigApplyStatus() error = %v", err)
	}
	if persisted.RequestedApplyMode != generation.ApplyModeAuto || persisted.AcceptedApplyMode != generation.ApplyModeNextBoot || persisted.Phase != generation.ConfigApplyPhaseNextBoot {
		t.Fatalf("persisted status = %#v", persisted)
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
				Identity: &IdentityOverlay{AuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl"}},
			},
		},
		NodeOverrides: map[string]NodeOverlay{
			"cp-1": {
				Identity: &IdentityOverlay{Hostname: "worker-1"},
			},
		},
	}))
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	if result.Manifest.Node.SystemRole != "control-plane" || result.Manifest.Node.Identity.Hostname != "worker-1" {
		t.Fatalf("merged node = %#v", result.Manifest.Node)
	}
	for _, domain := range []string{DomainNetworkd, DomainNodeIdentity, DomainBootstrapNodeMetadata} {
		if !containsDomain(result.Plan.Decision.ChangedDomains, domain) {
			t.Fatalf("changed domains = %#v, missing %s", result.Plan.Decision.ChangedDomains, domain)
		}
	}
	if containsDomain(result.Plan.Decision.ChangedDomains, DomainSSHOperatorAccess) {
		t.Fatalf("unchanged SSH access reported as changed: %#v", result.Plan.Decision.ChangedDomains)
	}
	if result.Status.Phase != generation.ConfigApplyPhaseNextBoot {
		t.Fatalf("status phase = %q", result.Status.Phase)
	}
}

func TestApplyTrustedBundleStagesKubeadmInputWithoutSideEffects(t *testing.T) {
	root := t.TempDir()
	runner := &fakeCommandRunner{}
	activator := &fakeActivator{}
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeNextBoot,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Kubernetes: &manifest.KubernetesConfig{Kubeadm: manifest.KubeadmReference{ConfigRef: "control-plane-v2"}},
		},
		CurrentManifest: manifestWithKubeadm(),
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane-v2": kubeadmPlan("control-plane-v2"),
		},
		Executor: &Executor{Runner: runner, Activator: activator, Now: fixedNow},
	}))
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v, result = %#v", err, result)
	}
	if result.Audit.Decision != DecisionAccepted || result.Audit.AcceptedApplyMode != generation.ApplyModeNextBoot || len(result.Audit.Diagnostics) != 1 || result.Audit.Diagnostics[0].Decision != DecisionActionRequired {
		t.Fatalf("audit = %#v", result.Audit)
	}
	configPath := filepath.Join(result.Tree.ConfextDir, "etc/katl/kubeadm/control-plane-v2/config.yaml")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("stat staged kubeadm input: %v", err)
	}
	if result.Status.GenerationID != "2026.06.05-002" || result.Status.PreviousGeneration != "2026.06.05-001" || result.Status.Phase != generation.ConfigApplyPhaseNextBoot {
		t.Fatalf("status identity/phase = %#v", result.Status)
	}
	if !result.Status.Kubeadm.Required || result.Status.Kubeadm.PreviousConfigName != "control-plane" || result.Status.Kubeadm.SelectedConfigName != "control-plane-v2" || !strings.Contains(result.Status.Kubeadm.Reason, "operator-owned") {
		t.Fatalf("kubeadm action status = %#v", result.Status.Kubeadm)
	}
	if !hasDomainAction(result.Status.DomainActions, DomainSelectedKubeadmConfig, "kubeadm-operation-required", generation.ConfigApplyActionSkipped) {
		t.Fatalf("domain actions = %#v", result.Status.DomainActions)
	}
	if len(runner.commands) != 0 || activator.activated != "" {
		t.Fatalf("normal apply executed commands %#v or activated %q", runner.commands, activator.activated)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/kubernetes")); !os.IsNotExist(err) {
		t.Fatalf("normal apply created /etc/kubernetes: %v", err)
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
	data, err := os.ReadFile(first.AuditPath)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(string(data), `"recordType": "katl.config-request.decision"`) || !strings.Contains(string(data), `"payload": {`) {
		t.Fatalf("audit is not enveloped:\n%s", data)
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

func TestReadAuditRejectsUnsupportedEnvelopeVersion(t *testing.T) {
	data, err := persistedrecord.MarshalEnvelope(persistedrecord.Envelope{
		RecordType:    ConfigRequestDecisionRecordType,
		RecordVersion: 2,
		Payload:       []byte("{}\n"),
	})
	if err != nil {
		t.Fatalf("MarshalEnvelope() error = %v", err)
	}
	_, err = decodeAudit(data)
	if err == nil || !strings.Contains(err.Error(), "unsupported persisted record") {
		t.Fatalf("decodeAudit() error = %v, want unsupported persisted record", err)
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

func TestApplyTrustedBundleRejectsUnsafeSysctlBeforeRender(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeLive,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Sysctl: &manifest.SysctlConfig{Settings: map[string]string{
				"net.ipv4.conf.all.forwarding": "1",
			}},
		},
		Executor: &Executor{Runner: &fakeCommandRunner{}, Activator: &fakeActivator{}, Now: fixedNow},
	}))
	if err == nil || !strings.Contains(err.Error(), "is not supported") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want unsupported sysctl key; result = %#v", err, result)
	}
	if result.Audit.Decision != DecisionRejected || !containsDomain(result.Audit.ChangedDomains, DomainSysctl) {
		t.Fatalf("audit = %#v", result.Audit)
	}
	assertGenerationMissing(t, root, "2026.06.05-002")
}

func TestApplyTrustedBundleRejectsEmptySysctlOverlayBeforeRender(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:    generation.ApplyModeLive,
		GenerationID: "2026.06.05-002",
		ClusterDefaults: NodeOverlay{
			Sysctl: &manifest.SysctlConfig{},
		},
		Executor: &Executor{Runner: &fakeCommandRunner{}, Activator: &fakeActivator{}, Now: fixedNow},
	}))
	if err == nil || !strings.Contains(err.Error(), "sysctl.settings must contain at least one setting") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want empty sysctl rejection; result = %#v", err, result)
	}
	if result.Audit.Decision != DecisionRejected || len(result.Audit.ChangedDomains) != 0 {
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

func TestApplyTrustedBundleRejectsUnsafeKubeadmRenderPath(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "cannot own kubeadm-managed /etc/kubernetes state") {
		t.Fatalf("ApplyTrustedBundle() error = %v, want unsafe kubeadm render rejection; result = %#v", err, result)
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
			Sysctl: &manifest.SysctlConfig{Settings: map[string]string{
				"net.ipv4.ip_forward": "1",
			}},
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
	request.KubernetesInitialized = override.KubernetesInitialized
	request.EndpointAdvertiserSysext = override.EndpointAdvertiserSysext
	request.Executor = override.Executor
	if override.Chown != nil {
		request.Chown = override.Chown
	}
	if override.Now != nil {
		request.Now = override.Now
	}
	ensureTestSysext(root, request.CurrentRecord)
	return request
}

func ensureTestSysext(root string, record generation.Record) {
	for _, ref := range record.Sysexts {
		path := filepath.Join(filepath.Clean(root), strings.TrimPrefix(ref.Path, "/"))
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(path, []byte("test sysext "+ref.Name+"\n"), 0o644); err != nil {
			panic(err)
		}
	}
}

func testEndpointAdvertiserArtifact(t *testing.T, root string) *generation.ExtensionRef {
	t.Helper()
	content := []byte("endpoint advertiser sysext\n")
	sum := sha256.Sum256(content)
	path := "/var/lib/katl/artifacts/katlos-image/katl-endpoint-advertiser.raw"
	fullPath := filepath.Join(root, strings.TrimPrefix(path, "/"))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return &generation.ExtensionRef{
		Name:            "endpoint-advertiser",
		Path:            path,
		ActivationPath:  "/run/extensions/katl-endpoint-advertiser.raw",
		SHA256:          hex.EncodeToString(sum[:]),
		ArtifactVersion: "2026.7.0",
		PayloadVersion:  "2026.7.0",
		Architecture:    "x86_64",
		Compatibility:   generation.ExtensionCompatibility{RuntimeInterfaces: []string{"katl-runtime-1"}},
	}
}

func selectEndpointAdvertiser(t *testing.T, root string, record *generation.Record) {
	t.Helper()
	artifact := testEndpointAdvertiserArtifact(t, root)
	source := filepath.Join(root, strings.TrimPrefix(artifact.Path, "/"))
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	selected := *artifact
	selected.Path = filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, record.GenerationID, "sysext", "endpoint-advertiser.raw"))
	target := filepath.Join(root, strings.TrimPrefix(selected.Path, "/"))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	record.Sysexts = append(record.Sysexts, selected)
}

func baseManifest() manifest.Manifest {
	return manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		Node: manifest.NodeConfig{
			Identity: manifest.NodeIdentity{
				Hostname: "cp-1",
				SSH: manifest.SSHIdentity{
					AuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl"},
				},
			},
			SystemRole: "control-plane",
		},
		Install: manifest.InstallConfig{
			WipeTarget: true,
			TargetDisk: manifest.DiskSelector{ByID: "disk/by-id/test"},
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

func hasDomainAction(actions []generation.ConfigApplyDomainAction, domain, action, status string) bool {
	for _, candidate := range actions {
		if candidate.Domain == domain && candidate.Action == action && candidate.Status == status {
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
