package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/kubeadmplan"
	"github.com/katl-dev/katl/internal/installer/operation"
)

const nativeKubeletBase = "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\n"
const nativeKubeletPatched = "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 121\n"

func TestKubeletRefreshValidation(t *testing.T) {
	for _, tc := range []struct {
		name, kind                           string
		coordinator, uploaded, rejectPreview bool
	}{
		{name: "worker without join credentials", kind: "JoinConfiguration"},
		{name: "invalid worker preview", kind: "JoinConfiguration", rejectPreview: true},
		{name: "invalid coordinator config", kind: "InitConfiguration", coordinator: true},
		{name: "coordinator with completed upload", kind: "InitConfiguration", coordinator: true, uploaded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubeletStartup(t, root)
			writeKubernetesStatusFile(t, root, "etc/hostname", "node-1\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", nativeKubeletBase)
			store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
			if err != nil {
				t.Fatal(err)
			}
			body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
			body.Component = "kubelet"
			body.CoordinatorUpload = tc.coordinator
			if !tc.coordinator {
				body.ConfigName = "worker"
				body.ConfigPath = "/etc/katl/kubeadm/worker/config.yaml"
			}
			body.KubernetesPayloadVersion = "v1.36.1"
			body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
			body.DesiredConfigSHA256, err = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(nativeKubeletBase))
			if err != nil {
				t.Fatal(err)
			}
			selected := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: " + tc.kind + "\n---\n" + nativeKubeletBase
			writeKubernetesStatusFile(t, root, body.ConfigPath, selected)
			writeKubeletRefreshInputs(t, root, body, selected, map[string]string{"kubeletconfiguration+merge.yaml": "maxPods: 121\n"})
			if tc.uploaded {
				prior := body
				prior.ConfigUploadRan = true
				finished := time.Now().UTC().Add(-time.Second)
				if _, err := store.Create(operation.OperationRecord{OperationID: "shared-upload", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: "complete", KubeadmControlPlaneConfig: &prior, CompletedAt: &finished, ExternalMutationStarted: true, Terminal: true, Result: operation.ResultSucceeded}, "complete", finished); err != nil {
					t.Fatal(err)
				}
			}
			record, err := store.Create(operation.OperationRecord{OperationID: "refresh-validation", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("d", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}

			executor := NewExecutor(root, store, "agent-test")
			executor.Async = false
			uploads, writes, restarts := 0, 0, 0
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if result, ok := readyKubeletHealthProbe(argv); ok {
					return result
				}
				if slices.Contains(argv, "jsonpath={.data.kubelet}") {
					return ToolResult{Stdout: []byte(nativeKubeletBase)}
				}
				if slices.Contains(argv, "validate") {
					message := "invalid selected kubeadm configuration"
					if tc.kind == "JoinConfiguration" {
						message = "discovery: Invalid value: \"\": bootstrapToken or file must be set"
					}
					return ToolResult{ExitStatus: 1, Stderr: []byte(message)}
				}
				if slices.Contains(argv, "upload-config") {
					uploads++
				}
				if argv[0] == "/usr/bin/env" {
					if tc.rejectPreview {
						return ToolResult{ExitStatus: 1, Stderr: []byte("native kubelet patch validation failed")}
					}
					writeKubeletPreview(t, root, argv, nativeKubeletPatched)
					return ToolResult{}
				}
				if argv[0] == "/usr/bin/kubeadm" && slices.Contains(argv, "upgrade") {
					writes++
					writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", nativeKubeletPatched)
				}
				if slices.Contains(argv, "restart") {
					restarts++
				}
				return ToolResult{}
			}

			err = executor.Execute(context.Background(), record)
			completed, readErr := store.Read(record.OperationID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			expected := nativeKubeletPatched
			if (tc.coordinator && !tc.uploaded) || tc.rejectPreview {
				expected = nativeKubeletBase
				wantError := "invalid selected kubeadm configuration"
				if tc.rejectPreview {
					wantError = "native kubelet patch validation failed"
				}
				if err == nil || completed.Result == operation.ResultSucceeded || completed.ExternalMutationStarted || uploads != 0 || writes != 0 || restarts != 0 {
					t.Fatalf("invalid input was not refused before mutation: error=%v record=%+v uploads=%d writes=%d restarts=%d", err, completed, uploads, writes, restarts)
				}
				if !strings.Contains(err.Error(), wantError) {
					t.Fatalf("validation error=%v, want %q", err, wantError)
				}
			} else if err != nil || completed.Result != operation.ResultSucceeded || uploads != 0 || writes != 1 || restarts != 1 {
				t.Fatalf("local refresh failed: error=%v record=%+v uploads=%d writes=%d restarts=%d", err, completed, uploads, writes, restarts)
			}
			actual, readErr := os.ReadFile(filepath.Join(root, "var/lib/kubelet/config.yaml"))
			if readErr != nil || string(actual) != expected {
				t.Fatalf("persisted kubelet config=%s, error=%v", actual, readErr)
			}
		})
	}
}

