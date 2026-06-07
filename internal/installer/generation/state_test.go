package generation

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/nspawntest"
)

const statePartUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func TestRenderState(t *testing.T) {
	assets, err := RenderState(StateRequest{PartitionUUID: statePartUUID})
	if err != nil {
		t.Fatalf("RenderState() error = %v", err)
	}
	wantMount := `[Unit]
Description=Katl writable state partition
Documentation=man:systemd.mount(5)
DefaultDependencies=no
Before=local-fs.target
Conflicts=umount.target
Before=umount.target

[Mount]
What=PARTUUID=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee
Where=/var
Type=auto
Options=rw

[Install]
WantedBy=local-fs.target
`
	if assets.VarMount != wantMount {
		t.Fatalf("var.mount:\n%s\nwant:\n%s", assets.VarMount, wantMount)
	}
	for _, want := range []string{
		"d /var/lib/katl 0755 root root -",
		"d /var/lib/katl/generations 0755 root root -",
		"d /var/lib/katl/install/logs 0755 root root -",
		"d /var/lib/katl/kubernetes/etc-kubernetes 0755 root root -",
		"d /var/lib/containerd 0755 root root -",
		"d /var/lib/kubelet 0755 root root -",
		"d /var/log/journal 2755 root systemd-journal -",
	} {
		if !strings.Contains(assets.Tmpfiles, want) {
			t.Fatalf("tmpfiles missing %q:\n%s", want, assets.Tmpfiles)
		}
	}
	if strings.Contains(assets.Tmpfiles, "/etc/kubernetes") {
		t.Fatalf("tmpfiles must not create mutable /etc projection target:\n%s", assets.Tmpfiles)
	}
}

func TestRenderKubernetesProjection(t *testing.T) {
	unit, err := RenderKubernetesProjection(KubernetesProjectionRequest{})
	if err != nil {
		t.Fatalf("RenderKubernetesProjection() error = %v", err)
	}
	want := `[Unit]
Description=Project persistent Kubernetes configuration
Documentation=man:systemd.mount(5)
DefaultDependencies=no
After=var.mount systemd-confext.service
Before=kubelet.service katl-kubeadm-ready.target
Conflicts=umount.target
Before=umount.target
RequiresMountsFor=/var/lib/katl/kubernetes/etc-kubernetes

[Mount]
What=/var/lib/katl/kubernetes/etc-kubernetes
Where=/etc/kubernetes
Type=none
Options=bind,rw
`
	if unit != want {
		t.Fatalf("etc-kubernetes.mount:\n%s\nwant:\n%s", unit, want)
	}
}

func TestStateCheckService(t *testing.T) {
	assets, err := RenderState(StateRequest{PartitionUUID: statePartUUID})
	if err != nil {
		t.Fatalf("RenderState() error = %v", err)
	}
	want := `[Unit]
Description=Check Katl writable state projections
Requires=var.mount etc-kubernetes.mount
After=var.mount etc-kubernetes.mount
Before=katl-kubeadm-ready.target

[Service]
Type=oneshot
StandardOutput=journal+console
SyslogIdentifier=katl-state-projection
ExecStart=/usr/bin/printf 'Katl state projection ready\n'

[Install]
WantedBy=multi-user.target
`
	if assets.StateCheckService != want {
		t.Fatalf("katl-state-projection-check.service:\n%s\nwant:\n%s", assets.StateCheckService, want)
	}
}

func TestRuntimeStatusService(t *testing.T) {
	assets, err := RenderState(StateRequest{PartitionUUID: statePartUUID})
	if err != nil {
		t.Fatalf("RenderState() error = %v", err)
	}
	want := `[Unit]
Description=Record Katl runtime handoff status
Documentation=man:systemd.service(5)
Requires=katl-state-projection-check.service
After=katl-state-projection-check.service
Before=katl-kubeadm-ready.target

[Service]
Type=oneshot
StandardOutput=journal+console
SyslogIdentifier=katl-runtime-status
ExecStart=/usr/lib/katl/runtime/katl-runtime-status --root=/

[Install]
RequiredBy=katl-kubeadm-ready.target
`
	if assets.RuntimeStatus != want {
		t.Fatalf("katl-runtime-handoff-status.service:\n%s\nwant:\n%s", assets.RuntimeStatus, want)
	}
}

