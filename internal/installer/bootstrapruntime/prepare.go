package bootstrapruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zariel/katl/internal/installer"
	"github.com/zariel/katl/internal/installer/bootstrapplan"
	"github.com/zariel/katl/internal/installer/confext"
	"github.com/zariel/katl/internal/installer/generation"
)

type Result struct {
	Record         generation.Record
	Spec           generation.GenerationSpec
	Status         generation.GenerationStatus
	ActivationPlan generation.ActivationPlan
	KubeadmPath    string
}

type storedInputFile struct {
	Rel        string
	RenderPath string
	Content    []byte
	Mode       os.FileMode
}

func Prepare(root string, plan bootstrapplan.Plan, now time.Time) (Result, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return Result{}, fmt.Errorf("runtime root is required")
	}
	record := plan.Operation
	if record.BootstrapRequest == nil {
		return Result{}, fmt.Errorf("operation bootstrapRequest is required")
	}
	candidate := strings.TrimSpace(record.CandidateGenerationID)
	if candidate == "" {
		candidate = strings.TrimSpace(record.BootstrapRequest.CandidateGenerationID)
	}
	if candidate == "" || candidate == "0" {
		return Result{}, fmt.Errorf("candidate generation id must name a non-zero generation")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	files, err := runtimeFiles(root, plan)
	if err != nil {
		return Result{}, err
	}
	release, err := confextRelease(plan.Previous)
	if err != nil {
		return Result{}, err
	}
	sysext, err := materializeSysext(root, candidate, plan.RuntimeInputs.SelectedKubernetesSysext)
	if err != nil {
		return Result{}, err
	}
	// Installed runtimes already carry the baseline units in /usr/lib; live
	// operation preparation may run with immutable /etc.
	if _, err := generation.WriteState(root, generation.StateRequest{PartitionUUID: plan.Previous.Root.PartitionUUID}); err != nil && !errors.Is(err, syscall.EROFS) {
		return Result{}, fmt.Errorf("write bootstrap runtime systemd state: %w", err)
	}
	tree, err := confext.RenderGenerationTree(confext.GenerationTreeRequest{
		GenerationsRoot: filepath.Join(filepath.Clean(root), "var/lib/katl/generations"),
		GenerationID:    candidate,
		Files:           files,
		Extension:       release,
		Chown:           func(string, int, int) error { return nil },
	})
	if err != nil {
		return Result{}, fmt.Errorf("render bootstrap runtime confext: %w", err)
	}
	confextDigest, err := generation.DigestDirectory(tree.ConfextDir)
	if err != nil {
		return Result{}, fmt.Errorf("digest bootstrap runtime confext: %w", err)
	}
	generated := generation.GeneratedConfext{
		Name:           "katl-node",
		Path:           "/var/lib/katl/generations/" + candidate + "/confext",
		ActivationPath: "/run/confexts/katl-node",
		SHA256:         confextDigest,
		Compatibility: generation.ConfextCompatibility{
			ID:           release.ID,
			VersionID:    release.VersionID,
			ConfextLevel: release.ConfextLevel,
		},
	}
	next, err := generation.NewRuntimeConfigRecord(generation.RuntimeConfigRequest{
		GenerationID:       candidate,
		Previous:           generation.RecordFromSplit(plan.Previous, plan.PreviousState),
		SourceDigest:       record.RequestDigest,
		Sysexts:            []generation.ExtensionRef{sysext},
		GeneratedConfext:   generated,
		ChangedDomains:     []string{"bootstrap-runtime", "kubeadm-input", "kubernetes-sysext"},
		RequestedApplyMode: generation.ApplyModeLive,
		AcceptedApplyMode:  generation.ApplyModeLive,
		Kubeadm: generation.KubeadmActionRequired{
			Required: true,
			Reason:   "bootstrap operation requires kubeadm " + kubeadmPhase(record.OperationKind),
		},
		CreatedAt: now,
	})
	if err != nil {
		return Result{}, err
	}
	next.Boot.LoaderEntryPath = "loader/entries/katl-" + next.GenerationID + ".conf"
	spec := generation.SpecFromRecord(next)
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCandidate, generation.BootStatePending, generation.HealthStateUnknown, now)
	if err != nil {
		return Result{}, err
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		return Result{}, err
	}
	metadataPath, err := generation.MetadataPath(root, candidate)
	if err != nil {
		return Result{}, err
	}
	if err := generation.WriteRecord(metadataPath, next); err != nil {
		return Result{}, err
	}
	if err := writeLiveActivationOverride(root, candidate); err != nil {
		return Result{}, err
	}
	activation, err := generation.ApplyActivation(root, next)
	if err != nil {
		return Result{}, fmt.Errorf("activate bootstrap runtime generation: %w", err)
	}
	return Result{
		Record:         next,
		Spec:           spec,
		Status:         status,
		ActivationPlan: activation,
		KubeadmPath:    plan.RuntimeInputs.KubeadmInput.Path,
	}, nil
}

