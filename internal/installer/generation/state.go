package generation

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	KubernetesSource = "/var/lib/katl/kubernetes/etc-kubernetes"
	KubernetesTarget = "/etc/kubernetes"
)

type StateRequest struct {
	PartitionUUID string
}

type KubernetesProjectionRequest struct {
	Source string
	Target string
}

type StateAssets struct {
	VarMount           string
	EtcKubernetesMount string
	GenerationActivate string
	KubeadmReadyTarget string
	ContainerdDropIn   string
	KubeletDropIn      string
	StateCheckService  string
	RuntimeStatus      string
	OperationService   string
	OperationReconcile string
	Tmpfiles           string
	Dirs               []StateDir
	MountPoints        []StateDir
}

type StateDir struct {
	Path string
	Mode os.FileMode
}

func RenderState(request StateRequest) (StateAssets, error) {
	uuid, err := stateUUID(request.PartitionUUID)
	if err != nil {
		return StateAssets{}, err
	}
	kubernetesMount, err := RenderKubernetesProjection(KubernetesProjectionRequest{})
	if err != nil {
		return StateAssets{}, err
	}
	dirs := stateDirs()
	assets := StateAssets{
		VarMount: strings.Join([]string{
			"[Unit]",
			"Description=Katl writable state partition",
			"Documentation=man:systemd.mount(5)",
			"DefaultDependencies=no",
			"Before=local-fs.target",
			"Conflicts=umount.target",
			"Before=umount.target",
			"",
			"[Mount]",
			"What=PARTUUID=" + uuid,
			"Where=/var",
			"Type=auto",
			"Options=rw",
			"",
			"[Install]",
			"WantedBy=local-fs.target",
			"",
		}, "\n"),
		EtcKubernetesMount: kubernetesMount,
		GenerationActivate: renderGenerationActivateService(),
		KubeadmReadyTarget: renderKubeadmReadyTarget(),
		ContainerdDropIn:   renderContainerdDropIn(),
		KubeletDropIn:      renderKubeletDropIn(),
		StateCheckService:  renderStateCheckService(),
		RuntimeStatus:      renderRuntimeStatusService(),
		OperationService:   renderOperationService(),
		OperationReconcile: renderOperationReconcileService(),
		Tmpfiles:           renderTmpfiles(dirs),
		Dirs:               dirs,
		MountPoints:        []StateDir{{Path: KubernetesTarget, Mode: 0o755}},
	}
	return assets, nil
}

func RenderKubernetesProjection(request KubernetesProjectionRequest) (string, error) {
	source, target, err := kubernetesProjectionPaths(request)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		"[Unit]",
		"Description=Project persistent Kubernetes configuration",
		"Documentation=man:systemd.mount(5)",
		"DefaultDependencies=no",
		"After=var.mount systemd-confext.service",
		"Before=kubelet.service katl-kubeadm-ready.target",
		"Conflicts=umount.target",
		"Before=umount.target",
		"RequiresMountsFor=" + source,
		"",
		"[Mount]",
		"What=" + source,
		"Where=" + target,
		"Type=none",
		"Options=bind,rw",
		"",
	}, "\n"), nil
}

func renderGenerationActivateService() string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Activate selected Katl generation extensions",
		"Documentation=man:systemd-sysext(8) man:systemd-confext(8)",
		"DefaultDependencies=no",
		"Requires=var.mount",
		"After=var.mount",
		"Before=systemd-sysext.service systemd-confext.service",
		"",
		"[Service]",
		"Type=oneshot",
		"StandardOutput=journal+console",
		"SyslogIdentifier=katl-generation-activate",
		"ExecStart=/usr/lib/katl/runtime/katl-generation-activate --root=/",
		"",
		"[Install]",
		"RequiredBy=systemd-sysext.service",
		"RequiredBy=systemd-confext.service",
		"",
	}, "\n")
}

func renderKubeadmReadyTarget() string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Katl kubeadm-ready handoff point",
		"Documentation=man:systemd.target(5)",
		"Requires=systemd-sysext.service systemd-confext.service containerd.service etc-kubernetes.mount katl-state-projection-check.service katl-runtime-handoff-status.service katl-operation-reconcile.service",
		"After=systemd-sysext.service systemd-confext.service containerd.service etc-kubernetes.mount katl-state-projection-check.service katl-runtime-handoff-status.service katl-operation-reconcile.service",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
}

func renderContainerdDropIn() string {
	return strings.Join([]string{
		"[Unit]",
		"Requires=var.mount",
		"After=var.mount",
		"Before=katl-kubeadm-ready.target",
		"RequiresMountsFor=/var/lib/containerd",
		"",
	}, "\n")
}