func TestGenerationActivationService(t *testing.T) {
	assets, err := RenderState(StateRequest{PartitionUUID: statePartUUID})
	if err != nil {
		t.Fatalf("RenderState() error = %v", err)
	}
	want := `[Unit]
Description=Activate selected Katl generation extensions
Documentation=man:systemd-sysext(8) man:systemd-confext(8)
DefaultDependencies=no
Requires=var.mount
After=var.mount
Before=systemd-sysext.service systemd-confext.service

[Service]
Type=oneshot
StandardOutput=journal+console
SyslogIdentifier=katl-generation-activate
ExecStart=/usr/lib/katl/runtime/katl-generation-activate --root=/

[Install]
RequiredBy=systemd-sysext.service
RequiredBy=systemd-confext.service
`
	if assets.GenerationActivate != want {
		t.Fatalf("katl-generation-activate.service:\n%s\nwant:\n%s", assets.GenerationActivate, want)
	}
}

func TestKubeadmReadyRuntimeUnits(t *testing.T) {
	assets, err := RenderState(StateRequest{PartitionUUID: statePartUUID})
	if err != nil {
		t.Fatalf("RenderState() error = %v", err)
	}
	wantTarget := `[Unit]
Description=Katl kubeadm-ready handoff point
Documentation=man:systemd.target(5)
Requires=systemd-sysext.service systemd-confext.service containerd.service etc-kubernetes.mount katl-state-projection-check.service katl-runtime-handoff-status.service
After=systemd-sysext.service systemd-confext.service containerd.service etc-kubernetes.mount katl-state-projection-check.service katl-runtime-handoff-status.service

[Install]
WantedBy=multi-user.target
`
	if assets.KubeadmReadyTarget != wantTarget {
		t.Fatalf("katl-kubeadm-ready.target:\n%s\nwant:\n%s", assets.KubeadmReadyTarget, wantTarget)
	}
	wantContainerd := `[Unit]
Requires=var.mount
After=var.mount
Before=katl-kubeadm-ready.target
RequiresMountsFor=/var/lib/containerd
`
	if assets.ContainerdDropIn != wantContainerd {
		t.Fatalf("containerd drop-in:\n%s\nwant:\n%s", assets.ContainerdDropIn, wantContainerd)
	}
	wantKubelet := `[Unit]
Requires=containerd.service etc-kubernetes.mount
After=var.mount containerd.service etc-kubernetes.mount
Before=katl-kubeadm-ready.target
RequiresMountsFor=/var/lib/kubelet /etc/kubernetes
`
	if assets.KubeletDropIn != wantKubelet {
		t.Fatalf("kubelet drop-in:\n%s\nwant:\n%s", assets.KubeletDropIn, wantKubelet)
	}
}

func TestWriteState(t *testing.T) {
	root := t.TempDir()
	assets, err := WriteState(root, StateRequest{PartitionUUID: statePartUUID})
	if err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}
	assertFile(t, filepath.Join(root, "etc/systemd/system/var.mount"), assets.VarMount)
	assertFile(t, filepath.Join(root, "etc/systemd/system/etc-kubernetes.mount"), assets.EtcKubernetesMount)
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-generation-activate.service"), assets.GenerationActivate)
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-kubeadm-ready.target"), assets.KubeadmReadyTarget)
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/multi-user.target.wants/katl-kubeadm-ready.target"), "../katl-kubeadm-ready.target")
	assertFile(t, filepath.Join(root, "etc/systemd/system/containerd.service.d/10-katl-runtime.conf"), assets.ContainerdDropIn)
	assertFile(t, filepath.Join(root, "etc/systemd/system/kubelet.service.d/10-katl-runtime.conf"), assets.KubeletDropIn)
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/systemd-sysext.service.requires/katl-generation-activate.service"), "../katl-generation-activate.service")
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/systemd-confext.service.requires/katl-generation-activate.service"), "../katl-generation-activate.service")
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-state-projection-check.service"), assets.StateCheckService)
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-runtime-handoff-status.service"), assets.RuntimeStatus)
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/katl-kubeadm-ready.target.requires/katl-runtime-handoff-status.service"), "../katl-runtime-handoff-status.service")
	assertMissing(t, filepath.Join(root, "etc/systemd/system/multi-user.target.wants/kubelet.service"))
	assertMissing(t, filepath.Join(root, "etc/systemd/system/katl-kubeadm-init.service"))
	assertMissing(t, filepath.Join(root, "etc/systemd/system/katl-kubeadm-join.service"))
	assertFile(t, filepath.Join(root, "etc/tmpfiles.d/katl-state.conf"), assets.Tmpfiles)
	assertDir(t, filepath.Join(root, "var/lib/katl"), 0o755)
	assertDir(t, filepath.Join(root, "var/lib/katl/generations"), 0o755)
	assertDir(t, filepath.Join(root, "var/lib/katl/install/logs"), 0o755)
	assertDir(t, filepath.Join(root, "var/lib/katl/kubernetes/etc-kubernetes"), 0o755)
	assertDir(t, filepath.Join(root, "var/lib/katl/ssh/host-keys"), 0o700)
	assertDir(t, filepath.Join(root, "var/lib/containerd"), 0o755)
	assertDir(t, filepath.Join(root, "var/lib/kubelet"), 0o755)
	assertDir(t, filepath.Join(root, "var/log/journal"), 0o755)
	assertDir(t, filepath.Join(root, "etc/kubernetes"), 0o755)
}

