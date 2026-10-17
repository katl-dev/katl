package kubeadmconfig

import (
	"reflect"
	"strings"
	"testing"
)

func TestSharedKubeletBase(t *testing.T) {
	for _, tt := range []struct {
		name          string
		local, shared bool
		wantError     bool
	}{
		{name: "plain legacy"},
		{name: "projected legacy", local: true, wantError: true},
		{name: "shared base", shared: true},
		{name: "local overlay with shared base", local: true, shared: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := Plan{NodeLocalKubelet: tt.local, SharedKubeletBase: tt.shared}
			err := plan.ValidateSharedKubeletBase()
			if (err != nil) != tt.wantError {
				t.Fatalf("shared-base publication error = %v, want error %t", err, tt.wantError)
			}
			if err != nil && !strings.Contains(err.Error(), "katlctl cluster apply --config <source>") {
				t.Fatalf("publication refusal lacks recovery guidance: %v", err)
			}
		})
	}
}

func TestInputFiles(t *testing.T) {
	plan := Plan{
		Name: "control-plane", NodeLocalKubelet: true, SharedKubeletBase: true,
		Config: File{RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml", Content: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"), Mode: 0o644},
	}
	files, err := plan.InputFiles()
	if err != nil {
		t.Fatal(err)
	}
	want := []File{
		plan.Config,
		{RenderPath: "/etc/katl/kubeadm/control-plane/katl-input.json", Content: []byte(`{"nodeLocalKubelet":true,"sharedKubeletBase":true}`), Mode: 0o644},
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("persisted inputs = %+v, want %+v", files, want)
	}
	if len(plan.NativeEtcFiles()) != 1 {
		t.Fatal("ownership metadata became a native kubeadm file")
	}
}

func TestInputOwnership(t *testing.T) {
	for _, test := range []struct {
		name, metadata string
		local, shared  bool
		invalid        bool
	}{
		{name: "legacy"},
		{name: "projected", metadata: `{"nodeLocalKubelet":true}`, local: true},
		{name: "shared base", metadata: `{"nodeLocalKubelet":true,"sharedKubeletBase":true}`, local: true, shared: true},
		{name: "invalid", metadata: `{"sharedKubeletBase":"true"}`, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := []File{{RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml", Content: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n")}}
			if test.metadata != "" {
				files = append(files, File{RenderPath: "/etc/katl/kubeadm/control-plane/katl-input.json", Content: []byte(test.metadata)})
			}
			plan, err := PlanFromInputFiles("control-plane", files)
			if test.invalid {
				if err == nil {
					t.Fatal("malformed ownership accepted")
				}
				return
			}
			if err != nil || plan.NodeLocalKubelet != test.local || plan.SharedKubeletBase != test.shared || len(plan.NativeEtcFiles()) != 1 {
				t.Fatalf("decoded ownership = %+v, error = %v", plan, err)
			}
		})
	}
}

func TestInputDigest(t *testing.T) {
	plan := Plan{
		Name:   "control-plane",
		Config: File{RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml", Content: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"), Mode: 0o644},
	}
	digest, err := plan.InputDigest()
	// Previously installed native-only intent must remain readable unchanged.
	if err != nil || digest != "06fc237a564fdbd0ef82567e1f98349f7102bea05dcc2ac5cf83c362eedfb29b" {
		t.Fatalf("legacy digest = %q, error = %v", digest, err)
	}
	seen := map[string]bool{digest: true}
	for _, ownership := range [][2]bool{{true, false}, {false, true}, {true, true}} {
		plan.NodeLocalKubelet, plan.SharedKubeletBase = ownership[0], ownership[1]
		digest, err := plan.InputDigest()
		if err != nil || seen[digest] {
			t.Fatalf("ownership %v did not distinguish input identity: %q, %v", ownership, digest, err)
		}
		seen[digest] = true
	}
}
