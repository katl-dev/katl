package bootstrapruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/bootstrapplan"
	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/generation"
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
	sysexts, err := rehomePreservedSysexts(root, candidate, plan.Previous)
	if err != nil {
		return Result{}, err
	}
	sysexts = append(sysexts, sysext)
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
		Sysexts:            sysexts,
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
	if data, readErr := os.ReadFile(filepath.Join(filepath.Clean(root), "proc/cmdline")); readErr == nil {
		next.KernelCommandLine = generation.MergeKernelCommandLine(next.KernelCommandLine, strings.Fields(string(data)))
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

func rehomePreservedSysexts(root string, candidate string, previous generation.GenerationSpec) ([]generation.ExtensionRef, error) {
	var preserved []generation.ExtensionRef
	for _, ref := range previous.Sysexts {
		if ref.Name == "kubernetes" {
			continue
		}
		source := filepath.Join(filepath.Clean(root), strings.TrimPrefix(filepath.Clean(ref.Path), string(filepath.Separator)))
		targetRuntime := filepath.Join("/var/lib/katl/generations", candidate, "sysext", filepath.Base(ref.Path))
		target := filepath.Join(filepath.Clean(root), strings.TrimPrefix(targetRuntime, string(filepath.Separator)))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, fmt.Errorf("create preserved sysext directory: %w", err)
		}
		in, err := os.Open(source)
		if err != nil {
			return nil, fmt.Errorf("open preserved sysext %s: %w", ref.Name, err)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			_ = in.Close()
			return nil, fmt.Errorf("create preserved sysext %s: %w", ref.Name, err)
		}
		digest := sha256.New()
		_, copyErr := copySysext(io.MultiWriter(out, digest), in)
		closeErr := errors.Join(in.Close(), out.Close())
		if copyErr != nil || closeErr != nil {
			_ = os.Remove(target)
			return nil, fmt.Errorf("copy preserved sysext %s: %w", ref.Name, errors.Join(copyErr, closeErr))
		}
		if got := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(got, ref.SHA256) {
			_ = os.Remove(target)
			return nil, fmt.Errorf("preserved sysext %s sha256 %s does not match %s", ref.Name, got, ref.SHA256)
		}
		ref.Path = filepath.ToSlash(targetRuntime)
		preserved = append(preserved, ref)
	}
	return preserved, nil
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
	in, err := os.Open(source)
	if err != nil {
		return generation.ExtensionRef{}, fmt.Errorf("open bundled Kubernetes sysext: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return generation.ExtensionRef{}, fmt.Errorf("create bootstrap sysext: %w", err)
	}
	digest := sha256.New()
	written, copyErr := copySysext(io.MultiWriter(out, digest), in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		return generation.ExtensionRef{}, fmt.Errorf("write bootstrap sysext: %w", errors.Join(copyErr, closeErr))
	}
	if selected.SizeBytes > 0 && uint64(written) != selected.SizeBytes {
		_ = os.Remove(target)
		return generation.ExtensionRef{}, fmt.Errorf("write bootstrap sysext: size %d does not match selected size %d", written, selected.SizeBytes)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(got, selected.SHA256) {
		_ = os.Remove(target)
		return generation.ExtensionRef{}, fmt.Errorf("write bootstrap sysext: sha256 %s does not match selected digest %s", got, selected.SHA256)
	}
	artifactVersion := strings.TrimSpace(selected.ArtifactVersion)
	if artifactVersion == "" {
		artifactVersion = "bootstrap-" + selected.PayloadVersion
	}
	return generation.ExtensionRef{
		Name:            "kubernetes",
		Path:            targetRuntime,
		ActivationPath:  selected.ActivationPath,
		SHA256:          selected.SHA256,
		ArtifactVersion: artifactVersion,
		PayloadVersion:  selected.PayloadVersion,
		Architecture:    selected.Architecture,
		Compatibility: generation.ExtensionCompatibility{
			RuntimeInterfaces: []string{selected.RuntimeInterface},
		},
	}, nil
}

func copySysext(dst io.Writer, src io.Reader) (int64, error) {
	return io.CopyBuffer(dst, src, make([]byte, 64<<10))
}

func runtimeFiles(root string, plan bootstrapplan.Plan) ([]confext.NativeEtcFile, error) {
	files, err := inheritedConfextFiles(root, plan.Previous)
	if err != nil {
		return nil, err
	}
	inputs, digest, err := readStoredKubeadmInput(root, plan.RuntimeInputs.KubeadmInput)
	if err != nil {
		return nil, err
	}
	if expected := strings.TrimSpace(plan.RuntimeInputs.KubeadmInput.Digest); expected != "" && digest != expected {
		return nil, fmt.Errorf("stored kubeadm input digest %s does not match intent %s", digest, expected)
	}
	for _, input := range inputs {
		content := input.Content
		if plan.Operation.OperationKind == bootstrapplan.OperationKindInit && input.RenderPath == plan.RuntimeInputs.KubeadmInput.Path {
			rendered, err := cluster.RenderInitConfig(input.Content, plan.RuntimeInputs.HostConfig.ControlPlaneEndpoint, plan.RuntimeInputs.HostConfig.ControlPlaneEndpointVIP)
			if err != nil {
				return nil, err
			}
			content = rendered
		}
		files = replaceNativeEtcFile(files, confext.NativeEtcFile{
			Path:    input.RenderPath,
			Content: string(content),
			Mode:    input.Mode,
		})
	}
	metadata, err := nodeMetadata(plan)
	if err != nil {
		return nil, err
	}
	files = replaceNativeEtcFile(files, metadata)
	return files, nil
}

func inheritedConfextFiles(root string, previous generation.GenerationSpec) ([]confext.NativeEtcFile, error) {
	if len(previous.Confexts) != 1 {
		return nil, fmt.Errorf("previous generation %s must have exactly one confext", previous.GenerationID)
	}
	expected := filepath.ToSlash(filepath.Join("/var/lib/katl/generations", previous.GenerationID, "confext"))
	declared := filepath.ToSlash(filepath.Clean(previous.Confexts[0].Path))
	if declared != expected {
		return nil, fmt.Errorf("previous generation confext path %s does not match %s", declared, expected)
	}
	confextRoot := filepath.Join(filepath.Clean(root), filepath.FromSlash(strings.TrimPrefix(declared, "/")))
	etcRoot := filepath.Join(confextRoot, "etc")
	var files []confext.NativeEtcFile
	if err := filepath.WalkDir(etcRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(etcRoot, path)
		if err != nil {
			return err
		}
		if rel == "extension-release.d" || strings.HasPrefix(filepath.ToSlash(rel), "extension-release.d/") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("previous confext entry %s is not a regular file", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, confext.NativeEtcFile{
			Path:    "/etc/" + filepath.ToSlash(rel),
			Content: string(content),
			Mode:    info.Mode().Perm(),
		})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read previous generation confext: %w", err)
	}
	return files, nil
}

func replaceNativeEtcFile(files []confext.NativeEtcFile, replacement confext.NativeEtcFile) []confext.NativeEtcFile {
	path := filepath.Clean(replacement.Path)
	for i := range files {
		if filepath.Clean(files[i].Path) == path {
			files[i] = replacement
			return files
		}
	}
	return append(files, replacement)
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
