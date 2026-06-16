package generation

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		"d /var/lib/katl/boot 0755 root root -",
		"d /var/lib/katl/generations 0755 root root -",
		"d /var/lib/katl/install/logs 0755 root root -",
		"d /var/lib/katl/operations 0750 root root -",
		"d /var/lib/katl/agent 0700 root root -",
		"d /var/lib/katl/cluster 0750 root root -",
		"d /var/lib/katl/config-requests 0750 root root -",
		"d /var/lib/katl/kubernetes/etc-kubernetes 0755 root root -",
		"d /var/lib/containerd 0755 root root -",
		"d /var/lib/etcd 0755 root root -",
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
Requires=katlc-agent.service
After=katlc-agent.service
Before=katl-boot-complete.target
RequiresMountsFor=/var/lib/katl

[Service]
Type=oneshot
StandardOutput=journal+console
SyslogIdentifier=katl-runtime-status
ExecStart=/usr/lib/katl/runtime/katl-runtime-status --root=/

[Install]
RequiredBy=katl-boot-complete.target
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
Requires=systemd-sysext.service systemd-confext.service containerd.service etc-kubernetes.mount katl-state-projection-check.service katlc-agent.service
After=systemd-sysext.service systemd-confext.service containerd.service etc-kubernetes.mount katl-state-projection-check.service katlc-agent.service

[Install]
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

func TestBootHealthRuntimeUnits(t *testing.T) {
	assets, err := RenderState(StateRequest{PartitionUUID: statePartUUID})
	if err != nil {
		t.Fatalf("RenderState() error = %v", err)
	}
	wantComplete := `[Unit]
Description=Katl boot-complete promotion point
Documentation=man:systemd.target(5)
Requires=katl-runtime-handoff-status.service katl-boot-health.service
After=katl-runtime-handoff-status.service katl-boot-health.service

[Install]
WantedBy=multi-user.target
`
	if assets.BootCompleteTarget != wantComplete {
		t.Fatalf("katl-boot-complete.target:\n%s\nwant:\n%s", assets.BootCompleteTarget, wantComplete)
	}
	wantHealth := `[Unit]
Description=Record successful Katl boot health
Documentation=man:systemd.service(5)
Requires=katl-runtime-handoff-status.service katlc-agent.service systemd-networkd.service
Wants=sshd.service
After=katl-runtime-handoff-status.service katlc-agent.service systemd-networkd.service sshd.service
Before=katl-boot-complete.target
RequiresMountsFor=/var/lib/katl

[Service]
Type=oneshot
StandardOutput=journal+console
SyslogIdentifier=katl-boot-health
ExecStart=/usr/lib/katl/runtime/katl-boot-health --root=/ --result=success --reason=katl-boot-complete.target

[Install]
RequiredBy=katl-boot-complete.target
`
	if assets.BootHealthService != wantHealth {
		t.Fatalf("katl-boot-health.service:\n%s\nwant:\n%s", assets.BootHealthService, wantHealth)
	}
	wantDeadman := `[Unit]
Description=Fail Katl boot health after deadline
Documentation=man:systemd.service(5)
Requires=var.mount
After=var.mount
RequiresMountsFor=/var/lib/katl

[Service]
Type=oneshot
StandardOutput=journal+console
SyslogIdentifier=katl-boot-deadman
ExecStart=/usr/lib/katl/runtime/katl-boot-health --root=/ --result=timeout --reason=katl-boot-health-deadline-expired --request-reboot
`
	if assets.BootDeadmanService != wantDeadman {
		t.Fatalf("katl-boot-deadman.service:\n%s\nwant:\n%s", assets.BootDeadmanService, wantDeadman)
	}
	wantTimer := `[Unit]
Description=Katl boot health deadline
Documentation=man:systemd.timer(5)

[Timer]
OnBootSec=10min
Unit=katl-boot-deadman.service

[Install]
WantedBy=timers.target
`
	if assets.BootDeadmanTimer != wantTimer {
		t.Fatalf("katl-boot-deadman.timer:\n%s\nwant:\n%s", assets.BootDeadmanTimer, wantTimer)
	}
}