func writeLiveActivationOverride(root string, candidate string) error {
	if _, err := generation.MetadataPath(root, candidate); err != nil {
		return err
	}
	path := filepath.Join(filepath.Clean(root), "run/systemd/system/katl-generation-activate.service.d/10-katl-live-generation.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create live generation activation override: %w", err)
	}
	content := strings.Join([]string{
		"[Service]",
		"ExecStart=",
		"ExecStart=/usr/lib/katl/runtime/katl-generation-activate --root=/ --generation " + candidate,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write live generation activation override: %w", err)
	}
	return nil
}

func confextRelease(previous generation.GenerationSpec) (confext.ExtensionRelease, error) {
	for _, ref := range previous.Confexts {
		if ref.Name == "katl-node" {
			compat := ref.Compatibility
			return confext.ExtensionRelease{
				Name:         "katl-node",
				ID:           compat.ID,
				VersionID:    compat.VersionID,
				ConfextLevel: compat.ConfextLevel,
			}, nil
		}
	}
	return confext.ExtensionRelease{}, fmt.Errorf("previous generation katl-node confext compatibility is required")
}

func materializeSysext(root string, candidate string, selected bootstrapplan.SelectedKubernetesSysext) (generation.ExtensionRef, error) {
	source := filepath.Join(filepath.Clean(root), strings.TrimPrefix(filepath.Clean(selected.Path), string(filepath.Separator)))
	name := filepath.Base(source)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return generation.ExtensionRef{}, fmt.Errorf("bundled Kubernetes sysext path is invalid")
	}
	targetRuntime := "/var/lib/katl/generations/" + candidate + "/sysext/" + name
	target := filepath.Join(filepath.Clean(root), strings.TrimPrefix(targetRuntime, "/"))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return generation.ExtensionRef{}, fmt.Errorf("create bootstrap sysext directory: %w", err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return generation.ExtensionRef{}, fmt.Errorf("read bundled Kubernetes sysext: %w", err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return generation.ExtensionRef{}, fmt.Errorf("write bootstrap sysext: %w", err)
	}
	return generation.ExtensionRef{
		Name:            "kubernetes",
		Path:            targetRuntime,
		ActivationPath:  selected.ActivationPath,
		SHA256:          selected.SHA256,
		ArtifactVersion: "bootstrap-" + selected.PayloadVersion,
		PayloadVersion:  selected.PayloadVersion,
		Architecture:    selected.Architecture,
		Compatibility: generation.ExtensionCompatibility{
			RuntimeInterfaces: []string{selected.RuntimeInterface},
		},
	}, nil
}

func runtimeFiles(root string, plan bootstrapplan.Plan) ([]confext.NativeEtcFile, error) {
	inputs, digest, err := readStoredKubeadmInput(root, plan.RuntimeInputs.KubeadmInput)
	if err != nil {
		return nil, err
	}
	if expected := strings.TrimSpace(plan.RuntimeInputs.KubeadmInput.Digest); expected != "" && digest != expected {
		return nil, fmt.Errorf("stored kubeadm input digest %s does not match intent %s", digest, expected)
	}
	var files []confext.NativeEtcFile
	for _, input := range inputs {
		files = append(files, confext.NativeEtcFile{
			Path:    input.RenderPath,
			Content: string(input.Content),
			Mode:    input.Mode,
		})
	}
	metadata, err := nodeMetadata(plan)
	if err != nil {
		return nil, err
	}
	files = append(files, metadata)
	return files, nil
}

