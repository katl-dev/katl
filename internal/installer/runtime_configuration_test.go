package installer

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestInstallRuntimeConfiguration(t *testing.T) {
	for _, media := range []bool{false, true} {
		for _, test := range []struct {
			name, declaration                    string
			marked, supported, legacy, noKubeadm bool
		}{
			{name: "supported", declaration: "shared-kubelet-base-v1\n", marked: true, supported: true},
			{name: "missing", marked: true},
			{name: "empty", declaration: "\n", marked: true},
			{name: "unknown", declaration: "future-feature-v2\n", marked: true},
			{name: "different version", declaration: "shared-kubelet-base-v10\n", marked: true},
			{name: "malformed", declaration: "shared-kubelet-base-v1\nfuture feature\n", marked: true},
			{name: "additive", declaration: "future-feature-v2\nshared-kubelet-base-v1\n", marked: true, supported: true},
			{name: "plain", supported: true},
			{name: "retained unmarked", supported: true, legacy: true},
			{name: "no kubeadm", supported: true, noKubeadm: true},
		} {
			t.Run(test.name+map[bool]string{false: "/explicit", true: "/media"}[media], func(t *testing.T) {
				payload := runtimeConfigurationFixture(t, test.declaration)
				commands := &runtimeConfigurationCommands{}
				store := &MemoryStateStore{}
				path := runtimeConfigurationManifest(t, media)
				if test.noKubeadm {
					path = writeManifest(t)
					if media {
						path = writeManifestWithoutImage(t)
					}
				}
				resolver := &recordingKatlosResolver{payload: payload}
				install := &Context{ManifestPath: path, Commands: commands, Store: store, KatlosResolver: resolver, KubeadmConfigs: kubeadmPlans()}
				if media {
					install.DefaultKatlosImage = manifest.KatlosImage{LocalRef: "images/katlos-install.squashfs", SHA256: strings.Repeat("b", 64), SizeBytes: 4096, Version: "2026.7.0", Architecture: "x86_64", RuntimeInterface: "katl-runtime-1", Role: "install"}
					install.MediaKatlosResolver = resolver
					install.KatlosResolver = &recordingKatlosResolver{err: errString("explicit resolver used for media default")}
				}
				selected := install.KubeadmConfigs["control-plane"]
				selected.SharedKubeletBase = test.marked
				selected.NodeLocalKubelet = test.marked || test.legacy
				install.KubeadmConfigs["control-plane"] = selected
				unused := selected
				unused.SharedKubeletBase = true
				install.KubeadmConfigs["unused"] = unused

				err := NewRunner(Plan{loadManifestStep{}, verifyKatlosImageStep{}, stubStep{id: PlanInstall}, stubStep{id: PrepareDisk}}, install).Run(context.Background())
				if test.supported {
					if err != nil || !slices.Contains(install.Completed, PrepareDisk) {
						t.Fatalf("supported installation stopped: %v, completed=%v", err, install.Completed)
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), "katlctl install apply") || !strings.Contains(err.Error(), "runtime") {
						t.Fatalf("unsupported runtime error = %v", err)
					}
					if slices.Contains(install.Completed, PlanInstall) || slices.Contains(install.Completed, PrepareDisk) {
						t.Fatalf("unsupported runtime reached disk planning: %v", install.Completed)
					}
					status := store.Statuses[len(store.Statuses)-1]
					if status.State != installstatus.StateFailedBeforeMutation || status.DestructiveMutation {
						t.Fatalf("unsupported runtime status = %+v", status)
					}
				}
				if test.marked && commands.reads != 1 || !test.marked && commands.reads != 0 {
					t.Fatalf("capability reads = %d, selected marked = %v", commands.reads, test.marked)
				}
				if len(commands.Calls) != 0 {
					t.Fatalf("unexpected host mutations: %v", commands.Calls)
				}
			})
		}
	}
}

func TestInstallRuntimeConfigurationNeedsEvidence(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		install := &Context{ManifestPath: runtimeConfigurationManifest(t, false), Commands: &NoopCommandRunner{}, Store: &MemoryStateStore{}, KubeadmConfigs: kubeadmPlans()}
		selected := install.KubeadmConfigs["control-plane"]
		selected.SharedKubeletBase = true
		install.KubeadmConfigs["control-plane"] = selected
		if resolved {
			install.KatlosResolver = &recordingKatlosResolver{}
		}
		err := NewRunner(Plan{loadManifestStep{}, verifyKatlosImageStep{}, stubStep{id: PrepareDisk}}, install).Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "katlctl install apply") || slices.Contains(install.Completed, PrepareDisk) {
			t.Fatalf("resolved=%v error=%v completed=%v", resolved, err, install.Completed)
		}
	}
}

type runtimeConfigurationCommands struct {
	NoopCommandRunner
	reads int
}

func runtimeConfigurationManifest(t *testing.T, media bool) string {
	t.Helper()
	path := writeManifestWithNode(t, `, "kubernetes": {"kubeadm": {"configRef": "control-plane"}}`)
	if media {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		delete(document, "katlosImage")
		data, err = json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func (r *runtimeConfigurationCommands) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.reads++
	return (ExecCommandRunner{}).Output(ctx, name, args...)
}

func runtimeConfigurationFixture(t *testing.T, declaration string) katlosimage.Payload {
	t.Helper()
	for _, tool := range []string{"mksquashfs", "unsquashfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("runtime capability regression requires %s", tool)
		}
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sentinel"), []byte("runtime root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if declaration != "" {
		path := filepath.Join(root, "usr/lib/katl/configuration-capabilities-v1")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(declaration), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := filepath.Join(t.TempDir(), "root.squashfs")
	if output, err := exec.Command("mksquashfs", root, image, "-noappend", "-quiet", "-processors", "1").CombinedOutput(); err != nil {
		t.Fatalf("create runtime fixture: %v: %s", err, output)
	}
	return katlosimage.Payload{Root: filepath.Dir(image), Runtime: katlosimage.Component{Path: filepath.Base(image)}}
}