func TestKubeletNativePatches(t *testing.T) {
	for _, tc := range []struct {
		name, before, preview, applied          string
		patches                                 map[string]string
		coordinator, invalid, priorFailedUpload bool
		unrecordedUpload                        bool
	}{
		{name: "ordinary patch", before: nativeKubeletBase, preview: nativeKubeletPatched, applied: nativeKubeletPatched, patches: map[string]string{"nested/kubeletconfiguration+merge.yaml": "maxPods: 121\n"}},
		{name: "removed patch", before: nativeKubeletPatched, preview: nativeKubeletBase, applied: nativeKubeletBase},
		{name: "shared upload masked locally", before: nativeKubeletPatched, preview: nativeKubeletPatched, patches: map[string]string{"kubeletconfiguration+merge.yaml": "maxPods: 121\n"}, coordinator: true},
		{name: "invalid patch after upload", before: nativeKubeletBase, coordinator: true, invalid: true, patches: map[string]string{"kubeletconfiguration+json.json": "[{\"op\":\"test\",\"path\":\"/maxPods\",\"value\":999}]"}},
		{name: "native apply drift", before: nativeKubeletBase, preview: nativeKubeletPatched, applied: nativeKubeletBase, patches: map[string]string{"kubeletconfiguration+merge.yaml": "maxPods: 121\n"}},
		{name: "revert failed shared upload", before: nativeKubeletBase, preview: nativeKubeletBase, coordinator: true, priorFailedUpload: true},
		{name: "revert interrupted shared upload", before: nativeKubeletBase, preview: nativeKubeletBase, coordinator: true, priorFailedUpload: true, unrecordedUpload: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubeletStartup(t, root)
			writeKubernetesStatusFile(t, root, "etc/hostname", "cp-1\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
			if err != nil {
				t.Fatal(err)
			}
			body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
			body.Component = "kubelet"
			body.KubernetesPayloadVersion = "v1.36.1"
			body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
			body.CoordinatorUpload = tc.coordinator
			body.SupportedFieldDelta = nil
			body.DesiredConfigSHA256, err = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(nativeKubeletBase))
			if err != nil {
				t.Fatal(err)
			}
			writeKubernetesStatusFile(t, root, body.ConfigPath, nativeKubeletBase)
			writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", tc.before)
			writeKubeletRefreshInputs(t, root, body, nativeKubeletBase, tc.patches)
			if tc.priorFailedUpload {
				for i, prior := range []struct {
					id                  string
					upload, coordinator bool
					result              string
				}{
					{"shared-a", true, true, operation.ResultSucceeded},
					{"failed-shared-b", true, true, operation.ResultFailedNeedsRepair},
					{"local-only-repair", false, false, operation.ResultSucceeded},
				} {
					copy := body
					copy.ConfigUploadRan, copy.CoordinatorUpload = prior.upload, prior.coordinator
					if prior.id == "failed-shared-b" && tc.unrecordedUpload {
						copy.ConfigUploadRan = false
					}
					if prior.id != "shared-a" {
						copy.DesiredConfigSHA256 = strings.Repeat("b", 64)
					}
					finished := time.Now().UTC().Add(time.Duration(i-3) * time.Second)
					if _, err := store.Create(operation.OperationRecord{CompletedAt: &finished, OperationID: prior.id, OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: "complete", KubeadmControlPlaneConfig: &copy, ExternalMutationStarted: true, Terminal: true, Result: prior.result}, "complete", time.Now().UTC().Add(time.Duration(i-3)*time.Second)); err != nil {
						t.Fatal(err)
					}
				}
			}
			newRecord := func(id string) operation.OperationRecord {
				t.Helper()
				copy := body
				record, err := store.Create(operation.OperationRecord{OperationID: id, OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("d", 64), Phase: "accepted", KubeadmControlPlaneConfig: &copy}, "accepted", time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				return record
			}
			record := newRecord("native-patch")
			executor := NewExecutor(root, store, "agent-test")
			executor.Async = false
			uploads, writes, restarts := 0, 0, 0
			previewPatches := ""
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if result, ok := readyKubeletHealthProbe(argv); ok {
					return result
				}
				if slices.Contains(argv, "jsonpath={.data.kubelet}") {
					if tc.priorFailedUpload && uploads == 0 {
						return ToolResult{Stdout: []byte(nativeKubeletBase + "registryPullQPS: 7\n")}
					}
					return ToolResult{Stdout: []byte(nativeKubeletBase)}
				}
				if slices.Contains(argv, "upload-config") {
					uploads++
					return ToolResult{}
				}
				if argv[0] == "/usr/bin/env" {
					assertKubeadmPatches(t, root, argv, tc.patches)
					if i := slices.Index(argv, "--patches"); i >= 0 {
						previewPatches = argv[i+1]
					}
					if tc.coordinator && uploads == 0 && strings.Contains(argv[1], "/native-patch/") {
						t.Fatal("preview used the old shared base before upload")
					}
					if tc.invalid {
						return ToolResult{ExitStatus: 1, Err: fmt.Errorf("native JSON patch test failed")}
					}
					writeKubeletPreview(t, root, argv, tc.preview)
					return ToolResult{}
				}
				if argv[0] == "/usr/bin/kubeadm" && slices.Contains(argv, "upgrade") {
					writes++
					assertKubeadmPatches(t, root, argv, tc.patches)
					if i := slices.Index(argv, "--patches"); i >= 0 && argv[i+1] != previewPatches {
						t.Fatal("apply used a different patch tree")
					}
					writeKubernetesStatusFile(t, root, "var/lib/kubelet/config.yaml", tc.applied)
				}
				if slices.Contains(argv, "restart") {
					restarts++
				}
				return ToolResult{}
			}
			err = executor.Execute(context.Background(), record)
			failed := tc.invalid || (tc.applied != "" && tc.applied != tc.preview)
			if (err != nil) != failed {
				t.Fatalf("execution error = %v, want failure=%v", err, failed)
			}
			completed, readErr := store.Read(record.OperationID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.coordinator && (uploads != 1 || !completed.KubeadmControlPlaneConfig.ConfigUploadRan) {
				t.Fatalf("shared upload was lost: uploads=%d record=%+v", uploads, completed)
			}
			expected := tc.before
			if tc.applied != "" {
				expected = tc.applied
			}
			actual, readErr := os.ReadFile(filepath.Join(root, "var/lib/kubelet/config.yaml"))
			if readErr != nil || string(actual) != expected {
				t.Fatalf("persisted config=%s, %v", actual, readErr)
			}
			if tc.invalid {
				if writes != 0 || restarts != 0 || !completed.RecoveryRequired {
					t.Fatalf("invalid native patch mutated local config or lost repair state: %d/%d %+v", writes, restarts, completed)
				}
				return
			}
			if failed {
				if completed.Result == operation.ResultSucceeded || restarts != 0 {
					t.Fatalf("native drift accepted: %+v", completed)
				}
				return
			}
			if completed.Result != operation.ResultSucceeded {
				t.Fatalf("result = %+v", completed)
			}
			if tc.before == tc.preview {
				if writes != 0 || restarts != 0 {
					t.Fatalf("matching native output rewrote/restarted: %d/%d", writes, restarts)
				}
			} else if writes != 1 || restarts != 1 {
				t.Fatalf("changed native output not applied: %d/%d", writes, restarts)
			}
			uploads, writes, restarts = 0, 0, 0
			// A repeated preview is read-only; the completed shared receipt prevents upload.
			if err := executor.Execute(context.Background(), newRecord("native-repeat")); err != nil {
				t.Fatal(err)
			}
			if uploads != 0 || writes != 0 || restarts != 0 {
				t.Fatalf("repeat mutated: uploads=%d writes=%d restarts=%d", uploads, writes, restarts)
			}
		})
	}
}

