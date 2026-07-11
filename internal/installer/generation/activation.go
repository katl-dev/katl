package generation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zariel/katl/internal/installer/operation"
)

const (
	DefaultExtensionsActivationDir = "/run/extensions"
	DefaultConfextsActivationDir   = "/run/confexts"
	GenerationRecordsDir           = "/var/lib/katl/generations"
)

type ActivationPlan struct {
	GenerationID string
	Sysexts      []ActivationLink
	Confexts     []ActivationLink
}

type ActivationLink struct {
	Name           string
	SourcePath     string
	ActivationPath string
}

func ReadRecord(path string) (Record, error) {
	if filepath.Base(path) == "metadata.json" {
		dir := filepath.Dir(path)
		if _, err := os.Stat(filepath.Join(dir, "spec.json")); err == nil {
			spec, status, splitErr := ReadSplitRecords(dir)
			if splitErr == nil {
				return RecordFromSplit(spec, status), nil
			}
			return Record{}, splitErr
		}
	}
	record, err := readRecordFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && filepath.Base(path) == "metadata.json" {
			spec, status, splitErr := ReadSplitRecords(filepath.Dir(path))
			if splitErr == nil {
				return RecordFromSplit(spec, status), nil
			}
		}
		return Record{}, err
	}
	return record, nil
}

