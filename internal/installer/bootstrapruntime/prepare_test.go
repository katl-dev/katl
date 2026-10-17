package bootstrapruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/bootstrapplan"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
)

type trackingReader struct {
	reader  io.Reader
	maxRead int
}

type writerOnly struct {
	io.Writer
}

func (r *trackingReader) Read(buffer []byte) (int, error) {
	if len(buffer) > r.maxRead {
		r.maxRead = len(buffer)
	}
	return r.reader.Read(buffer)
}

func TestCopySysextUsesBoundedReads(t *testing.T) {
	payload := bytes.Repeat([]byte("k"), 4<<20)
	reader := &trackingReader{reader: bytes.NewReader(payload)}
	var target bytes.Buffer
	written, err := copySysext(writerOnly{Writer: &target}, reader)
	if err != nil {
		t.Fatalf("copySysext() error = %v", err)
	}
	if written != int64(len(payload)) || !bytes.Equal(target.Bytes(), payload) {
		t.Fatalf("copySysext() copied %d bytes, want %d", written, len(payload))
	}
	if reader.maxRead > 64<<10 {
		t.Fatalf("copySysext() maximum read = %d, want at most %d", reader.maxRead, 64<<10)
	}
}

func TestPrepareMaterializesCandidateRuntimeWithoutBootDefault(t *testing.T) {
	root := t.TempDir()
	previous, previousStatus := writeGenerationZero(t, root)
	enablement := filepath.Join(root, "var/lib/katl/generations/0/confext/etc/systemd/system/multi-user.target.wants/bird.service")
	if err := os.MkdirAll(filepath.Dir(enablement), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/lib/systemd/system/bird.service", enablement); err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:          generation.APIVersion,
		Kind:                generation.BootSelectionKind,
		DefaultGenerationID: "0",
		BootedGenerationID:  "0",
		DefaultBootEntry:    "loader/entries/katl-0.conf",
		UpdatedAt:           time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	sysextPayload := []byte("kubernetes-sysext-payload")
	writeFile(t, filepath.Join(root, "var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw"), string(sysextPayload))
	writeFile(t, filepath.Join(root, "proc/cmdline"), "root=PARTUUID=old rootfstype=squashfs ro systemd.machine_id=old katl.generation=0 katl.root-slot=root-a console=ttyS0,115200n8 katl.vmtest_agent=1\n")
	inputDir, err := installer.StoredKubeadmInputDir(root, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inputDir, "config.yaml"), strings.Join([]string{
		"apiVersion: kubeadm.k8s.io/v1beta4",
		"kind: InitConfiguration",
		"---",
		"apiVersion: kubeadm.k8s.io/v1beta4",
		"kind: ClusterConfiguration",
		"",
	}, "\n"))
	kubeadmInput := bootstrapplan.KubeadmInput{
		ConfigRef: "control-plane",
		Intent:    "control-plane",
		Path:      "/etc/katl/kubeadm/control-plane/config.yaml",
	}
	kubeadmInput.Digest = storedInputDigest(t, root, kubeadmInput)

	result, err := prepareSnapshot(t, root, bootstrapplan.Plan{
		Operation: operation.OperationRecord{
			OperationID:           "bootstrap-init-01",
			OperationKind:         bootstrapplan.OperationKindInit,
			RequestDigest:         strings.Repeat("1", 64),
			CandidateGenerationID: "1",
			BootstrapRequest: &operation.BootstrapRequest{
				InventoryNodeName:        "cp-1",
				SystemRole:               "control-plane",
				KubernetesPayloadVersion: "v1.36.1",
				BootstrapProfileRef:      "control-plane",
				CandidateGenerationID:    "1",
				APIProxyConfig: `{
					"canonicalEndpoint":"api.katl.test:6443",
					"tlsServerName":"api.katl.test",
					"listeners":[{"exposure":"node-local","address":"127.0.0.1:7445"}],
					"backends":[{"name":"cp-1","address":"192.0.2.10:6443","local":true}]
				}`,
			},
		},
		Previous:      previous,
		PreviousState: previousStatus,
		RuntimeInputs: bootstrapplan.RuntimeInputs{
			SelectedKubernetesSysext: bootstrapplan.SelectedKubernetesSysext{
				Path:             "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw",
				SHA256:           digest(sysextPayload),
				SizeBytes:        uint64(len(sysextPayload)),
				PayloadVersion:   "v1.36.1",
				ActivationPath:   "/run/extensions/katl-kubernetes.raw",
				Architecture:     "x86_64",
				RuntimeInterface: "katl-runtime-1",
			},
			HostConfig: bootstrapplan.HostConfig{
				NodeName:             "cp-1",
				SystemRole:           "control-plane",
				ControlPlaneEndpoint: "api.katl.test:6443",
				NodeMetadataPath:     "/etc/katl/node.json",
			},
			KubeadmInput: kubeadmInput,
			KubernetesProjection: bootstrapplan.KubernetesProjection{
				What:  generation.KubernetesSource,
				Where: generation.KubernetesTarget,
			},
		},
	}, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.Record.GenerationID != "1" || len(result.Spec.Sysexts) != 1 || result.Status.CommitState != generation.CommitStateCandidate {
		t.Fatalf("result = %#v status %#v", result.Record, result.Status)
	}
	if result.Spec.Boot.LoaderEntryPath != "loader/entries/katl-1.conf" {
		t.Fatalf("candidate loader entry path = %q", result.Spec.Boot.LoaderEntryPath)
	}
	commandLine := strings.Join(result.Spec.KernelCommandLine, " ")
	if !strings.Contains(commandLine, "console=ttyS0,115200n8") || !strings.Contains(commandLine, "katl.vmtest_agent=1") || strings.Contains(commandLine, "root=PARTUUID=old") || strings.Contains(commandLine, "katl.generation=0") {
		t.Fatalf("candidate kernel command line = %q", commandLine)
	}
	assertContains(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/katl/kubeadm/control-plane/config.yaml"), "InitConfiguration")
	assertContains(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/katl/kubeadm/control-plane/config.yaml"), "controlPlaneEndpoint: api.katl.test:6443")
	desiredKubeadm, sourceGeneration, err := configapply.ReadEffectiveGenerationKubeadmConfig(root, "1", "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if sourceGeneration != "1" || strings.Contains(string(desiredKubeadm.Config.Content), "controlPlaneEndpoint:") {
		t.Fatalf("desired kubeadm input = source %q content:\n%s", sourceGeneration, desiredKubeadm.Config.Content)
	}
	assertContains(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/katl/bootstrap-runtime.json"), `"controlPlaneEndpoint": "api.katl.test:6443"`)
	assertContains(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/katl/api-proxy/config.json"), `"canonicalEndpoint": "api.katl.test:6443"`)
	assertContains(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/katl/api-proxy/config.json"), `"address": "192.0.2.10:6443"`)
	assertContains(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/extension-release.d/extension-release."+generation.GeneratedConfextName), "ID=katlos")
	assertContains(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/systemd/network/80-katl-vmtest-dhcp.network"), "DHCP=yes")
	assertSymlink(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/systemd/system/multi-user.target.wants/bird.service"), "/usr/lib/systemd/system/bird.service")
	assertContains(t, filepath.Join(root, "etc/systemd/system/etc-kubernetes.mount"), "What=/var/lib/katl/kubernetes/etc-kubernetes")
	assertContains(t, filepath.Join(root, "etc/systemd/system/katl-kubeadm-ready.target"), "Requires=systemd-sysext.service systemd-confext.service containerd.service kubelet.service etc-kubernetes.mount")
	assertContains(t, filepath.Join(root, "etc/systemd/system/containerd.service.d/10-katl-runtime.conf"), "RequiresMountsFor=/var/lib/containerd")
	assertContains(t, filepath.Join(root, "etc/systemd/system/kubelet.service.d/10-katl-runtime.conf"), "Requires=containerd.service etc-kubernetes.mount")
	assertContains(t, filepath.Join(root, "etc/systemd/system/kubelet.service.d/10-katl-runtime.conf"), "ConditionPathExists=/var/lib/kubelet/config.yaml")
	assertContains(t, filepath.Join(root, "run/systemd/system/katl-generation-activate.service.d/10-katl-live-generation.conf"), "--generation 1")
	assertSymlink(t, filepath.Join(root, "run/extensions/katl-kubernetes.raw"), "/var/lib/katl/generations/1/sysext/katl-kubernetes.raw")
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "0" || selection.TargetBootGenerationID != "" || selection.TrialGenerationID != "" {
		t.Fatalf("boot selection changed = %#v", selection)
	}
	if _, err := os.Lstat(filepath.Join(root, "etc/systemd/system/multi-user.target.wants/katl-kubeadm-ready.target")); !os.IsNotExist(err) {
		t.Fatalf("baseline katl-kubeadm-ready.target was enabled: %v", err)
	}
}

func TestPrepareRejectsTamperedKubeadmInput(t *testing.T) {
	root := t.TempDir()
	plan := preparePlan(t, root)
	inputDir, err := installer.StoredKubeadmInputDir(root, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inputDir, "config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n")
	plan.RuntimeInputs.KubeadmInput.Digest = storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput)
	writeFile(t, filepath.Join(inputDir, "config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n")

	_, err = prepareSnapshot(t, root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "selected kubeadm input digest") {
		t.Fatalf("Prepare() error = %v, want stored input digest refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "var/lib/katl/generations/1/status.json")); !os.IsNotExist(statErr) {
		t.Fatalf("candidate status exists after refusal: %v", statErr)
	}
}

func TestPrepareInitHealthOwnership(t *testing.T) {
	for _, tt := range []struct {
		name            string
		init            string
		withoutEndpoint bool
	}{
		{name: "operator phases", init: "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nskipPhases: [addon/kube-proxy]\n"},
		{name: "already delegated", init: "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nskipPhases: [addon/kube-proxy, wait-control-plane]\n"},
		{name: "implicit init"},
		{name: "without endpoint", init: "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nskipPhases: [addon/kube-proxy]\n", withoutEndpoint: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			plan := preparePlan(t, root)
			if tt.withoutEndpoint {
				plan.RuntimeInputs.HostConfig.ControlPlaneEndpoint = ""
			}
			inputDir := filepath.Join(root, "var/lib/katl/cluster/kubeadm/control-plane")
			shared := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n---\napiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\nhealthzPort: 10248\nrotateCertificates: false\nclusterDNS: []\n"
			source := shared
			if tt.init != "" {
				source = tt.init + "---\n" + shared
			}
			patch := `[{"op":"test","path":"/maxPods","value":110},{"op":"replace","path":"/maxPods","value":112}]` + "\n"
			writeFile(t, filepath.Join(inputDir, "config.yaml"), source)
			writeFile(t, filepath.Join(inputDir, "patches/kubeletconfiguration+json.json"), patch)
			plan.RuntimeInputs.KubeadmInput.Digest = storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput)

			_, err := prepareSnapshot(t, root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			renderedDir := filepath.Join(root, "var/lib/katl/generations/1/confext/etc/katl/kubeadm/control-plane")
			content, err := os.ReadFile(filepath.Join(renderedDir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			decoder := yaml.NewDecoder(bytes.NewReader(content))
			var initCount int
			var kubeletCount int
			for {
				var document struct {
					Kind               string   `yaml:"kind"`
					APIVersion         string   `yaml:"apiVersion"`
					SkipPhases         []string `yaml:"skipPhases"`
					MaxPods            int      `yaml:"maxPods"`
					HealthzPort        int      `yaml:"healthzPort"`
					RotateCertificates bool     `yaml:"rotateCertificates"`
					ClusterDNS         []string `yaml:"clusterDNS"`
				}
				if err := decoder.Decode(&document); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				switch document.Kind {
				case "InitConfiguration":
					initCount++
					want := []string{"wait-control-plane"}
					if tt.init != "" {
						want = []string{"addon/kube-proxy", "wait-control-plane"}
					}
					if document.APIVersion != "kubeadm.k8s.io/v1beta4" || !slices.Equal(document.SkipPhases, want) {
						t.Fatalf("managed init = %+v, want skipPhases %v", document, want)
					}
				case "KubeletConfiguration":
					kubeletCount++
					if document.MaxPods != 110 || document.HealthzPort != 10248 || document.RotateCertificates || document.ClusterDNS == nil || len(document.ClusterDNS) != 0 {
						t.Fatalf("shared kubelet input changed: %+v", document)
					}
				}
			}
			if initCount != 1 || kubeletCount != 1 {
				t.Fatalf("rendered %d init documents and %d kubelet documents", initCount, kubeletCount)
			}
			for _, path := range []string{filepath.Join(inputDir, "config.yaml"), filepath.Join(root, "var/lib/katl/generations/1/kubeadm-input/control-plane/config.yaml")} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != source {
					t.Fatalf("stored source changed at %s: %s, %v", path, got, err)
				}
			}
			gotPatch, err := os.ReadFile(filepath.Join(renderedDir, "patches/kubeletconfiguration+json.json"))
			if err != nil || string(gotPatch) != patch {
				t.Fatalf("native patch changed: %s, %v", gotPatch, err)
			}
		})
	}
}

func TestPrepareJoinPreservesHealthOwnership(t *testing.T) {
	for _, kind := range []string{bootstrapplan.OperationKindJoinWorker, bootstrapplan.OperationKindJoinControlPlane} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			plan := preparePlan(t, root)
			plan.Operation.OperationKind = kind
			source := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\nskipPhases: [preflight]\n"
			writeFile(t, filepath.Join(root, "var/lib/katl/cluster/kubeadm/control-plane/config.yaml"), source)
			plan.RuntimeInputs.KubeadmInput.Digest = storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput)

			_, err := prepareSnapshot(t, root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(root, "var/lib/katl/generations/1/confext/etc/katl/kubeadm/control-plane/config.yaml"))
			if err != nil || string(got) != source {
				t.Fatalf("join configuration changed: %s, %v", got, err)
			}
		})
	}
}

func TestPrepareRejectsMalformedSkipPhases(t *testing.T) {
	for _, value := range []string{"addon/kube-proxy", "[addon/kube-proxy, 3]"} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			plan := preparePlan(t, root)
			writeFile(t, filepath.Join(root, "var/lib/katl/cluster/kubeadm/control-plane/config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nskipPhases: "+value+"\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n")
			plan.RuntimeInputs.KubeadmInput.Digest = storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput)

			_, err := prepareSnapshot(t, root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
			if err == nil || !strings.Contains(err.Error(), "InitConfiguration.skipPhases must be a list of strings") {
				t.Fatalf("Prepare() error = %v, want actionable skipPhases error", err)
			}
			if _, err := os.Stat(filepath.Join(root, "var/lib/katl/generations/1")); !os.IsNotExist(err) {
				t.Fatalf("candidate materialized after invalid skipPhases: %v", err)
			}
		})
	}
}

func TestPrepareSharedBaseOwnership(t *testing.T) {
	const local = `{"kubeadm":{"configRef":"control-plane","configPath":"/etc/katl/kubeadm/control-plane/config.yaml","nodeLocalKubelet":true}}`
	for _, tt := range []struct {
		name, stored, inherited, live, kind, wantError string
	}{
		{name: "retained projected input", stored: `{"nodeLocalKubelet":true}`, wantError: "katlctl cluster apply --config <source>"},
		{name: "installed projected input", inherited: local, wantError: "katlctl cluster apply --config <source>"},
		{name: "untrusted shared hint", inherited: `{"kubeadm":{"configRef":"control-plane","configPath":"/etc/katl/kubeadm/control-plane/config.yaml","nodeLocalKubelet":true,"sharedKubeletBase":true}}`, wantError: "katlctl cluster apply --config <source>"},
		{name: "wrong selected path", inherited: `{"kubeadm":{"configRef":"control-plane","configPath":"/etc/katl/kubeadm/other/config.yaml","nodeLocalKubelet":true}}`, wantError: "selected node metadata kubeadm configPath"},
		{name: "malformed selected ownership", inherited: `{"kubeadm":{"configRef":"control-plane","configPath":"/etc/katl/kubeadm/control-plane/config.yaml","nodeLocalKubelet":"true"}}`, wantError: "decode selected node metadata kubeadm ownership"},
		{name: "foreign ref grants no permission", stored: `{"nodeLocalKubelet":true}`, inherited: `{"kubeadm":{"configRef":"other","sharedKubeletBase":true}}`, wantError: "katlctl cluster apply --config <source>"},
		{name: "foreign local hint ignored", inherited: `{"kubeadm":{"configRef":"other","configPath":"/etc/katl/kubeadm/other/config.yaml","nodeLocalKubelet":true}}`},
		{name: "plain legacy", inherited: `{"kubeadm":{"configRef":"control-plane","configPath":"/etc/katl/kubeadm/control-plane/config.yaml","nodeLocalKubelet":false}}`},
		{name: "live metadata ignored", live: local},
		{name: "proven shared base", stored: `{"nodeLocalKubelet":true,"sharedKubeletBase":true}`, inherited: local},
		{name: "join does not publish", stored: `{"nodeLocalKubelet":true}`, inherited: local, kind: bootstrapplan.OperationKindJoinControlPlane},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			plan := preparePlan(t, root)
			if tt.kind != "" {
				plan.Operation.OperationKind = tt.kind
			}
			inputDir := filepath.Join(root, "var/lib/katl/cluster/kubeadm/control-plane")
			writeFile(t, filepath.Join(inputDir, "config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n")
			if tt.stored != "" {
				writeFile(t, filepath.Join(inputDir, "katl-input.json"), tt.stored)
			}
			if tt.inherited != "" {
				writeFile(t, filepath.Join(root, "var/lib/katl/generations/0/confext/etc/katl/node.json"), tt.inherited)
			}
			if tt.live != "" {
				writeFile(t, filepath.Join(root, "etc/katl/node.json"), tt.live)
			}
			plan.RuntimeInputs.KubeadmInput.Digest = storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput)

			_, err := prepareSnapshot(t, root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Prepare() error = %v, want %q", err, tt.wantError)
				}
				if _, err := os.Stat(filepath.Join(root, "var/lib/katl/generations/1")); !os.IsNotExist(err) {
					t.Fatalf("candidate materialized after shared-base refusal: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput); got != plan.RuntimeInputs.KubeadmInput.Digest {
				t.Fatalf("ownership check changed stored input digest: got %s, want %s", got, plan.RuntimeInputs.KubeadmInput.Digest)
			}
		})
	}
}

func TestPreparePreservesConfiguration(t *testing.T) {
	root := t.TempDir()
	plan := preparePlan(t, root)
	current, err := configapply.ReadGenerationManifest(root, "0")
	if err != nil {
		t.Fatal(err)
	}
	content := "net.ipv4.ip_forward = 1\n"
	current.Node.HostConfiguration = manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		"forwarding": {State: manifest.HostConfigurationPresent, Files: []manifest.HostConfigurationFile{{
			Path: "/etc/sysctl.d/80-forwarding.conf", Content: &content, Mode: 0o644,
		}}},
	}}
	if err := configapply.WriteGenerationManifest(root, "0", current); err != nil {
		t.Fatal(err)
	}
	oldInput := kubeadmconfig.Plan{Config: kubeadmconfig.File{
		RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml",
		Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nnodeRegistration:\n  name: obsolete\n"), Mode: 0o644,
	}}
	if err := configapply.WriteGenerationKubeadmConfig(root, "0", "control-plane", oldInput); err != nil {
		t.Fatal(err)
	}
	newInput := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nnodeRegistration:\n  name: cp-1\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n"
	writeFile(t, filepath.Join(root, "var/lib/katl/cluster/kubeadm/control-plane/config.yaml"), newInput)
	desired, err := fixtureKubeadmInput(root, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if err := configapply.WriteGenerationKubeadmConfig(root, "0", "control-plane", desired); err != nil {
		t.Fatal(err)
	}
	plan.RuntimeInputs.KubeadmInput.Digest = storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput)

	if _, err := prepareSnapshot(t, root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "var/lib/katl/generations/0")); err != nil {
		t.Fatal(err)
	}
	got, err := configapply.ReadLocalGenerationConfiguration(root, "1")
	if err != nil {
		t.Fatal(err)
	}
	if content := got.Manifest.Node.HostConfiguration.Sets["forwarding"].Files[0].Content; content == nil || *content != "net.ipv4.ip_forward = 1\n" {
		t.Fatalf("bootstrap lost current host configuration: %#v", content)
	}
	if got.Manifest.Node.Kubernetes.Kubeadm.ConfigRef != "control-plane" || string(got.KubeadmConfigs["control-plane"].Config.Content) != newInput {
		t.Fatalf("bootstrap did not retain newly selected desired input: %#v", got.KubeadmConfigs)
	}
}

