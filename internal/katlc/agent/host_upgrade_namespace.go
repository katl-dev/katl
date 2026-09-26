package agent

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/kernelcmdline"
)

// preparedUpgrade is a private snapshot and a candidate produced by the target
// release. It is removed after validation or publication.
type preparedUpgrade struct {
	work   string
	root   string
	result preparedUpgradeResult
}

func (p preparedUpgrade) close() { _ = os.RemoveAll(p.work) }

func (e *Executor) prepareUpgradeInNamespace(ctx context.Context, payload katlosimage.Payload, handoff generation.UpgradeHandoff) (preparedUpgrade, error) {
	if payload.ImagePath == "" {
		return preparedUpgrade{}, fmt.Errorf("target preparation requires a verified image file")
	}
	base := filepath.Join(runtimeRoot(e.Root), "var/lib/katl/artifacts/host-upgrade")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return preparedUpgrade{}, err
	}
	work, err := os.MkdirTemp(base, "prepare-")
	if err != nil {
		return preparedUpgrade{}, err
	}
	p := preparedUpgrade{work: work, root: filepath.Join(work, "state")}
	ok := false
	defer func() {
		if !ok {
			p.close()
		}
	}()
	if err := snapshotUpgradeSource(e.Root, p.root, handoff); err != nil {
		return preparedUpgrade{}, fmt.Errorf("snapshot source generation: %w", err)
	}
	if err := generation.WriteUpgradeHandoff(p.root, handoff); err != nil {
		return preparedUpgrade{}, err
	}
	previous, _, err := generation.ReadGeneration(p.root, handoff.SourceGenerationID)
	if err != nil {
		return preparedUpgrade{}, err
	}
	bootRecord := generation.Record{
		GenerationID: handoff.CandidateGenerationID, RuntimeVersion: payload.Index.Version,
		Root:              generation.RootSelection{Slot: handoff.RootSlot, PartitionUUID: handoff.RootPartitionUUID, Flavour: payload.Index.Flavour},
		Boot:              generation.BootSelection{UKIPath: handoff.UKIPath, LoaderEntryPath: handoff.LoaderEntryPath},
		KernelCommandLine: kernelcmdline.MergeCurrent(payload.Boot.Compatibility.KernelCommandLine, previous.KernelCommandLine, nil),
		CreatedAt:         handoff.CreatedAt,
	}
	// The source writes the exact planned entry into the private view. The target
	// planner must reproduce it byte for byte before any live boot mutation.
	machineID, err := os.ReadFile(filepath.Join(p.root, "etc/machine-id"))
	if err != nil {
		return preparedUpgrade{}, err
	}
	if _, err := generation.WriteEntry(filepath.Join(p.root, "efi"), generation.LoaderRequest{Record: bootRecord, MachineID: strings.TrimSpace(string(machineID))}); err != nil {
		return preparedUpgrade{}, err
	}
	imageRoot := payload.Root
	if imageRoot == "" {
		return preparedUpgrade{}, fmt.Errorf("verified target image is not mounted")
	}
	program := filepath.Join(work, "target-prepare")
	if err := extractTargetPlanner(ctx, payload.ComponentPath(payload.Runtime), program); err != nil {
		return preparedUpgrade{}, err
	}
	if err := os.Chmod(program, 0o500); err != nil {
		return preparedUpgrade{}, err
	}
	args := []string{
		"--wait", "--pipe", "--collect", "--service-type=exec",
		"--property=PrivateNetwork=yes", "--property=PrivateDevices=yes",
		"--property=ProtectSystem=strict", "--property=ProtectHome=yes",
		"--property=NoNewPrivileges=yes", "--property=RestrictNamespaces=yes",
		"--property=CapabilityBoundingSet=", "--property=AmbientCapabilities=",
		"--property=ReadWritePaths=" + work,
		"--property=InaccessiblePaths=/efi /sys/firmware/efi",
		"--", program, "--root=" + p.root,
		"--generation=" + handoff.CandidateGenerationID,
		"--prepare-upgrade-image-root=" + imageRoot,
		"--prepare-upgrade-source=" + handoff.SourceGenerationID,
		"--prepare-upgrade-operation=" + handoff.OperationID,
	}
	command := exec.CommandContext(ctx, "systemd-run", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return preparedUpgrade{}, fmt.Errorf("target generation preparation failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	result, err := readPreparedUpgradeResult(p.root, handoff, payload)
	if err != nil {
		return preparedUpgrade{}, err
	}
	p.result = result
	ok = true
	return p, nil
}

func extractTargetPlanner(ctx context.Context, rootImage, destination string) error {
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "unsquashfs", "-cat", rootImage, "usr/lib/katl/runtime/katl-generation-activate")
	command.Stdout = out
	var stderr bytes.Buffer
	command.Stderr = &stderr
	runErr := command.Run()
	closeErr := out.Close()
	if runErr != nil || closeErr != nil {
		return fmt.Errorf("extract target planner: %v %v: %s", runErr, closeErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func snapshotUpgradeSource(source, target string, handoff generation.UpgradeHandoff) error {
	for _, dir := range []string{"var/lib/katl/generations", "var/lib/katl/operations"} {
		if err := copyUpgradeTree(filepath.Join(runtimeRoot(source), dir), filepath.Join(target, dir)); err != nil {
			return err
		}
	}
	for _, name := range []string{"etc/machine-id", "var/lib/katl/install/manifest.json"} {
		from := filepath.Join(runtimeRoot(source), name)
		if _, err := os.Stat(from); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		to := filepath.Join(target, name)
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		if err := copyUpgradeComponent(from, to); err != nil {
			return err
		}
	}
	for _, name := range []string{"etc/kubernetes/admin.conf", "etc/kubernetes/manifests/kube-apiserver.yaml", "etc/kubernetes/kubelet.conf", "var/lib/kubelet/config.yaml", "var/lib/etcd/member"} {
		from := filepath.Join(runtimeRoot(source), name)
		info, err := os.Stat(from)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		to := filepath.Join(target, name)
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.Mkdir(to, 0o700); err != nil {
				return err
			}
		} else if err := os.WriteFile(to, nil, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func copyUpgradeTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		to := filepath.Join(target, rel)
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(to, info.Mode().Perm())
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, to)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file in upgrade tree: %s", path)
		}
		if err := copyUpgradeComponent(path, to); err != nil {
			return err
		}
		// Confext directory digests include file mode, so snapshot and
		// publication must preserve it exactly.
		return os.Chmod(to, info.Mode().Perm())
	})
}

// Publication occurs on the state filesystem in one rename. Until that rename,
// generation enumeration cannot observe an incomplete candidate.
func publishPreparedUpgrade(root string, prepared preparedUpgrade, candidate string) error {
	source, err := generation.GenerationDir(prepared.root, candidate)
	if err != nil {
		return err
	}
	destination, err := generation.GenerationDir(root, candidate)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return fmt.Errorf("candidate generation already exists: %v", err)
	}
	stage := filepath.Join(prepared.work, "publish", candidate)
	if err := copyUpgradeTree(source, stage); err != nil {
		return err
	}
	digest, err := generation.DigestDirectory(stage)
	if err != nil {
		return fmt.Errorf("digest copied candidate: %w", err)
	}
	if digest != prepared.result.CandidateSHA256 {
		return fmt.Errorf("copied candidate digest mismatch")
	}
	if err := syncUpgradeTree(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func syncUpgradeTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