func readRecordFile(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, fmt.Errorf("read generation metadata: %w", err)
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("decode generation metadata: %w", err)
	}
	if err := ValidateRecord(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func MetadataPath(root string, generationID string) (string, error) {
	generationID, err := cleanSegment("generation id", generationID)
	if err != nil {
		return "", err
	}
	return rootedPath(root, filepath.Join(GenerationRecordsDir, generationID, "metadata.json"))
}

func SelectedGenerationFromCommandLine(cmdline string) (string, error) {
	for _, field := range strings.Fields(cmdline) {
		value, ok := strings.CutPrefix(field, "katl.generation=")
		if !ok {
			continue
		}
		return cleanSegment("generation id", value)
	}
	return "", fmt.Errorf("katl.generation kernel argument is required")
}

func PlanActivation(record Record) (ActivationPlan, error) {
	if err := ValidateRecord(record); err != nil {
		return ActivationPlan{}, err
	}
	generationID, err := cleanSegment("generation id", record.GenerationID)
	if err != nil {
		return ActivationPlan{}, err
	}
	plan := ActivationPlan{GenerationID: generationID}
	seen := make(map[string]struct{}, len(record.Sysexts)+len(record.Confexts))
	for _, ref := range record.Sysexts {
		source, err := cleanGenerationPath("sysext "+ref.Name, generationID, ref.Path, "sysext")
		if err != nil {
			return ActivationPlan{}, err
		}
		activation, err := cleanActivationPath("sysext "+ref.Name, ref.ActivationPath, DefaultExtensionsActivationDir)
		if err != nil {
			return ActivationPlan{}, err
		}
		if err := rememberActivation(seen, activation); err != nil {
			return ActivationPlan{}, err
		}
		plan.Sysexts = append(plan.Sysexts, ActivationLink{Name: ref.Name, SourcePath: source, ActivationPath: activation})
	}
	for _, ref := range record.Confexts {
		source, err := cleanGenerationPath("confext "+ref.Name, generationID, ref.Path, "confext")
		if err != nil {
			return ActivationPlan{}, err
		}
		activation, err := cleanActivationPath("confext "+ref.Name, ref.ActivationPath, DefaultConfextsActivationDir)
		if err != nil {
			return ActivationPlan{}, err
		}
		if err := rememberActivation(seen, activation); err != nil {
			return ActivationPlan{}, err
		}
		plan.Confexts = append(plan.Confexts, ActivationLink{Name: ref.Name, SourcePath: source, ActivationPath: activation})
	}
	return plan, nil
}

func ApplyActivation(root string, record Record) (ActivationPlan, error) {
	plan, err := PlanActivation(record)
	if err != nil {
		return ActivationPlan{}, err
	}
	if err := rejectKubernetesSysextChange(root, record); err != nil {
		return ActivationPlan{}, err
	}
	for _, ref := range record.Sysexts {
		path, err := rootedPath(root, ref.Path)
		if err != nil {
			return ActivationPlan{}, err
		}
		if err := verifyFileSHA256(path, ref.SHA256); err != nil {
			return ActivationPlan{}, fmt.Errorf("verify sysext %q: %w", ref.Name, err)
		}
	}
	for _, ref := range record.Confexts {
		path, err := rootedPath(root, ref.Path)
		if err != nil {
			return ActivationPlan{}, err
		}
		got, err := DigestDirectory(path)
		if err != nil {
			return ActivationPlan{}, fmt.Errorf("digest confext %q: %w", ref.Name, err)
		}
		if got != ref.SHA256 {
			return ActivationPlan{}, fmt.Errorf("verify confext %q: SHA-256 mismatch", ref.Name)
		}
	}
	if err := resetActivationDirs(root); err != nil {
		return ActivationPlan{}, err
	}
	for _, link := range append(append([]ActivationLink{}, plan.Sysexts...), plan.Confexts...) {
		target, err := rootedPath(root, link.ActivationPath)
		if err != nil {
			return ActivationPlan{}, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return ActivationPlan{}, fmt.Errorf("create parent for %s: %w", link.ActivationPath, err)
		}
		if err := os.Symlink(link.SourcePath, target); err != nil {
			return ActivationPlan{}, fmt.Errorf("activate %s: %w", link.ActivationPath, err)
		}
	}
	return plan, nil
}

func rejectKubernetesSysextChange(root string, record Record) error {
	nextRef, nextOK := selectedKubernetesSysext(record.Sysexts)
	if !nextOK {
		return nil
	}
	previousID, err := previousGenerationForActivation(root, record)
	if err != nil {
		return err
	}
	if previousID == "" {
		return nil
	}
	previous, err := readRecordFromRoot(root, previousID)
	if err != nil {
		return fmt.Errorf("read previous generation for Kubernetes sysext policy: %w", err)
	}
	previousRef, previousOK := selectedKubernetesSysext(previous.Sysexts)
	if previousOK && nextOK && strings.EqualFold(previousRef.SHA256, nextRef.SHA256) && previousRef.PayloadVersion == nextRef.PayloadVersion {
		return nil
	}
	if !previousOK {
		return nil
	}
	if err := authorizeKubernetesUpgradeActivation(root, record, nextRef); err == nil {
		return nil
	} else if record.KubernetesUpgrade != nil {
		return err
	}
	return fmt.Errorf("normal runtime configuration cannot activate Kubernetes sysext change from %s/%s to %s/%s before target kubeadm access mode and kubelet activation gate are implemented", previousRef.PayloadVersion, previousRef.SHA256, nextRef.PayloadVersion, nextRef.SHA256)
}

func authorizeKubernetesUpgradeActivation(root string, record Record, nextRef ExtensionRef) error {
	upgrade := record.KubernetesUpgrade
	if upgrade == nil {
		return fmt.Errorf("generation has no Kubernetes upgrade authorization")
	}
	if upgrade.TargetKubeadmAccessMode != "operation-private-sysext" || upgrade.KubeletActivationGate != "operation-released-target-kubelet" {
		return fmt.Errorf("generation %s has unsupported Kubernetes upgrade authorization %q/%q", record.GenerationID, upgrade.TargetKubeadmAccessMode, upgrade.KubeletActivationGate)
	}
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		return fmt.Errorf("open Kubernetes upgrade operation store: %w", err)
	}
	op, err := store.Read(upgrade.OperationID)
	if err != nil {
		return fmt.Errorf("read Kubernetes upgrade operation %s: %w", upgrade.OperationID, err)
	}
	if op.OperationKind != "kubeadm-upgrade" || !op.Terminal || op.Result != operation.ResultSucceeded || op.CandidateGenerationID != record.GenerationID || op.GenerationCommitState != operation.GenerationCommitCommitted {
		return fmt.Errorf("Kubernetes upgrade operation %s did not successfully commit generation %s", upgrade.OperationID, record.GenerationID)
	}
	if op.KubernetesSysextUpdate == nil || op.KubernetesSysextUpdate.TargetPayloadVersion != nextRef.PayloadVersion || !strings.EqualFold(op.KubernetesSysextUpdate.TargetSysextSHA256, nextRef.SHA256) {
		return fmt.Errorf("Kubernetes upgrade operation %s target does not match generation %s", upgrade.OperationID, record.GenerationID)
	}
	evidence := op.KubeadmUpgradeEvidence
	if evidence == nil || evidence.TargetKubeadmAccessMode != upgrade.TargetKubeadmAccessMode || evidence.KubeletActivationGate != upgrade.KubeletActivationGate || evidence.KubeletGateState != "target-observed" {
		return fmt.Errorf("Kubernetes upgrade operation %s lacks completed target kubeadm and kubelet gate evidence", upgrade.OperationID)
	}
	return nil
}

