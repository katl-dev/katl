package vmtest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	vmtestpb "github.com/zariel/katl/internal/vmtest/proto"
)

func TestKubeadmReadySmokeChecksRuntimeHandoff(t *testing.T) {
	result := guestResult(t)
	client := newScriptedGuestClient()
	client.commandResults = map[string][]*vmtestpb.CommandResult{
		commandKey("systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"):               {okCommand()},
		commandKey("test", "-f", DefaultKubeadmConfigPath):                                         {okCommand()},
		commandKey("findmnt", "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE"): {stdoutCommand("/dev/vdb4[/lib/katl/kubernetes/etc-kubernetes]\n")},
		commandKey("test", "-x", "/usr/bin/kubeadm"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/kubelet"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/kubectl"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/crictl"):                                                {okCommand()},
		commandKey("systemctl", "is-active", "--quiet", "containerd.service"):                      {okCommand()},
		commandKey("crictl", "info"):                                                               {stdoutCommand("{}\n")},
	}
	guest := NewGuestControl(result, client)

	if err := RunKubeadmReadySmoke(context.Background(), guest, KubeadmReadySmokePlan{}); err != nil {
		t.Fatalf("RunKubeadmReadySmoke() error = %v", err)
	}

	if client.commandCount(commandKey("kubeadm", "init", "--config", DefaultKubeadmConfigPath, "--skip-phases=addon/coredns,addon/kube-proxy")) != 0 {
		t.Fatalf("readiness smoke must not run kubeadm init: %#v", client.commandRequests)
	}
	if !client.sensitiveCommand(commandKey("crictl", "info")) {
		t.Fatalf("crictl info was not marked sensitive: %#v", client.commandRequests)
	}
}

func TestInstalledKubeadmReadySmokeUsesPackagedRuntime(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	result, err := NewRunner(Options{StateRoot: root, RunID: "run-1"}).Plan(Scenario{Name: "kubeadm-ready-smoke"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, vmConfig := vmFixture(t)
	vmConfig.Expect = runtimeBootSignal
	vmConfig.VSock.Enabled = true
	client := newScriptedGuestClient()
	client.commandResults = map[string][]*vmtestpb.CommandResult{
		commandKey("systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"):               {okCommand()},
		commandKey("test", "-f", DefaultKubeadmConfigPath):                                         {okCommand()},
		commandKey("findmnt", "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE"): {stdoutCommand(DefaultProjectedKubernetesPath + "\n")},
		commandKey("test", "-x", "/usr/bin/kubeadm"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/kubelet"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/kubectl"):                                               {okCommand()},
		commandKey("test", "-x", "/usr/bin/crictl"):                                                {okCommand()},
		commandKey("systemctl", "is-active", "--quiet", "containerd.service"):                      {okCommand()},
		commandKey("crictl", "info"):                                                               {stdoutCommand("{}\n")},
	}
	runner := VMRunner{
		Executor: blockingVMExec{write: runtimeBootSignal},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
			output:   func(string, ...string) ([]byte, error) { return []byte("vhost-vsock-pci guest-cid\n"), nil },
		},
	}

	result = RunInstalledKubeadmReadySmoke(context.Background(), result, KubeadmReadySmokeConfig{
		Runtime: InstalledRuntimeConfig{
			Disk:         disk,
			DiskFormat:   DiskRaw,
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Smoke: KubeadmReadySmokePlan{
			ReadyPollInterval: time.Millisecond,
		},
		AgentConnector: func(context.Context, VSockPlan, string) (KubeadmReadySmokeAgentSession, error) {
			return client, nil
		},
	}, runner)

	if result.Status != StatusPassed {
		t.Fatalf("result = %#v", result)
	}
	domainXML := readDomainXML(t, result)
	if !strings.Contains(domainXML, `<source file="`+filepath.Join(result.QEMUDir, "vdb.snapshot.qcow2")+`"></source>`) {
		t.Fatalf("domain XML did not boot packaged disk: %s", domainXML)
	}
}
