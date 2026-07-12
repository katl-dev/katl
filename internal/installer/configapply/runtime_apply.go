package configapply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/configdomain"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
)

const (
	NodeConfigurationChangeAPIVersion = "katl.dev/v1alpha1"
	ConfigRequestAuditKind            = "ConfigRequestAudit"
	ConfigRequestDecisionRecordType   = "katl.config-request.decision"
	configRequestDecisionVersion      = 1
)

type TrustedBundleRequest struct {
	Root                            string
	SourceID                        string
	DesiredVersion                  string
	NodeName                        string
	ApplyMode                       string
	GenerationID                    string
	CurrentManifest                 manifest.Manifest
	CurrentRecord                   generation.Record
	ClusterDefaults                 NodeOverlay
	SystemRoleOverrides             map[string]NodeOverlay
	NodeOverrides                   map[string]NodeOverlay
	KubeadmConfigs                  map[string]kubeadmconfig.Plan
	KubernetesVersion               string
	KubernetesActivationPath        string
	RuntimeKubernetesVersion        string
	RuntimeKubernetesActivationPath string
	Executor                        *Executor
	Chown                           func(path string, uid int, gid int) error
	Now                             func() time.Time
}

type NodeOverlay struct {
	Identity       *IdentityOverlay
	SystemRole     string
	Networkd       *manifest.NetworkdConfig
	Sysctl         *manifest.SysctlConfig
	Kubernetes     *manifest.KubernetesConfig
	UnsafeEtcFiles []confext.NativeEtcFile
	KubeadmChanged bool
	LivePreflight  map[string]bool
}

type IdentityOverlay struct {
	Hostname       string   `json:"hostname,omitempty" yaml:"hostname,omitempty"`
	AuthorizedKeys []string `json:"authorizedKeys,omitempty" yaml:"authorizedKeys,omitempty"`
}

type TrustedBundleResult struct {
	Manifest     manifest.Manifest
	Files        []confext.NativeEtcFile
	Tree         confext.GenerationTree
	Plan         Result
	Status       generation.ConfigApplyStatus
	Audit        ConfigRequestAudit
	MetadataPath string
	StatusPath   string
	AuditPath    string
}

type ConfigRequestAudit struct {
	APIVersion          string                           `json:"apiVersion"`
	Kind                string                           `json:"kind"`
	SourceID            string                           `json:"sourceID"`
	DesiredVersion      string                           `json:"desiredVersion"`
	RequestDigest       string                           `json:"requestDigest"`
	RequestedApplyMode  string                           `json:"requestedApplyMode"`
	AcceptedApplyMode   string                           `json:"acceptedApplyMode,omitempty"`
	ChangedDomains      []string                         `json:"changedDomains,omitempty"`
	PreviousGeneration  string                           `json:"previousGenerationID,omitempty"`
	CandidateGeneration string                           `json:"candidateGenerationID,omitempty"`
	Decision            string                           `json:"decision"`
	Diagnostics         []Diagnostic                     `json:"diagnostics,omitempty"`
	Kubeadm             generation.KubeadmActionRequired `json:"kubeadm,omitempty"`
	FailureReason       string                           `json:"failureReason,omitempty"`
	UpdatedAt           time.Time                        `json:"updatedAt"`
}