func previousGenerationForActivation(root string, record Record) (string, error) {
	if record.ConfigApply != nil && strings.TrimSpace(record.ConfigApply.PreviousGeneration) != "" {
		return strings.TrimSpace(record.ConfigApply.PreviousGeneration), nil
	}
	if strings.TrimSpace(record.PreviousGenerationID) != "" {
		return strings.TrimSpace(record.PreviousGenerationID), nil
	}
	selection, err := ReadBootSelection(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read boot selection for Kubernetes sysext policy: %w", err)
	}
	for _, candidate := range []string{selection.BootedGenerationID, selection.DefaultGenerationID, selection.TargetBootGenerationID} {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && candidate != record.GenerationID {
			return candidate, nil
		}
	}
	return "", nil
}

func readRecordFromRoot(root string, generationID string) (Record, error) {
	metadataPath, err := MetadataPath(root, generationID)
	if err != nil {
		return Record{}, err
	}
	return ReadRecord(metadataPath)
}

func selectedKubernetesSysext(refs []ExtensionRef) (ExtensionRef, bool) {
	for _, ref := range refs {
		if ref.Name == "kubernetes" {
			return ref, true
		}
	}
	return ExtensionRef{}, false
}

func resetActivationDirs(root string) error {
	for _, dir := range []string{DefaultExtensionsActivationDir, DefaultConfextsActivationDir} {
		path, err := rootedPath(root, dir)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("clear activation directory %s: %w", dir, err)
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("create activation directory %s: %w", dir, err)
		}
	}
	return nil
}

func cleanGenerationPath(name string, generationID string, value string, kind string) (string, error) {
	value = filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(strings.TrimSpace(value), "/")))
	if value == "." || value == "/" {
		return "", fmt.Errorf("%s path is required", name)
	}
	prefix := filepath.ToSlash(filepath.Join(GenerationRecordsDir, generationID, kind))
	if value != prefix && !strings.HasPrefix(value, prefix+"/") {
		return "", fmt.Errorf("%s path %q must be under %s", name, value, prefix)
	}
	return value, nil
}

func cleanActivationPath(name string, value string, root string) (string, error) {
	value = filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(strings.TrimSpace(value), "/")))
	if value == root || value == "/" {
		return "", fmt.Errorf("%s activation path must name an entry under %s", name, root)
	}
	if !strings.HasPrefix(value, root+"/") {
		return "", fmt.Errorf("%s activation path %q must be under %s", name, value, root)
	}
	return value, nil
}

func rememberActivation(seen map[string]struct{}, activation string) error {
	if _, ok := seen[activation]; ok {
		return fmt.Errorf("duplicate activation path %q", activation)
	}
	seen[activation] = struct{}{}
	return nil
}

func rootedPath(root string, absolutePath string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("target root is required")
	}
	absolutePath = filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(strings.TrimSpace(absolutePath), "/")))
	if absolutePath == "/" {
		return filepath.Clean(root), nil
	}
	return filepath.Join(filepath.Clean(root), filepath.FromSlash(strings.TrimPrefix(absolutePath, "/"))), nil
}

func verifyFileSHA256(path string, want string) error {
	if err := validateSHA256("artifact", want); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return fmt.Errorf("SHA-256 mismatch")
	}
	return nil
}