func readStoredKubeadmInput(root string, input bootstrapplan.KubeadmInput) ([]storedInputFile, string, error) {
	ref := strings.TrimSpace(input.ConfigRef)
	dir, err := installer.StoredKubeadmInputDir(root, ref)
	if err != nil {
		return nil, "", err
	}
	configPath, err := cleanRenderPath(input.Path)
	if err != nil {
		return nil, "", err
	}
	var inputs []storedInputFile
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("stored kubeadm input %s is not a regular file", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return fmt.Errorf("stored kubeadm input %s escapes input directory", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		inputs = append(inputs, storedInputFile{
			Rel:        filepath.ToSlash(rel),
			RenderPath: filepath.ToSlash(filepath.Join("/etc/katl/kubeadm", ref, rel)),
			Content:    data,
			Mode:       mode,
		})
		return nil
	}); err != nil {
		return nil, "", fmt.Errorf("read stored kubeadm input: %w", err)
	}
	if len(inputs) == 0 {
		return nil, "", fmt.Errorf("stored kubeadm input %q is empty", ref)
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].RenderPath < inputs[j].RenderPath })
	configIndex := -1
	for i, file := range inputs {
		if file.RenderPath == configPath {
			configIndex = i
			break
		}
	}
	if configIndex < 0 {
		return nil, "", fmt.Errorf("stored kubeadm input missing config %s", configPath)
	}
	hash := sha256.New()
	writeStoredDigestFile(hash, "config", inputs[configIndex])
	for i, file := range inputs {
		if i == configIndex {
			continue
		}
		writeStoredDigestFile(hash, "patch", file)
	}
	return inputs, hex.EncodeToString(hash.Sum(nil)), nil
}

func cleanRenderPath(path string) (string, error) {
	path = filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(strings.TrimSpace(path), "/")))
	if path == "/" || !strings.HasPrefix(path, "/etc/katl/kubeadm/") {
		return "", fmt.Errorf("kubeadm input path %q must be under /etc/katl/kubeadm", path)
	}
	return path, nil
}

func writeStoredDigestFile(hash hash.Hash, kind string, file storedInputFile) {
	fmt.Fprintf(hash, "%s\x00%s\x00%#o\x00", kind, file.RenderPath, file.Mode)
	hash.Write(file.Content)
	hash.Write([]byte{0})
}

func nodeMetadata(plan bootstrapplan.Plan) (confext.NativeEtcFile, error) {
	data, err := json.MarshalIndent(struct {
		APIVersion string                             `json:"apiVersion"`
		Kind       string                             `json:"kind"`
		Host       bootstrapplan.HostConfig           `json:"host"`
		Kubeadm    bootstrapplan.KubeadmInput         `json:"kubeadm"`
		Kubernetes bootstrapplan.KubernetesProjection `json:"kubernetesProjection"`
	}{
		APIVersion: "katl.dev/v1alpha1",
		Kind:       "BootstrapRuntime",
		Host:       plan.RuntimeInputs.HostConfig,
		Kubeadm:    plan.RuntimeInputs.KubeadmInput,
		Kubernetes: plan.RuntimeInputs.KubernetesProjection,
	}, "", "  ")
	if err != nil {
		return confext.NativeEtcFile{}, fmt.Errorf("marshal bootstrap runtime metadata: %w", err)
	}
	return confext.NativeEtcFile{
		Path:    "/etc/katl/bootstrap-runtime.json",
		Content: string(append(data, '\n')),
		Mode:    0o644,
	}, nil
}

func kubeadmPhase(kind string) string {
	switch kind {
	case bootstrapplan.OperationKindInit:
		return "init"
	case bootstrapplan.OperationKindJoinWorker:
		return "join"
	default:
		return "operation"
	}
}