func ApplyTrustedBundle(ctx context.Context, request TrustedBundleRequest) (TrustedBundleResult, error) {
	if strings.TrimSpace(request.Root) == "" {
		return TrustedBundleResult{}, fmt.Errorf("runtime root is required")
	}
	sourceID, err := cleanAuditSegment("sourceID", request.SourceID)
	if err != nil {
		return TrustedBundleResult{}, err
	}
	desiredVersion, err := cleanDesiredVersion(request.DesiredVersion)
	if err != nil {
		return TrustedBundleResult{}, err
	}
	request.SourceID = sourceID
	request.DesiredVersion = desiredVersion
	if strings.TrimSpace(request.GenerationID) == "" {
		return TrustedBundleResult{}, fmt.Errorf("generation id is required")
	}
	now := request.now()
	applyMode, err := normalizeRequestedMode(request.ApplyMode)
	if err != nil {
		audit := request.audit(sourceID, desiredVersion, DecisionRejected, nil, nil, err, now)
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	request.ApplyMode = applyMode
	if err := rejectRuntimeSelectionOverrides(request); err != nil {
		audit := request.audit(sourceID, desiredVersion, DecisionRejected, nil, nil, err, now)
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	replay, ok, err := request.checkFreshness(sourceID, desiredVersion, now)
	if err != nil {
		return replay, err
	}
	if ok {
		return replay, nil
	}
	merged, changes, unsafeFiles, err := mergeRuntimeConfig(request)
	if err != nil {
		audit := request.audit(sourceID, desiredVersion, "", nil, nil, err, now)
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	if err := manifest.Validate(merged); err != nil {
		audit := request.audit(sourceID, desiredVersion, "", changes, nil, err, now)
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Manifest: merged, Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	matrixDecision, err := Plan(request.ApplyMode, changes)
	if err != nil {
		audit := request.audit(sourceID, desiredVersion, "", changes, matrixDecision.Diagnostics, err, now)
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Manifest: merged, Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	files, err := configdomain.NativeEtcFiles(configdomain.RenderRequest{
		Manifest:                 merged,
		KubeadmConfigs:           request.KubeadmConfigs,
		KubernetesVersion:        runtimeKubernetesPayloadVersion(request),
		KubernetesActivationPath: runtimeKubernetesActivationPath(request),
	})
	if err != nil {
		audit := request.audit(sourceID, desiredVersion, "", changes, nil, err, now)
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Manifest: merged, Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	files = append(files, unsafeFiles...)
	audit := request.audit(sourceID, desiredVersion, DecisionAccepted, changes, matrixDecision.Diagnostics, nil, now)
	audit.CandidateGeneration = request.GenerationID
	audit.AcceptedApplyMode = matrixDecision.AcceptedMode
	auditPath, err := writeAudit(request.Root, sourceID, desiredVersion, audit)
	if err != nil {
		return TrustedBundleResult{Manifest: merged, Files: files, Audit: audit}, err
	}
	release, err := confextReleaseFromCurrent(request.CurrentRecord)
	if err != nil {
		audit = request.audit(sourceID, desiredVersion, "", changes, nil, err, now)
		audit.CandidateGeneration = request.GenerationID
		audit.AcceptedApplyMode = matrixDecision.AcceptedMode
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Manifest: merged, Files: files, Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	tree, err := confext.RenderGenerationTree(confext.GenerationTreeRequest{
		GenerationsRoot: filepath.Join(filepath.Clean(request.Root), strings.TrimPrefix(generation.GenerationRecordsDir, "/")),
		GenerationID:    request.GenerationID,
		Files:           files,
		Extension:       release,
		Chown:           request.Chown,
	})
	if err != nil {
		audit = request.audit(sourceID, desiredVersion, "", changes, nil, err, now)
		audit.CandidateGeneration = request.GenerationID
		audit.AcceptedApplyMode = matrixDecision.AcceptedMode
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Manifest: merged, Files: files, Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	digest, err := generation.DigestDirectory(tree.ConfextDir)
	if err != nil {
		audit = request.audit(sourceID, desiredVersion, "", changes, nil, err, now)
		audit.CandidateGeneration = request.GenerationID
		audit.AcceptedApplyMode = matrixDecision.AcceptedMode
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Manifest: merged, Files: files, Tree: tree, Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	sysexts, err := materializeSysexts(request.Root, request.GenerationID, request.CurrentRecord.Sysexts)
	if err != nil {
		audit = request.audit(sourceID, desiredVersion, "", changes, nil, err, now)
		audit.CandidateGeneration = request.GenerationID
		audit.AcceptedApplyMode = matrixDecision.AcceptedMode
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Manifest: merged, Files: files, Tree: tree, Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	plan, err := PlanChange(request.CurrentRecord, NodeConfigurationChange{
		APIVersion:   NodeConfigurationChangeAPIVersion,
		Kind:         NodeConfigurationChangeKind,
		GenerationID: request.GenerationID,
		SourceDigest: requestDigest(request),
		Apply:        Apply{Mode: request.ApplyMode},
		Changes:      changes,
		Sysexts:      sysexts,
		GeneratedConfext: generation.GeneratedConfext{
			Name:           release.Name,
			Path:           filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, request.GenerationID, "confext")),
			ActivationPath: "/run/confexts/" + release.Name,
			SHA256:         digest,
			Compatibility: generation.ConfextCompatibility{
				ID:           release.ID,
				VersionID:    release.VersionID,
				ConfextLevel: release.ConfextLevel,
			},
		},
		Kubeadm:     request.kubeadmActionRequired(changes),
		RequestedAt: now,
	})
	if err != nil {
		audit = request.audit(sourceID, desiredVersion, "", changes, nil, err, now)
		audit.CandidateGeneration = request.GenerationID
		audit.AcceptedApplyMode = matrixDecision.AcceptedMode
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Manifest: merged, Files: files, Tree: tree, Audit: audit, AuditPath: auditPath}, joinAuditError(err, auditErr)
	}
	metadataPath, err := generation.MetadataPath(request.Root, plan.GenerationRecord.GenerationID)
	if err != nil {
		return TrustedBundleResult{}, err
	}
	if err := generation.WriteRecord(metadataPath, plan.GenerationRecord); err != nil {
		return TrustedBundleResult{}, err
	}
	statusPath, err := generation.ConfigApplyStatusPath(request.Root, plan.GenerationRecord.GenerationID)
	if err != nil {
		return TrustedBundleResult{}, err
	}
	status := plan.Status
	status, err = generation.MarkConfigApplyPhase(status, initialPhase(plan.Decision.AcceptedMode), now)
	if err != nil {
		return TrustedBundleResult{}, err
	}
	if err := generation.WriteConfigApplyStatus(statusPath, status); err != nil {
		return TrustedBundleResult{}, err
	}
	if plan.Decision.AcceptedMode == generation.ApplyModeLive {
		if request.Executor == nil {
			return TrustedBundleResult{}, fmt.Errorf("live apply executor is required")
		}
		executor := *request.Executor
		executor.StatusPath = statusPath
		status, err = executor.ExecuteLive(ctx, plan)
		if err != nil {
			audit := request.audit(sourceID, desiredVersion, generation.ConfigApplyActionFailed, changes, nil, err, now)
			audit.CandidateGeneration = plan.GenerationRecord.GenerationID
			audit.AcceptedApplyMode = plan.Decision.AcceptedMode
			auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
			return TrustedBundleResult{
				Manifest:     merged,
				Files:        files,
				Tree:         tree,
				Plan:         plan,
				Status:       status,
				Audit:        audit,
				MetadataPath: metadataPath,
				StatusPath:   statusPath,
				AuditPath:    auditPath,
			}, joinAuditError(err, auditErr)
		}
	}
	return TrustedBundleResult{
		Manifest:     merged,
		Files:        files,
		Tree:         tree,
		Plan:         plan,
		Status:       status,
		Audit:        audit,
		MetadataPath: metadataPath,
		StatusPath:   statusPath,
		AuditPath:    auditPath,
	}, nil
}

func confextReleaseFromCurrent(record generation.Record) (confext.ExtensionRelease, error) {
	for _, ref := range record.Confexts {
		if strings.TrimSpace(ref.Name) != "katl-node" {
			continue
		}
		release := confext.ExtensionRelease{
			Name:         ref.Name,
			ID:           ref.Compatibility.ID,
			VersionID:    ref.Compatibility.VersionID,
			ConfextLevel: ref.Compatibility.ConfextLevel,
		}
		if strings.TrimSpace(release.ID) == "" || strings.TrimSpace(release.VersionID) == "" || release.ConfextLevel < 1 {
			return confext.ExtensionRelease{}, fmt.Errorf("current katl-node confext compatibility metadata is incomplete")
		}
		return release, nil
	}
	return confext.ExtensionRelease{}, fmt.Errorf("current katl-node confext compatibility metadata is required")
}

func PlanTrustedBundle(request TrustedBundleRequest) (TrustedBundleResult, error) {
	if strings.TrimSpace(request.Root) == "" {
		return TrustedBundleResult{}, fmt.Errorf("runtime root is required")
	}
	sourceID, err := cleanAuditSegment("sourceID", request.SourceID)
	if err != nil {
		return TrustedBundleResult{}, err
	}
	desiredVersion, err := cleanDesiredVersion(request.DesiredVersion)
	if err != nil {
		return TrustedBundleResult{}, err
	}
	request.SourceID = sourceID
	request.DesiredVersion = desiredVersion
	if strings.TrimSpace(request.GenerationID) == "" {
		return TrustedBundleResult{}, fmt.Errorf("generation id is required")
	}
	now := request.now()
	applyMode, err := normalizeRequestedMode(request.ApplyMode)
	if err != nil {
		return TrustedBundleResult{}, err
	}
	request.ApplyMode = applyMode
	if err := rejectRuntimeSelectionOverrides(request); err != nil {
		return TrustedBundleResult{}, err
	}
	merged, changes, unsafeFiles, err := mergeRuntimeConfig(request)
	if err != nil {
		return TrustedBundleResult{Manifest: merged}, err
	}
	if err := manifest.Validate(merged); err != nil {
		return TrustedBundleResult{Manifest: merged}, err
	}
	matrixDecision, err := Plan(request.ApplyMode, changes)
	if err != nil {
		return TrustedBundleResult{
			Manifest: merged,
			Plan:     Result{Decision: matrixDecision},
		}, err
	}
	files, err := configdomain.NativeEtcFiles(configdomain.RenderRequest{
		Manifest:                 merged,
		KubeadmConfigs:           request.KubeadmConfigs,
		KubernetesVersion:        runtimeKubernetesPayloadVersion(request),
		KubernetesActivationPath: runtimeKubernetesActivationPath(request),
	})
	if err != nil {
		return TrustedBundleResult{Manifest: merged}, err
	}
	files = append(files, unsafeFiles...)
	status, err := generation.NewConfigApplyStatus(generation.ConfigApplyStatusRequest{
		GenerationID:       request.GenerationID,
		PreviousGeneration: request.CurrentRecord.GenerationID,
		RequestedApplyMode: matrixDecision.RequestedMode,
		AcceptedApplyMode:  matrixDecision.AcceptedMode,
		ChangedDomains:     matrixDecision.ChangedDomains,
		HealthState:        request.CurrentRecord.HealthState,
		Kubeadm:            request.kubeadmActionRequired(changes),
		UpdatedAt:          now,
	})
	if err != nil {
		return TrustedBundleResult{Manifest: merged, Files: files}, err
	}
	status.DomainActions = domainActions(matrixDecision.AcceptedMode, matrixDecision.ChangedDomains)
	if err := generation.ValidateConfigApplyStatus(status); err != nil {
		return TrustedBundleResult{Manifest: merged, Files: files}, err
	}
	return TrustedBundleResult{
		Manifest: merged,
		Files:    files,
		Plan: Result{
			Decision: matrixDecision,
			Status:   status,
		},
		Status: status,
	}, nil
}

func mergeRuntimeConfig(request TrustedBundleRequest) (manifest.Manifest, []Change, []confext.NativeEtcFile, error) {
	merged := request.CurrentManifest
	domains := domainAccumulator{}
	var unsafeFiles []confext.NativeEtcFile
	if err := validateOverlay("clusterDefaults", request.ClusterDefaults); err != nil {
		return manifest.Manifest{}, nil, nil, err
	}
	applyOverlay(&merged.Node, request.ClusterDefaults, &domains, &unsafeFiles)
	roleOverlay := request.SystemRoleOverrides[merged.Node.SystemRole]
	if err := validateOverlay("systemRoleOverrides."+merged.Node.SystemRole, roleOverlay); err != nil {
		return manifest.Manifest{}, nil, nil, err
	}
	applyOverlay(&merged.Node, roleOverlay, &domains, &unsafeFiles)
	nodeOverlay := request.NodeOverrides[request.NodeName]
	if request.NodeName != "" {
		if err := validateOverlay("nodeOverrides."+request.NodeName, nodeOverlay); err != nil {
			return manifest.Manifest{}, nil, nil, err
		}
		applyOverlay(&merged.Node, nodeOverlay, &domains, &unsafeFiles)
	}
	if len(domains.domains) == 0 {
		return manifest.Manifest{}, nil, nil, fmt.Errorf("runtime configuration change has no supported changed domains; desired state already matches")
	}
	return merged, domains.changes(request.ClusterDefaults, roleOverlay, nodeOverlay), unsafeFiles, nil
}

func validateOverlay(path string, overlay NodeOverlay) error {
	if overlay.Sysctl != nil && len(overlay.Sysctl.Settings) == 0 {
		return fmt.Errorf("%s.sysctl.settings must contain at least one setting", path)
	}
	return nil
}

func applyOverlay(node *manifest.NodeConfig, overlay NodeOverlay, domains *domainAccumulator, unsafeFiles *[]confext.NativeEtcFile) {
	if overlay.Identity != nil {
		if overlay.Identity.Hostname != "" {
			changed := node.Identity.Hostname != overlay.Identity.Hostname
			node.Identity.Hostname = overlay.Identity.Hostname
			if changed {
				domains.add(DomainNodeIdentity)
				domains.add(DomainBootstrapNodeMetadata)
			}
		}
		if overlay.Identity.AuthorizedKeys != nil {
			changed := !slices.Equal(node.Identity.SSH.AuthorizedKeys, overlay.Identity.AuthorizedKeys)
			node.Identity.SSH.AuthorizedKeys = append([]string(nil), overlay.Identity.AuthorizedKeys...)
			if changed {
				domains.add(DomainSSHOperatorAccess)
			}
		}
	}
	if overlay.SystemRole != "" {
		changed := node.SystemRole != overlay.SystemRole
		node.SystemRole = overlay.SystemRole
		if changed {
			domains.add(DomainSystemRole)
			domains.add(DomainBootstrapNodeMetadata)
		}
	}
	if overlay.Networkd != nil {
		changed := !slices.Equal(node.Networkd.Files, overlay.Networkd.Files)
		node.Networkd = *overlay.Networkd
		if changed {
			domains.add(DomainNetworkd)
		}
	}
	if overlay.Sysctl != nil {
		changed := !maps.Equal(node.Sysctl.Settings, overlay.Sysctl.Settings)
		node.Sysctl = *overlay.Sysctl
		if changed {
			domains.add(DomainSysctl)
		}
	}
	if overlay.Kubernetes != nil {
		changed := node.Kubernetes != *overlay.Kubernetes
		node.Kubernetes = *overlay.Kubernetes
		if changed {
			domains.add(DomainSelectedKubeadmConfig)
			domains.add(DomainBootstrapNodeMetadata)
		}
	}
	if overlay.KubeadmChanged {
		domains.add(DomainKubeadmConfig)
	}
	if len(overlay.UnsafeEtcFiles) > 0 {
		*unsafeFiles = append(*unsafeFiles, overlay.UnsafeEtcFiles...)
		domains.add(DomainArbitraryEtc)
	}
}

type domainAccumulator struct {
	domains []string
	seen    map[string]struct{}
}

func (a *domainAccumulator) add(domain string) {
	if a.seen == nil {
		a.seen = map[string]struct{}{}
	}
	if _, ok := a.seen[domain]; ok {
		return
	}
	a.seen[domain] = struct{}{}
	a.domains = append(a.domains, domain)
}

func (a *domainAccumulator) changes(overlays ...NodeOverlay) []Change {
	preflight := map[string]bool{}
	for _, overlay := range overlays {
		for domain, ok := range overlay.LivePreflight {
			if ok {
				preflight[domain] = true
			}
		}
	}
	changes := make([]Change, 0, len(a.domains))
	for _, domain := range a.domains {
		changes = append(changes, Change{Domain: domain, LivePreflightOK: preflight[domain]})
	}
	return changes
}

func (request TrustedBundleRequest) kubeadmActionRequired(changes []Change) generation.KubeadmActionRequired {
	for _, change := range changes {
		if change.Domain == DomainKubeadmConfig || change.Domain == DomainSelectedKubeadmConfig {
			previous := request.CurrentManifest.Node.Kubernetes.Kubeadm.ConfigRef
			selected := previous
			for _, overlay := range []NodeOverlay{request.ClusterDefaults, request.SystemRoleOverrides[request.CurrentManifest.Node.SystemRole], request.NodeOverrides[request.NodeName]} {
				if overlay.Kubernetes != nil {
					selected = overlay.Kubernetes.Kubeadm.ConfigRef
				}
			}
			return generation.KubeadmActionRequired{
				Required:           true,
				PreviousConfigName: previous,
				SelectedConfigName: selected,
				Reason:             "rendered kubeadm desired input changed; live kubeadm cluster state remains operator-owned and requires an explicit kubeadm-aware action",
			}
		}
	}
	return generation.KubeadmActionRequired{}
}

func initialPhase(mode string) string {
	if mode == generation.ApplyModeNextBoot {
		return generation.ConfigApplyPhaseNextBoot
	}
	return generation.ConfigApplyPhaseRendered
}

func materializeSysexts(root, generationID string, refs []generation.ExtensionRef) ([]generation.ExtensionRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	targetDir := filepath.Join(filepath.Clean(root), strings.TrimPrefix(generation.GenerationRecordsDir, "/"), generationID, "sysext")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, fmt.Errorf("create candidate sysext directory: %w", err)
	}
	out := make([]generation.ExtensionRef, 0, len(refs))
	for _, ref := range refs {
		source := filepath.Join(filepath.Clean(root), strings.TrimPrefix(ref.Path, "/"))
		name := filepath.Base(ref.Path)
		if name == "." || name == "/" || name == "" {
			return nil, fmt.Errorf("sysext %q source path is invalid", ref.Name)
		}
		target := filepath.Join(targetDir, name)
		if err := linkOrCopyFile(source, target); err != nil {
			return nil, fmt.Errorf("materialize sysext %q: %w", ref.Name, err)
		}
		next := ref
		next.Path = filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, generationID, "sysext", name))
		out = append(out, next)
	}
	return out, nil
}

func linkOrCopyFile(source, target string) error {
	if filepath.Clean(source) == filepath.Clean(target) {
		return nil
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	if targetInfo, err := os.Stat(target); err == nil {
		if os.SameFile(info, targetInfo) {
			return nil
		}
		if err := os.Remove(target); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(source, target); err == nil {
		return nil
	}
	return copyFile(source, target, info.Mode().Perm())
}

func copyFile(source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (request TrustedBundleRequest) audit(sourceID, desiredVersion, decision string, changes []Change, diagnostics []Diagnostic, cause error, now time.Time) ConfigRequestAudit {
	if decision == "" {
		if cause != nil {
			decision = DecisionRejected
		} else {
			decision = DecisionAccepted
		}
	}
	domains := make([]string, 0, len(changes))
	for _, change := range changes {
		domains = append(domains, change.Domain)
	}
	return ConfigRequestAudit{
		APIVersion:         generation.APIVersion,
		Kind:               ConfigRequestAuditKind,
		SourceID:           sourceID,
		DesiredVersion:     desiredVersion,
		RequestDigest:      requestDigest(request),
		RequestedApplyMode: request.ApplyMode,
		ChangedDomains:     domains,
		PreviousGeneration: request.CurrentRecord.GenerationID,
		Decision:           decision,
		Diagnostics:        diagnostics,
		Kubeadm:            request.kubeadmActionRequired(changes),
		FailureReason:      generation.RedactConfigApplyMessage(errorString(cause)),
		UpdatedAt:          now.UTC(),
	}
}

func writeAudit(root, sourceID, desiredVersion string, audit ConfigRequestAudit) (string, error) {
	path := auditPath(root, sourceID, desiredVersion)
	data, err := marshalAudit(audit)
	if err != nil {
		return "", fmt.Errorf("marshal config request audit: %w", err)
	}
	if err := persistedrecord.WriteFileAtomic(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write config request audit: %w", err)
	}
	return path, nil
}

func joinAuditError(cause error, auditErr error) error {
	if auditErr == nil {
		return cause
	}
	if cause == nil {
		return auditErr
	}
	return errors.Join(cause, auditErr)
}

func auditPath(root, sourceID, desiredVersion string) string {
	return filepath.Join(filepath.Clean(root), "var/lib/katl/config-requests", sourceID, desiredVersion+".json")
}

func (request TrustedBundleRequest) checkFreshness(sourceID, desiredVersion string, now time.Time) (TrustedBundleResult, bool, error) {
	path := auditPath(request.Root, sourceID, desiredVersion)
	existing, err := readAudit(path)
	if err == nil {
		if existing.RequestDigest != requestDigest(request) {
			return TrustedBundleResult{Audit: existing, AuditPath: path}, false, fmt.Errorf("desiredVersion %s for sourceID %s already has a different request digest", desiredVersion, sourceID)
		}
		return replayResult(request.Root, existing, path), true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return TrustedBundleResult{}, false, err
	}
	latest, ok, err := latestVersion(request.Root, sourceID)
	if err != nil {
		return TrustedBundleResult{}, false, err
	}
	if ok && compareVersion(desiredVersion, latest) < 0 {
		staleErr := fmt.Errorf("desiredVersion %s for sourceID %s is older than recorded version %s", desiredVersion, sourceID, latest)
		audit := request.audit(sourceID, desiredVersion, DecisionRejected, nil, nil, staleErr, now)
		auditPath, auditErr := writeAudit(request.Root, sourceID, desiredVersion, audit)
		return TrustedBundleResult{Audit: audit, AuditPath: auditPath}, false, joinAuditError(staleErr, auditErr)
	}
	return TrustedBundleResult{}, false, nil
}

func rejectRuntimeSelectionOverrides(request TrustedBundleRequest) error {
	if strings.TrimSpace(request.KubernetesVersion) != "" {
		return fmt.Errorf("runtime config apply does not accept Kubernetes sysext version selection")
	}
	if strings.TrimSpace(request.KubernetesActivationPath) != "" {
		return fmt.Errorf("runtime config apply does not accept raw Kubernetes sysext activation paths")
	}
	return nil
}

func readAudit(path string) (ConfigRequestAudit, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ConfigRequestAudit{}, err
	}
	audit, err := decodeAudit(data)
	if err != nil {
		return ConfigRequestAudit{}, fmt.Errorf("decode config request audit: %w", err)
	}
	return audit, nil
}

func marshalAudit(audit ConfigRequestAudit) ([]byte, error) {
	payload, err := json.MarshalIndent(audit, "", "  ")
	if err != nil {
		return nil, err
	}
	return persistedrecord.MarshalEnvelope(persistedrecord.Envelope{
		RecordType:    ConfigRequestDecisionRecordType,
		RecordVersion: configRequestDecisionVersion,
		Payload:       append(payload, '\n'),
	})
}

func decodeAudit(data []byte) (ConfigRequestAudit, error) {
	if looksLikeAuditEnvelope(data) {
		envelope, err := persistedrecord.DecodeEnvelope(data)
		if err != nil {
			return ConfigRequestAudit{}, err
		}
		if envelope.RecordType != ConfigRequestDecisionRecordType || envelope.RecordVersion != configRequestDecisionVersion {
			return ConfigRequestAudit{}, fmt.Errorf("%w: %s/v%d", persistedrecord.ErrUnsupportedRecord, envelope.RecordType, envelope.RecordVersion)
		}
		return persistedrecord.DecodePayload[ConfigRequestAudit](envelope)
	}
	var audit ConfigRequestAudit
	if err := json.Unmarshal(data, &audit); err != nil {
		return ConfigRequestAudit{}, err
	}
	return audit, nil
}

func looksLikeAuditEnvelope(data []byte) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false
	}
	_, ok := fields["recordType"]
	return ok
}

func replayResult(root string, audit ConfigRequestAudit, auditPath string) TrustedBundleResult {
	result := TrustedBundleResult{
		Audit:     audit,
		AuditPath: auditPath,
	}
	if strings.TrimSpace(audit.CandidateGeneration) == "" {
		return result
	}
	metadataPath, err := generation.MetadataPath(root, audit.CandidateGeneration)
	if err == nil {
		if record, readErr := generation.ReadRecord(metadataPath); readErr == nil {
			result.Plan.GenerationRecord = record
			result.MetadataPath = metadataPath
		}
	}
	statusPath, err := generation.ConfigApplyStatusPath(root, audit.CandidateGeneration)
	if err == nil {
		if status, readErr := generation.ReadConfigApplyStatus(statusPath); readErr == nil {
			result.Status = status
			result.StatusPath = statusPath
		}
	}
	return result
}

func latestVersion(root, sourceID string) (string, bool, error) {
	dir := filepath.Join(filepath.Clean(root), "var/lib/katl/config-requests", sourceID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read config request audit directory: %w", err)
	}
	var latest string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".json")
		if _, err := cleanDesiredVersion(version); err != nil {
			continue
		}
		if latest == "" || compareVersion(version, latest) > 0 {
			latest = version
		}
	}
	if latest == "" {
		return "", false, nil
	}
	return latest, true, nil
}

func compareVersion(left, right string) int {
	left = normalizeVersion(left)
	right = normalizeVersion(right)
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func normalizeVersion(value string) string {
	value = strings.TrimLeft(strings.TrimSpace(value), "0")
	if value == "" {
		return "0"
	}
	return value
}

func cleanAuditSegment(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if value != filepath.Base(value) || value == "." || value == ".." {
		return "", fmt.Errorf("%s %q must be a single path segment", name, value)
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r) {
			continue
		}
		return "", fmt.Errorf("%s %q contains unsupported character %q", name, value, r)
	}
	return value, nil
}

func cleanDesiredVersion(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("desiredVersion is required")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("desiredVersion %q must be an unsigned decimal sequence number", value)
		}
	}
	if len(value) > 1 && value[0] == '0' {
		return "", fmt.Errorf("desiredVersion %q must not contain leading zeroes", value)
	}
	return value, nil
}