func TestSharedKubeletBaseProof(t *testing.T) {
	for _, tc := range []struct {
		name        string
		marked      bool
		wrongDigest bool
	}{
		{name: "legacy projected"}, {name: "new shared base", marked: true}, {name: "different base", marked: true, wrongDigest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
			body.Component = "kubelet"
			body.KubernetesPayloadVersion = "v1.36.1"
			body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
			body.NodeLocalKubelet = true
			body.CoordinatorUpload = true
			body.DesiredConfigSHA256, _ = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(nativeKubeletBase))
			writeKubeadmConfigFixture(t, root, body.DesiredGenerationID, body.ConfigName, nativeKubeletBase, nil)
			configuration, err := configapply.ReadGenerationConfiguration(root, body.DesiredGenerationID)
			if err != nil {
				t.Fatal(err)
			}
			plan := configuration.KubeadmConfigs[body.ConfigName]
			plan.NodeLocalKubelet = true
			plan.SharedKubeletBase = tc.marked
			configuration.KubeadmConfigs[body.ConfigName] = plan
			if err := configapply.WriteGenerationConfiguration(root, body.DesiredGenerationID, configuration); err != nil {
				t.Fatal(err)
			}
			if tc.wrongDigest {
				body.DesiredConfigSHA256 = strings.Repeat("f", 64)
			}
			err = validateSharedKubeletBase(root, &body)
			if (err == nil) != (tc.marked && !tc.wrongDigest) {
				t.Fatalf("ownership proof = %v", err)
			}
			if !tc.marked && !strings.Contains(err.Error(), "katlctl cluster apply --config <source>") {
				t.Fatalf("missing recovery guidance: %v", err)
			}
			if err == nil {
				return
			}
			store, storeErr := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
			if storeErr != nil {
				t.Fatal(storeErr)
			}
			record, storeErr := store.Create(operation.OperationRecord{OperationID: "legacy-proof", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("c", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
			if storeErr != nil {
				t.Fatal(storeErr)
			}
			executor := NewExecutor(root, store, "agent-test")
			executor.Async = false
			executor.RunTool = func(context.Context, []string, func(int)) ToolResult {
				t.Fatal("unproven shared base reached a native command")
				return ToolResult{}
			}
			if err := executor.Execute(context.Background(), record); err == nil {
				t.Fatal("unproven shared base accepted")
			}
			refused, _ := store.Read(record.OperationID)
			if refused.ExternalMutationStarted || refused.MutatingToolRan {
				t.Fatalf("unproven base mutated: %+v", refused)
			}
		})
	}
}

func TestKubeletPreviewInputs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		files     map[string]string
		wantError bool
	}{
		{name: "native instance", files: map[string]string{"instance-config.yaml": "containerRuntimeEndpoint: unix:///run/operator.sock\n"}},
		{name: "legacy flags", files: map[string]string{"kubeadm-flags.env": "KUBELET_KUBEADM_ARGS=\"--container-runtime-endpoint=unix:///run/legacy.sock\"\n"}},
		{name: "missing native instance", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			executor := NewExecutor(root, operation.Store{}, "agent-test")
			for name, content := range tc.files {
				writeKubernetesStatusFile(t, root, filepath.Join("var/lib/kubelet", name), content)
			}
			staleDir := "var/lib/katl/operations/preview/tools/kubelet-preview"
			writeKubernetesStatusFile(t, root, filepath.Join(staleDir, "config.yaml"), "stale preview")
			writeKubernetesStatusFile(t, root, filepath.Join(staleDir, "instance-config.yaml"), "stale instance")
			dir, err := executor.prepareKubeletPreview("preview")
			if (err != nil) != tc.wantError {
				t.Fatalf("prepare error = %v", err)
			}
			if tc.wantError {
				return
			}
			for name, content := range tc.files {
				got, err := os.ReadFile(filepath.Join(root, dir, name))
				if err != nil || string(got) != content {
					t.Fatalf("native seed %s = %q, %v", name, got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, dir, "config.yaml")); !os.IsNotExist(err) {
				t.Fatalf("stale preview retained: %v", err)
			}
			if _, exists := tc.files["instance-config.yaml"]; !exists {
				if _, err := os.Stat(filepath.Join(root, dir, "instance-config.yaml")); !os.IsNotExist(err) {
					t.Fatalf("stale instance retained: %v", err)
				}
			}
		})
	}
}

func TestSharedKubeletReceipt(t *testing.T) {
	root := t.TempDir()
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(root, store, "agent-test")
	body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
	body.Component = "kubelet"
	body.KubernetesPayloadVersion = "v1.36.1"
	body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
	now := time.Now().UTC()
	for i, tc := range []struct {
		id       string
		uploaded bool
		result   string
		digest   string
	}{
		{"shared-upload", true, operation.ResultSucceeded, strings.Repeat("a", 64)},
		{"newer-local-mutation", false, operation.ResultSucceeded, strings.Repeat("b", 64)},
	} {
		copy := body
		copy.ConfigUploadRan = tc.uploaded
		copy.DesiredConfigSHA256 = tc.digest
		finished := now.Add(time.Duration(i) * time.Second)
		if _, err := store.Create(operation.OperationRecord{CompletedAt: &finished, OperationID: tc.id, OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("d", 64), Phase: "accepted", KubeadmControlPlaneConfig: &copy, Result: tc.result, Terminal: true}, "accepted", now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := executor.lastSharedKubeletUpload("current")
	if err != nil || got != strings.Repeat("a", 64) {
		t.Fatalf("shared receipt = %q, %v", got, err)
	}
}