func TestPrepareReplacesSelectedPatchTree(t *testing.T) {
	root := t.TempDir()
	plan := preparePlan(t, root)
	previousEtc := filepath.Join(root, "var/lib/katl/generations/0/confext/etc")
	writeFile(t, filepath.Join(previousEtc, "katl/kubeadm/control-plane/patches/kubeletconfiguration999+merge.yaml"), "maxPods: 110\n")
	writeFile(t, filepath.Join(previousEtc, "katl/kubeadm/control-plane-extra/patches/kubeletconfiguration+merge.yaml"), "maxPods: 130\n")

	stored := filepath.Join(root, "var/lib/katl/cluster/kubeadm/control-plane")
	writeFile(t, filepath.Join(stored, "config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n")
	writeFile(t, filepath.Join(stored, "patches/00-native/kubeletconfiguration+merge.yaml"), "maxPods: 121\n")
	writeFile(t, filepath.Join(stored, "patches/10-node/kubeletconfiguration+merge.yaml"), "healthzPort: 0\n")
	desired, err := fixtureKubeadmInput(root, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if err := configapply.WriteGenerationKubeadmConfig(root, "0", "control-plane", desired); err != nil {
		t.Fatal(err)
	}
	plan.RuntimeInputs.KubeadmInput.Digest = storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput)

	if _, err := prepareSnapshot(t, root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	candidateEtc := filepath.Join(root, "var/lib/katl/generations/1/confext/etc")
	for path, want := range map[string]string{
		"katl/kubeadm/control-plane/patches/00-native/kubeletconfiguration+merge.yaml": "maxPods: 121\n",
		"katl/kubeadm/control-plane/patches/10-node/kubeletconfiguration+merge.yaml":   "healthzPort: 0\n",
		"katl/kubeadm/control-plane-extra/patches/kubeletconfiguration+merge.yaml":     "maxPods: 130\n",
	} {
		got, err := os.ReadFile(filepath.Join(candidateEtc, path))
		if err != nil || string(got) != want {
			t.Fatalf("candidate %s = %q, err = %v; want %q", path, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(candidateEtc, "katl/kubeadm/control-plane/patches/kubeletconfiguration999+merge.yaml")); !os.IsNotExist(err) {
		t.Fatalf("obsolete selected patch survived bootstrap preparation: %v", err)
	}
}

func TestPreparePreservesInstalledOwnership(t *testing.T) {
	for _, tampered := range []bool{false, true} {
		t.Run(fmt.Sprint(tampered), func(t *testing.T) {
			root := t.TempDir()
			plan := preparePlan(t, root)
			current, err := configapply.ReadGenerationManifest(root, "0")
			if err != nil {
				t.Fatal(err)
			}
			current.Node.Kubernetes.Kubeadm.ConfigRef = "control-plane"
			native, err := kubeadmconfig.PlanFromRenderedFiles("control-plane", []kubeadmconfig.File{
				{RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml", Content: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n---\napiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\n")},
				{RenderPath: "/etc/katl/kubeadm/control-plane/patches/10-node/kubeletconfiguration+merge.yaml", Content: []byte("maxPods: 121\n")},
			})
			if err != nil {
				t.Fatal(err)
			}
			native.NodeLocalKubelet, native.SharedKubeletBase = true, true
			if _, err := installer.WriteClusterIntent(installer.ClusterIntentRequest{
				TargetRoot: root, Manifest: current, GenerationID: "0", KubernetesVersion: "v1.36.1",
				KubeadmConfigs: map[string]kubeadmconfig.Plan{"control-plane": native},
			}); err != nil {
				t.Fatal(err)
			}
			intent, _, err := installer.ReadClusterIntent(root)
			if err != nil {
				t.Fatal(err)
			}
			plan.RuntimeInputs.KubeadmInput.Digest = intent.Kubeadm.InputDigest
			stored := filepath.Join(root, "var/lib/katl/cluster/kubeadm/control-plane")
			if tampered {
				writeFile(t, filepath.Join(stored, "katl-input.json"), `{"nodeLocalKubelet":true}`)
			}

			_, err = prepareSnapshot(t, root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
			if tampered {
				if err == nil || !strings.Contains(err.Error(), "selected kubeadm input digest") {
					t.Fatalf("modified ownership accepted: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "var/lib/katl/generations/1/status.json")); !os.IsNotExist(err) {
					t.Fatalf("candidate exists after ownership refusal: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(root, "var/lib/katl/generations/0")); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(stored); err != nil {
				t.Fatal(err)
			}
			got, err := configapply.ReadLocalGenerationConfiguration(root, "1")
			if err != nil {
				t.Fatal(err)
			}
			selected := got.KubeadmConfigs["control-plane"]
			if !selected.NodeLocalKubelet || !selected.SharedKubeletBase || !strings.Contains(string(selected.Config.Content), "maxPods: 110") || len(selected.Patches) != 1 || string(selected.Patches[0].Content) != "maxPods: 121\n" {
				t.Fatalf("candidate lost shared-base ownership: %+v", selected)
			}
			if _, err := os.Stat(filepath.Join(root, "var/lib/katl/generations/1/confext/etc/katl/kubeadm/control-plane/katl-input.json")); !os.IsNotExist(err) {
				t.Fatalf("ownership metadata rendered as native input: %v", err)
			}
		})
	}
}

func TestPrepareRejectsStaleStoredKubeadmInputFile(t *testing.T) {
	root := t.TempDir()
	plan := preparePlan(t, root)
	inputDir, err := installer.StoredKubeadmInputDir(root, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inputDir, "config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n")
	plan.RuntimeInputs.KubeadmInput.Digest = storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput)
	writeFile(t, filepath.Join(inputDir, "patches/stale.yaml"), "stale: true\n")

	_, err = prepareSnapshot(t, root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "selected kubeadm input digest") {
		t.Fatalf("Prepare() error = %v, want stored input digest refusal", err)
	}
}

func preparePlan(t *testing.T, root string) bootstrapplan.Plan {
	t.Helper()
	previous, previousStatus := writeGenerationZero(t, root)
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:          generation.APIVersion,
		Kind:                generation.BootSelectionKind,
		DefaultGenerationID: "0",
		BootedGenerationID:  "0",
		DefaultBootEntry:    "loader/entries/katl-0.conf",
		UpdatedAt:           time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	sysextPayload := []byte("kubernetes-sysext-payload")
	writeFile(t, filepath.Join(root, "var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw"), string(sysextPayload))
	return bootstrapplan.Plan{
		Operation: operation.OperationRecord{
			OperationID:           "bootstrap-init-01",
			OperationKind:         bootstrapplan.OperationKindInit,
			RequestDigest:         strings.Repeat("1", 64),
			CandidateGenerationID: "1",
			BootstrapRequest: &operation.BootstrapRequest{
				InventoryNodeName:        "cp-1",
				SystemRole:               "control-plane",
				KubernetesPayloadVersion: "v1.36.1",
				BootstrapProfileRef:      "control-plane",
				CandidateGenerationID:    "1",
			},
		},
		Previous:      previous,
		PreviousState: previousStatus,
		RuntimeInputs: bootstrapplan.RuntimeInputs{
			SelectedKubernetesSysext: bootstrapplan.SelectedKubernetesSysext{
				Path:             "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw",
				SHA256:           digest(sysextPayload),
				SizeBytes:        uint64(len(sysextPayload)),
				PayloadVersion:   "v1.36.1",
				ActivationPath:   "/run/extensions/katl-kubernetes.raw",
				Architecture:     "x86_64",
				RuntimeInterface: "katl-runtime-1",
			},
			HostConfig: bootstrapplan.HostConfig{
				NodeName:             "cp-1",
				SystemRole:           "control-plane",
				ControlPlaneEndpoint: "api.katl.test:6443",
				NodeMetadataPath:     "/etc/katl/node.json",
			},
			KubeadmInput: bootstrapplan.KubeadmInput{
				ConfigRef: "control-plane",
				Intent:    "control-plane",
				Path:      "/etc/katl/kubeadm/control-plane/config.yaml",
			},
			KubernetesProjection: bootstrapplan.KubernetesProjection{
				What:  generation.KubernetesSource,
				Where: generation.KubernetesTarget,
			},
		},
	}
}

func storedInputDigest(t *testing.T, root string, input bootstrapplan.KubeadmInput) string {
	t.Helper()
	selected, _, err := configapply.ReadEffectiveGenerationKubeadmConfig(root, "0", input.ConfigRef)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := selected.InputDigest()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func prepareSnapshot(t *testing.T, root string, plan bootstrapplan.Plan, now time.Time) (Result, error) {
	t.Helper()
	configuration, err := configapply.ReadGenerationConfiguration(root, plan.Previous.GenerationID)
	if err != nil {
		return Result{}, err
	}
	plan.SelectedInput = bootstrapplan.InputSelection{Configuration: configuration, Input: plan.RuntimeInputs.KubeadmInput}
	return Prepare(root, plan, now)
}

func fixtureKubeadmInput(root, ref string) (kubeadmconfig.Plan, error) {
	dir := filepath.Join(root, "var/lib/katl/cluster/kubeadm", ref)
	var files []kubeadmconfig.File
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files = append(files, kubeadmconfig.File{RenderPath: filepath.Join("/etc/katl/kubeadm", ref, relative), Content: content, Mode: info.Mode().Perm()})
		return nil
	}); err != nil {
		return kubeadmconfig.Plan{}, err
	}
	return kubeadmconfig.PlanFromInputFiles(ref, files)
}

func writeGenerationZero(t *testing.T, root string) (generation.GenerationSpec, generation.GenerationStatus) {
	t.Helper()
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		Root: generation.RootSelection{
			RuntimeVersion:        "0.1.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeArtifactSHA256: strings.Repeat("a", 64),
		},
		GenerationID: "0",
		UKIPath:      "/efi/EFI/Linux/katl-0.efi",
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/0/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("b", 64),
			Compatibility: generation.ConfextCompatibility{
				ID:           "katlos",
				VersionID:    "0.1.0",
				ConfextLevel: 1,
			},
		},
		CreatedAt: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := generation.SpecFromRecord(record)
	specDigest, err := generation.CanonicalSpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	status := generation.StatusFromRecord(record, specDigest)
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	if err := configapply.WriteGenerationManifest(root, "0", manifest.Manifest{
		APIVersion: manifest.APIVersion, Kind: manifest.Kind,
		Node: manifest.NodeConfig{
			Identity:   manifest.NodeIdentity{Hostname: "cp-1", SSH: manifest.SSHIdentity{AuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl"}}},
			SystemRole: "control-plane",
			Kubernetes: manifest.KubernetesConfig{Kubeadm: manifest.KubeadmReference{ConfigRef: "control-plane"}},
		},
		Install:     manifest.InstallConfig{WipeTarget: true, TargetDisk: manifest.DiskSelector{ByID: "disk/by-id/test"}},
		KatlosImage: manifest.KatlosImage{LocalRef: "images/katlos.raw", SHA256: strings.Repeat("a", 64), SizeBytes: 1024, Version: "0.1.0", Architecture: "x86_64", Role: "install"},
	}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "var/lib/katl/generations/0/confext/etc/systemd/network/80-katl-vmtest-dhcp.network"), "[Match]\nName=en*\n\n[Network]\nDHCP=yes\n")
	writeFile(t, filepath.Join(root, "var/lib/katl/generations/0/confext/etc/extension-release.d/extension-release.katl-node"), "ID=katlos\n")
	return spec, status
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertContains(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func assertSymlink(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("symlink %s -> %s, want %s", path, got, want)
	}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