func TestAgentRuntimeUnit(t *testing.T) {
	assets, err := RenderState(StateRequest{PartitionUUID: statePartUUID})
	if err != nil {
		t.Fatalf("RenderState() error = %v", err)
	}
	want := `[Unit]
Description=Run Katl node management agent
Documentation=man:systemd.service(5)
Requires=var.mount katl-generation-activate.service
Wants=network-online.target
After=local-fs.target var.mount katl-generation-activate.service network-online.target
Before=katl-kubeadm-ready.target
RequiresMountsFor=/var/lib/katl

[Service]
Type=simple
ExecStartPre=/usr/bin/katlc agent init-token --path /var/lib/katl/agent/token
ExecStart=/usr/bin/katlc agent serve --root=/ --listen tcp://0.0.0.0:9443 --auth-token-file /var/lib/katl/agent/token
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=katlc-agent
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
`
	if assets.AgentService != want {
		t.Fatalf("katlc-agent.service:\n%s\nwant:\n%s", assets.AgentService, want)
	}
}

func TestWriteState(t *testing.T) {
	root := t.TempDir()
	legacySystemdRoot := filepath.Join(root, "etc/systemd/system")
	if err := os.MkdirAll(filepath.Join(legacySystemdRoot, "katl-kubeadm-ready.target.requires"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacySystemdRoot, "multi-user.target.wants"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacySystemdRoot, "katl-operation@.service"), []byte("old operation unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacySystemdRoot, "katl-operation-reconcile.service"), []byte("old reconcile unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../katl-operation-reconcile.service", filepath.Join(legacySystemdRoot, "katl-kubeadm-ready.target.requires/katl-operation-reconcile.service")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../katl-state-projection-check.service", filepath.Join(legacySystemdRoot, "multi-user.target.wants/katl-state-projection-check.service")); err != nil {
		t.Fatal(err)
	}

	assets, err := WriteState(root, StateRequest{PartitionUUID: statePartUUID})
	if err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}
	assertFile(t, filepath.Join(root, "etc/systemd/system/var.mount"), assets.VarMount)
	assertFile(t, filepath.Join(root, "etc/systemd/system/etc-kubernetes.mount"), assets.EtcKubernetesMount)
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-generation-activate.service"), assets.GenerationActivate)
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-kubeadm-ready.target"), assets.KubeadmReadyTarget)
	assertMissing(t, filepath.Join(root, "etc/systemd/system/multi-user.target.wants/katl-kubeadm-ready.target"))
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-boot-complete.target"), assets.BootCompleteTarget)
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-boot-health.service"), assets.BootHealthService)
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-boot-deadman.service"), assets.BootDeadmanService)
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-boot-deadman.timer"), assets.BootDeadmanTimer)
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/multi-user.target.wants/katl-boot-complete.target"), "../katl-boot-complete.target")
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/katl-boot-complete.target.requires/katl-boot-health.service"), "../katl-boot-health.service")
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/timers.target.wants/katl-boot-deadman.timer"), "../katl-boot-deadman.timer")
	assertFile(t, filepath.Join(root, "etc/systemd/system/containerd.service.d/10-katl-runtime.conf"), assets.ContainerdDropIn)
	assertFile(t, filepath.Join(root, "etc/systemd/system/kubelet.service.d/10-katl-runtime.conf"), assets.KubeletDropIn)
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/systemd-sysext.service.requires/katl-generation-activate.service"), "../katl-generation-activate.service")
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/systemd-confext.service.requires/katl-generation-activate.service"), "../katl-generation-activate.service")
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-state-projection-check.service"), assets.StateCheckService)
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-runtime-handoff-status.service"), assets.RuntimeStatus)
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/katl-boot-complete.target.requires/katl-runtime-handoff-status.service"), "../katl-runtime-handoff-status.service")
	assertMissing(t, filepath.Join(root, "etc/systemd/system/multi-user.target.wants/katl-state-projection-check.service"))
	assertFile(t, filepath.Join(root, "etc/systemd/system/katlc-agent.service"), assets.AgentService)
	assertSymlink(t, filepath.Join(root, "etc/systemd/system/multi-user.target.wants/katlc-agent.service"), "../katlc-agent.service")
	assertMissing(t, filepath.Join(root, "etc/systemd/system/katl-operation@.service"))
	assertMissing(t, filepath.Join(root, "etc/systemd/system/katl-operation-reconcile.service"))
	assertMissing(t, filepath.Join(root, "etc/systemd/system/katl-kubeadm-ready.target.requires/katl-operation-reconcile.service"))
	assertMissing(t, filepath.Join(root, "etc/systemd/system/multi-user.target.wants/kubelet.service"))
	assertMissing(t, filepath.Join(root, "etc/systemd/system/katl-kubeadm-init.service"))
	assertMissing(t, filepath.Join(root, "etc/systemd/system/katl-kubeadm-join.service"))
	assertFile(t, filepath.Join(root, "etc/tmpfiles.d/katl-state.conf"), assets.Tmpfiles)
	assertDir(t, filepath.Join(root, "var/lib/katl"), 0o755)
	assertDir(t, filepath.Join(root, "var/lib/katl/boot"), 0o755)
	assertDir(t, filepath.Join(root, "var/lib/katl/generations"), 0o755)
	assertDir(t, filepath.Join(root, "var/lib/katl/install/logs"), 0o755)
	assertDir(t, filepath.Join(root, "var/lib/katl/operations"), 0o750)
	assertDir(t, filepath.Join(root, "var/lib/katl/agent"), 0o700)
	assertDir(t, filepath.Join(root, "var/lib/katl/cluster"), 0o750)
	assertDir(t, filepath.Join(root, "var/lib/katl/config-requests"), 0o750)
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
	assertRepoFile(t, filepath.Join(systemdRoot, "katl-boot-complete.target"), assets.BootCompleteTarget)
	assertRepoFile(t, filepath.Join(systemdRoot, "katl-boot-health.service"), assets.BootHealthService)
	assertRepoFile(t, filepath.Join(systemdRoot, "katl-boot-deadman.service"), assets.BootDeadmanService)
	assertRepoFile(t, filepath.Join(systemdRoot, "katl-boot-deadman.timer"), assets.BootDeadmanTimer)
	assertRepoFile(t, filepath.Join(systemdRoot, "containerd.service.d/10-katl-runtime.conf"), assets.ContainerdDropIn)
	assertRepoFile(t, filepath.Join(systemdRoot, "kubelet.service.d/10-katl-runtime.conf"), assets.KubeletDropIn)
	assertRepoFile(t, filepath.Join(systemdRoot, "katl-state-projection-check.service"), assets.StateCheckService)
	assertRepoFile(t, filepath.Join(systemdRoot, "katl-runtime-handoff-status.service"), assets.RuntimeStatus)
	assertRepoFile(t, filepath.Join(systemdRoot, "katlc-agent.service"), assets.AgentService)
	assertRepoFile(t, filepath.Join(root, "mkosi.profiles/runtime/mkosi.extra/usr/lib/tmpfiles.d/katl-state.conf"), assets.Tmpfiles)

	assertSymlink(t, filepath.Join(systemdRoot, "local-fs.target.wants/var.mount"), "../var.mount")
	assertMissing(t, filepath.Join(systemdRoot, "local-fs.target.wants/etc-kubernetes.mount"))
	assertMissing(t, filepath.Join(systemdRoot, "multi-user.target.wants/katl-kubeadm-ready.target"))
	assertSymlink(t, filepath.Join(systemdRoot, "multi-user.target.wants/katl-boot-complete.target"), "../katl-boot-complete.target")
	assertSymlink(t, filepath.Join(systemdRoot, "multi-user.target.wants/katlc-agent.service"), "../katlc-agent.service")
	assertMissing(t, filepath.Join(systemdRoot, "multi-user.target.wants/katl-state-projection-check.service"))
	assertSymlink(t, filepath.Join(systemdRoot, "timers.target.wants/katl-boot-deadman.timer"), "../katl-boot-deadman.timer")
	assertSymlink(t, filepath.Join(systemdRoot, "systemd-sysext.service.requires/katl-generation-activate.service"), "../katl-generation-activate.service")
	assertSymlink(t, filepath.Join(systemdRoot, "systemd-confext.service.requires/katl-generation-activate.service"), "../katl-generation-activate.service")
	assertMissing(t, filepath.Join(systemdRoot, "katl-kubeadm-ready.target.requires/katl-runtime-handoff-status.service"))
	assertSymlink(t, filepath.Join(systemdRoot, "katl-boot-complete.target.requires/katl-boot-health.service"), "../katl-boot-health.service")
	assertSymlink(t, filepath.Join(systemdRoot, "katl-boot-complete.target.requires/katl-runtime-handoff-status.service"), "../katl-runtime-handoff-status.service")
	assertMissing(t, filepath.Join(systemdRoot, "katl-operation@.service"))
	assertMissing(t, filepath.Join(systemdRoot, "katl-operation-reconcile.service"))
	assertMissing(t, filepath.Join(systemdRoot, "katl-kubeadm-ready.target.requires/katl-operation-reconcile.service"))
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

func writeStateVerifyFixture(t *testing.T, root string) {
	t.Helper()
	if _, err := WriteState(root, StateRequest{PartitionUUID: statePartUUID}); err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}
	writeUnit(t, root, "usr/lib/systemd/system/local-fs.target", "[Unit]\nDescription=Local File Systems\n")
	writeUnit(t, root, "usr/lib/systemd/system/multi-user.target", "[Unit]\nDescription=Multi-User System\n")
	writeUnit(t, root, "usr/lib/systemd/system/timers.target", "[Unit]\nDescription=Timers\n")
	writeUnit(t, root, "usr/lib/systemd/system/umount.target", "[Unit]\nDescription=Unmount All Filesystems\n")
	writeUnit(t, root, "usr/lib/systemd/system/sysinit.target", "[Unit]\nDescription=System Initialization\n")
	writeUnit(t, root, "usr/lib/systemd/system/systemd-sysext.service", "[Unit]\nDescription=System Extension Images\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	writeUnit(t, root, "usr/lib/systemd/system/systemd-confext.service", "[Unit]\nDescription=System Configuration Extension Images\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	writeUnit(t, root, "usr/lib/systemd/system/containerd.service", "[Unit]\nDescription=Containerd\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	writeUnit(t, root, "usr/lib/systemd/system/kubelet.service", "[Unit]\nDescription=Kubelet\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	writeUnit(t, root, "usr/lib/systemd/system/network-online.target", "[Unit]\nDescription=Network Online\n")
	writeUnit(t, root, "usr/lib/systemd/system/systemd-networkd.service", "[Unit]\nDescription=Network Configuration\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	writeUnit(t, root, "usr/lib/systemd/system/sshd.service", "[Unit]\nDescription=OpenSSH server daemon\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	writeUnit(t, root, "usr/lib/katl/runtime/katl-generation-activate", "#!/bin/sh\nexit 0\n")
	writeUnit(t, root, "usr/lib/katl/runtime/katl-boot-health", "#!/bin/sh\nexit 0\n")
	writeUnit(t, root, "usr/lib/katl/runtime/katl-runtime-status", "#!/bin/sh\nexit 0\n")
	writeUnit(t, root, "usr/bin/katlc", "#!/bin/sh\nexit 0\n")
	writeUnit(t, root, "usr/bin/printf", "#!/bin/sh\nexit 0\n")
	writeUnit(t, root, "usr/bin/true", "#!/bin/sh\nexit 0\n")
	for _, fixture := range []string{"usr/bin/katlc", "usr/bin/printf", "usr/bin/true", "usr/lib/katl/runtime/katl-generation-activate", "usr/lib/katl/runtime/katl-boot-health", "usr/lib/katl/runtime/katl-runtime-status"} {
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
		"/etc/systemd/system/katl-boot-complete.target",
		"/etc/systemd/system/katl-boot-health.service",
		"/etc/systemd/system/katl-boot-deadman.service",
		"/etc/systemd/system/katl-boot-deadman.timer",
		"/etc/systemd/system/katl-state-projection-check.service",
		"/etc/systemd/system/katl-runtime-handoff-status.service",
		"/etc/systemd/system/katlc-agent.service",
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