func TestRuntimeStaticStateUnits(t *testing.T) {
	assets, err := RenderState(StateRequest{PartitionUUID: statePartUUID})
	if err != nil {
		t.Fatalf("RenderState() error = %v", err)
	}
	root := repoRoot(t)
	systemdRoot := filepath.Join(root, "mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system")

	assertRepoFile(t, filepath.Join(systemdRoot, "var.mount"), strings.ReplaceAll(assets.VarMount, "PARTUUID="+statePartUUID, "/dev/disk/by-partlabel/KATL_STATE"))
	assertRepoFile(t, filepath.Join(systemdRoot, "etc-kubernetes.mount"), assets.EtcKubernetesMount)
	assertRepoFile(t, filepath.Join(systemdRoot, "katl-generation-activate.service"), assets.GenerationActivate)
	assertRepoFile(t, filepath.Join(systemdRoot, "katl-kubeadm-ready.target"), assets.KubeadmReadyTarget)
	assertRepoFile(t, filepath.Join(systemdRoot, "containerd.service.d/10-katl-runtime.conf"), assets.ContainerdDropIn)
	assertRepoFile(t, filepath.Join(systemdRoot, "kubelet.service.d/10-katl-runtime.conf"), assets.KubeletDropIn)
	assertRepoFile(t, filepath.Join(systemdRoot, "katl-state-projection-check.service"), assets.StateCheckService)
	assertRepoFile(t, filepath.Join(systemdRoot, "katl-runtime-handoff-status.service"), assets.RuntimeStatus)
	assertRepoFile(t, filepath.Join(root, "mkosi.profiles/runtime/mkosi.extra/usr/lib/tmpfiles.d/katl-state.conf"), assets.Tmpfiles)

	assertSymlink(t, filepath.Join(systemdRoot, "local-fs.target.wants/var.mount"), "../var.mount")
	assertMissing(t, filepath.Join(systemdRoot, "local-fs.target.wants/etc-kubernetes.mount"))
	assertSymlink(t, filepath.Join(systemdRoot, "multi-user.target.wants/katl-kubeadm-ready.target"), "../katl-kubeadm-ready.target")
	assertSymlink(t, filepath.Join(systemdRoot, "multi-user.target.wants/katl-state-projection-check.service"), "../katl-state-projection-check.service")
	assertSymlink(t, filepath.Join(systemdRoot, "systemd-sysext.service.requires/katl-generation-activate.service"), "../katl-generation-activate.service")
	assertSymlink(t, filepath.Join(systemdRoot, "systemd-confext.service.requires/katl-generation-activate.service"), "../katl-generation-activate.service")
	assertSymlink(t, filepath.Join(systemdRoot, "katl-kubeadm-ready.target.requires/katl-runtime-handoff-status.service"), "../katl-runtime-handoff-status.service")
}

func TestRenderStateRejectsUUID(t *testing.T) {
	_, err := RenderState(StateRequest{PartitionUUID: "abc rw"})
	if err == nil || !strings.Contains(err.Error(), "must not contain whitespace") {
		t.Fatalf("RenderState() error = %v, want UUID validation failure", err)
	}
}