func requestDigest(request TrustedBundleRequest) string {
	type digestInput struct {
		SourceID             string
		DesiredVersion       string
		NodeName             string
		ApplyMode            string
		GenerationID         string
		CurrentManifest      manifest.Manifest
		ClusterDefaults      NodeOverlay
		SystemRoleOverrides  map[string]NodeOverlay
		NodeOverrides        map[string]NodeOverlay
		KubeadmConfigs       map[string]kubeadmconfig.Plan
		KubernetesVersion    string
		KubernetesActivation string
		RuntimeVersion       string
		RuntimeActivation    string
	}
	data, _ := json.Marshal(digestInput{
		SourceID:             request.SourceID,
		DesiredVersion:       request.DesiredVersion,
		NodeName:             request.NodeName,
		ApplyMode:            request.ApplyMode,
		GenerationID:         request.GenerationID,
		CurrentManifest:      request.CurrentManifest,
		ClusterDefaults:      request.ClusterDefaults,
		SystemRoleOverrides:  request.SystemRoleOverrides,
		NodeOverrides:        request.NodeOverrides,
		KubeadmConfigs:       request.KubeadmConfigs,
		KubernetesVersion:    request.KubernetesVersion,
		KubernetesActivation: request.KubernetesActivationPath,
		RuntimeVersion:       request.RuntimeKubernetesVersion,
		RuntimeActivation:    request.RuntimeKubernetesActivationPath,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (request TrustedBundleRequest) now() time.Time {
	if request.Now != nil {
		return request.Now()
	}
	return time.Now().UTC()
}

func selectedKubernetesPayloadVersion(record generation.Record) string {
	for _, sysext := range record.Sysexts {
		if sysext.Name == "kubernetes" {
			return sysext.PayloadVersion
		}
	}
	return ""
}

func selectedKubernetesActivationPath(record generation.Record) string {
	for _, sysext := range record.Sysexts {
		if sysext.Name == "kubernetes" {
			return sysext.ActivationPath
		}
	}
	return ""
}

func runtimeKubernetesPayloadVersion(request TrustedBundleRequest) string {
	return firstNonEmpty(request.KubernetesVersion, selectedKubernetesPayloadVersion(request.CurrentRecord), request.RuntimeKubernetesVersion)
}

func runtimeKubernetesActivationPath(request TrustedBundleRequest) string {
	return firstNonEmpty(request.KubernetesActivationPath, selectedKubernetesActivationPath(request.CurrentRecord), request.RuntimeKubernetesActivationPath)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