func renderKubeletDropIn() string {
	return strings.Join([]string{
		"[Unit]",
		"Requires=containerd.service etc-kubernetes.mount",
		"After=var.mount containerd.service etc-kubernetes.mount",
		"Before=katl-kubeadm-ready.target",
		"RequiresMountsFor=/var/lib/kubelet /etc/kubernetes",
		"",
	}, "\n")
}

func renderStateCheckService() string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Check Katl writable state projections",
		"Requires=var.mount etc-kubernetes.mount",
		"After=var.mount etc-kubernetes.mount",
		"Before=katl-kubeadm-ready.target",
		"",
		"[Service]",
		"Type=oneshot",
		"StandardOutput=journal+console",
		"SyslogIdentifier=katl-state-projection",
		"ExecStart=/usr/bin/printf 'Katl state projection ready\\n'",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
}

func renderRuntimeStatusService() string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Record Katl runtime handoff status",
		"Documentation=man:systemd.service(5)",
		"Requires=katl-state-projection-check.service",
		"After=katl-state-projection-check.service",
		"Before=katl-kubeadm-ready.target",
		"",
		"[Service]",
		"Type=oneshot",
		"StandardOutput=journal+console",
		"SyslogIdentifier=katl-runtime-status",
		"ExecStart=/usr/lib/katl/runtime/katl-runtime-status --root=/",
		"",
		"[Install]",
		"RequiredBy=katl-kubeadm-ready.target",
		"",
	}, "\n")
}

func renderOperationService() string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Run Katl operation %i",
		"Documentation=man:systemd.service(5)",
		"RequiresMountsFor=/var/lib/katl/operations",
		"After=katl-operation-reconcile.service",
		"Before=katl-kubeadm-ready.target",
		"",
		"[Service]",
		"Type=oneshot",
		"StandardOutput=journal+console",
		"StandardError=journal+console",
		"SyslogIdentifier=katl-operation",
		"Environment=KATL_OPERATION_ID=%i",
		"Environment=KATL_OPERATION_UNIT=katl-operation@%i.service",
		"ExecStart=/usr/bin/katlc operation execute --operation-id %i --root=/",
		"TimeoutStartSec=30min",
		"",
	}, "\n")
}

func renderOperationReconcileService() string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Reconcile Katl operation records",
		"Documentation=man:systemd.service(5)",
		"Requires=var.mount katl-generation-activate.service",
		"RequiresMountsFor=/var/lib/katl/operations",
		"After=local-fs.target var.mount katl-generation-activate.service systemd-sysext.service systemd-confext.service",
		"Before=katl-kubeadm-ready.target katl-boot-complete.target katl-operation@.service",
		"",
		"[Service]",
		"Type=oneshot",
		"StandardOutput=journal+console",
		"StandardError=journal+console",
		"SyslogIdentifier=katl-operation-reconcile",
		"ExecStart=/usr/bin/katlc operation reconcile --boot --root=/",
		"",
		"[Install]",
		"RequiredBy=katl-kubeadm-ready.target",
		"",
	}, "\n")
}

func WriteState(root string, request StateRequest) (StateAssets, error) {
	if strings.TrimSpace(root) == "" {
		return StateAssets{}, fmt.Errorf("target root is required")
	}
	assets, err := RenderState(request)
	if err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/systemd/system/var.mount", assets.VarMount, 0o644); err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/systemd/system/etc-kubernetes.mount", assets.EtcKubernetesMount, 0o644); err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/systemd/system/katl-generation-activate.service", assets.GenerationActivate, 0o644); err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/systemd/system/katl-kubeadm-ready.target", assets.KubeadmReadyTarget, 0o644); err != nil {
		return StateAssets{}, err
	}
	if err := writeSymlink(root, "etc/systemd/system/multi-user.target.wants/katl-kubeadm-ready.target", "../katl-kubeadm-ready.target"); err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/systemd/system/containerd.service.d/10-katl-runtime.conf", assets.ContainerdDropIn, 0o644); err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/systemd/system/kubelet.service.d/10-katl-runtime.conf", assets.KubeletDropIn, 0o644); err != nil {
		return StateAssets{}, err
	}
	if err := writeSymlink(root, "etc/systemd/system/systemd-sysext.service.requires/katl-generation-activate.service", "../katl-generation-activate.service"); err != nil {
		return StateAssets{}, err
	}
	if err := writeSymlink(root, "etc/systemd/system/systemd-confext.service.requires/katl-generation-activate.service", "../katl-generation-activate.service"); err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/systemd/system/katl-state-projection-check.service", assets.StateCheckService, 0o644); err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/systemd/system/katl-runtime-handoff-status.service", assets.RuntimeStatus, 0o644); err != nil {
		return StateAssets{}, err
	}
	if err := writeSymlink(root, "etc/systemd/system/katl-kubeadm-ready.target.requires/katl-runtime-handoff-status.service", "../katl-runtime-handoff-status.service"); err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/systemd/system/katl-operation@.service", assets.OperationService, 0o644); err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/systemd/system/katl-operation-reconcile.service", assets.OperationReconcile, 0o644); err != nil {
		return StateAssets{}, err
	}
	if err := writeSymlink(root, "etc/systemd/system/katl-kubeadm-ready.target.requires/katl-operation-reconcile.service", "../katl-operation-reconcile.service"); err != nil {
		return StateAssets{}, err
	}
	if err := writeFile(root, "etc/tmpfiles.d/katl-state.conf", assets.Tmpfiles, 0o644); err != nil {
		return StateAssets{}, err
	}
	for _, dir := range append(append([]StateDir{}, assets.Dirs...), assets.MountPoints...) {
		path := filepath.Join(root, strings.TrimPrefix(dir.Path, "/"))
		if err := os.MkdirAll(path, dir.Mode); err != nil {
			return StateAssets{}, fmt.Errorf("create %s: %w", dir.Path, err)
		}
		if err := os.Chmod(path, dir.Mode); err != nil {
			return StateAssets{}, fmt.Errorf("chmod %s: %w", dir.Path, err)
		}
	}
	return assets, nil
}