func TestKubernetesProjectionRejectsPath(t *testing.T) {
	tests := []struct {
		name    string
		request KubernetesProjectionRequest
		want    string
	}{
		{name: "run source", request: KubernetesProjectionRequest{Source: "/run/katl/kubernetes", Target: KubernetesTarget}, want: "source"},
		{name: "broad etc target", request: KubernetesProjectionRequest{Source: KubernetesSource, Target: "/etc"}, want: "target"},
		{name: "sibling etc target", request: KubernetesProjectionRequest{Source: KubernetesSource, Target: "/etc/ssh"}, want: "target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RenderKubernetesProjection(tt.request)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("RenderKubernetesProjection() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestStateUnitsVerify(t *testing.T) {
	if os.Getenv("KATL_VERIFY_SYSTEMD_UNITS") != "1" {
		t.Skip("set KATL_VERIFY_SYSTEMD_UNITS=1 to run systemd-analyze verify")
	}
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze not available")
	}
	root := t.TempDir()
	writeStateVerifyFixture(t, root)

	argv := append([]string{"systemd-analyze", "verify", "--root=" + root}, stateVerifyUnits()...)
	cmd := exec.Command(argv[0], argv[1:]...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze verify failed: %v\n%s", err, output)
	}
}

func TestStateUnitsVerifyNspawn(t *testing.T) {
	root := t.TempDir()
	writeStateVerifyFixture(t, root)

	const guestRoot = "/mnt/katl-generated-units"
	options := nspawntest.DefaultOptions()
	options.Missing = nspawntest.MissingSkips
	if err := nspawntest.PrepareDefaultRoot(t.Context(), &options, repoRoot(t)); err != nil {
		t.Fatalf("prepare nspawn userspace fixture: %v", err)
	}
	nspawntest.NewRunner(options).Run(t, nspawntest.Scenario{
		Name: "state unit verify",
		Binds: []nspawntest.Bind{{
			Source: root,
			Target: guestRoot,
		}},
		Commands: []nspawntest.Command{{
			Name: "systemd-analyze verify",
			Argv: append([]string{
				"systemd-analyze",
				"verify",
				"--root=" + guestRoot,
			}, stateVerifyUnits()...),
		}},
	})
}

func writeStateVerifyFixture(t *testing.T, root string) {
	t.Helper()
	if _, err := WriteState(root, StateRequest{PartitionUUID: statePartUUID}); err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}
	writeUnit(t, root, "usr/lib/systemd/system/local-fs.target", "[Unit]\nDescription=Local File Systems\n")
	writeUnit(t, root, "usr/lib/systemd/system/multi-user.target", "[Unit]\nDescription=Multi-User System\n")
	writeUnit(t, root, "usr/lib/systemd/system/umount.target", "[Unit]\nDescription=Unmount All Filesystems\n")
	writeUnit(t, root, "usr/lib/systemd/system/sysinit.target", "[Unit]\nDescription=System Initialization\n")
	writeUnit(t, root, "usr/lib/systemd/system/systemd-sysext.service", "[Unit]\nDescription=System Extension Images\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	writeUnit(t, root, "usr/lib/systemd/system/systemd-confext.service", "[Unit]\nDescription=System Configuration Extension Images\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	writeUnit(t, root, "usr/lib/systemd/system/containerd.service", "[Unit]\nDescription=Containerd\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	writeUnit(t, root, "usr/lib/systemd/system/kubelet.service", "[Unit]\nDescription=Kubelet\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	writeUnit(t, root, "usr/lib/katl/runtime/katl-generation-activate", "#!/bin/sh\nexit 0\n")
	writeUnit(t, root, "usr/lib/katl/runtime/katl-runtime-status", "#!/bin/sh\nexit 0\n")
	writeUnit(t, root, "usr/bin/printf", "#!/bin/sh\nexit 0\n")
	writeUnit(t, root, "usr/bin/true", "#!/bin/sh\nexit 0\n")
	for _, fixture := range []string{"usr/bin/printf", "usr/bin/true", "usr/lib/katl/runtime/katl-generation-activate", "usr/lib/katl/runtime/katl-runtime-status"} {
		if err := os.Chmod(filepath.Join(root, filepath.FromSlash(fixture)), 0o755); err != nil {
			t.Fatalf("chmod %s fixture: %v", fixture, err)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(output))
}

func stateVerifyUnits() []string {
	return []string{
		"/etc/systemd/system/var.mount",
		"/etc/systemd/system/etc-kubernetes.mount",
		"/etc/systemd/system/katl-generation-activate.service",
		"/etc/systemd/system/katl-kubeadm-ready.target",
		"/etc/systemd/system/katl-state-projection-check.service",
		"/etc/systemd/system/katl-runtime-handoff-status.service",
		"/usr/lib/systemd/system/containerd.service",
		"/usr/lib/systemd/system/kubelet.service",
	}
}

func assertFile(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s:\n%s\nwant:\n%s", path, data, want)
	}
}

func assertRepoFile(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := string(data); got != want {
		t.Fatalf("%s:\n%s\nwant:\n%s", path, got, want)
	}
}

func assertDir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Fatalf("%s mode = %04o, want %04o", path, got, mode)
	}
}

func assertSymlink(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	if got != want {
		t.Fatalf("%s -> %s, want %s", path, got, want)
	}
}

func writeUnit(t *testing.T, root string, rel string, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