func stateDirs() []StateDir {
	return []StateDir{
		{Path: "/var/lib/katl", Mode: 0o755},
		{Path: "/var/lib/katl/boot", Mode: 0o755},
		{Path: "/var/lib/katl/generations", Mode: 0o755},
		{Path: "/var/lib/katl/install", Mode: 0o755},
		{Path: "/var/lib/katl/install/logs", Mode: 0o755},
		{Path: "/var/lib/katl/identity", Mode: 0o755},
		{Path: "/var/lib/katl/operations", Mode: 0o750},
		{Path: "/var/lib/katl/cluster", Mode: 0o750},
		{Path: "/var/lib/katl/config-requests", Mode: 0o750},
		{Path: "/var/lib/katl/kubernetes", Mode: 0o755},
		{Path: KubernetesSource, Mode: 0o755},
		{Path: "/var/lib/katl/ssh", Mode: 0o755},
		{Path: "/var/lib/katl/ssh/host-keys", Mode: 0o700},
		{Path: "/var/lib/containerd", Mode: 0o755},
		{Path: "/var/lib/etcd", Mode: 0o755},
		{Path: "/var/lib/kubelet", Mode: 0o755},
		{Path: "/var/log/journal", Mode: 0o2755},
	}
}

func renderTmpfiles(dirs []StateDir) string {
	lines := make([]string, 0, len(dirs)+1)
	lines = append(lines, "# Katl writable state seed directories")
	for _, dir := range dirs {
		group := "root"
		if dir.Path == "/var/log/journal" {
			group = "systemd-journal"
		}
		lines = append(lines, fmt.Sprintf("d %s %04o root %s -", dir.Path, dir.Mode, group))
	}
	return strings.Join(append(lines, ""), "\n")
}

func stateUUID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("state partition UUID is required")
	}
	if strings.ContainsAny(value, " \t\n\r") {
		return "", fmt.Errorf("state partition UUID must not contain whitespace")
	}
	return value, nil
}

func kubernetesProjectionPaths(request KubernetesProjectionRequest) (string, string, error) {
	source := cleanProjectionPath(request.Source, KubernetesSource)
	target := cleanProjectionPath(request.Target, KubernetesTarget)
	if source != KubernetesSource {
		return "", "", fmt.Errorf("kubernetes source must be %s", KubernetesSource)
	}
	if target != KubernetesTarget {
		return "", "", fmt.Errorf("kubernetes target must be %s", KubernetesTarget)
	}
	return source, target, nil
}

func cleanProjectionPath(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	return path.Clean("/" + strings.TrimPrefix(value, "/"))
}

func writeFile(root string, rel string, content string, mode os.FileMode) error {
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent for %s: %w", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	return nil
}

func writeSymlink(root string, rel string, target string) error {
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent for %s: %w", rel, err)
	}
	current, err := os.Readlink(path)
	if err == nil {
		if current == target {
			return nil
		}
		return fmt.Errorf("symlink %s points to %s, want %s", rel, current, target)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", rel, err)
	}
	if err := os.Symlink(target, path); err != nil {
		return fmt.Errorf("link %s: %w", rel, err)
	}
	return nil
}
