package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
)

func TestControlPlaneManifestConvergence(t *testing.T) {
	const ready = `[{"type":"ContainersReady","status":"True"}]`
	for _, tc := range []struct {
		name, hash, conditions string
		formatOnly             bool
		deleting               bool
		wantError              bool
	}{
		{name: "old Ready pod", hash: "before-api", conditions: ready, wantError: true},
		{name: "changed pod not Ready", hash: "after-api", conditions: `[{"type":"ContainersReady","status":"False"}]`, wantError: true},
		{name: "changed Ready pod", hash: "after-api", conditions: ready},
		{name: "node controller marks pod not Ready", hash: "after-api", conditions: `[{"type":"Ready","status":"False"},{"type":"ContainersReady","status":"True"}]`},
		{name: "PodReady cannot replace container health", hash: "after-api", conditions: `[{"type":"Ready","status":"True"},{"type":"ContainersReady","status":"False"}]`, wantError: true},
		{name: "missing readiness", hash: "after-api", conditions: `[]`, wantError: true},
		{name: "ambiguous readiness", hash: "after-api", conditions: `[{"type":"ContainersReady","status":"False"},{"type":"ContainersReady","status":"True"}]`, wantError: true},
		{name: "terminating Ready pod", hash: "after-api", conditions: ready, deleting: true, wantError: true},
		{name: "formatting preserves native identity", hash: "before-api", conditions: ready, formatOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			store := operation.Store{Root: filepath.Join(root, "operations")}
			record := operation.OperationRecord{OperationID: "configure", KubeadmControlPlaneConfig: &operation.KubeadmControlPlaneConfig{NodeName: "cp-1"}}
			for _, component := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
				before := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: " + component + "\nspec:\n  containers:\n  - name: " + component + "\n    image: example.invalid/component:v1\n"
				after := before
				if component == "kube-apiserver" {
					if tc.formatOnly {
						after = "# Rendered with different formatting\nkind: Pod\napiVersion: v1\nmetadata: {name: kube-apiserver}\nspec:\n  containers: [{image: example.invalid/component:v1, name: kube-apiserver}]\n"
					} else {
						after = strings.Replace(before, "component:v1", "component:v2", 1)
					}
				}
				writeKubernetesStatusFile(t, root, "operations/configure/manifest-backup/"+component+".yaml", before)
				writeKubernetesStatusFile(t, root, "etc/kubernetes/manifests/"+component+".yaml", after)
			}
			before := map[string]controlPlaneMirror{
				"kube-apiserver.yaml":          {hash: "before-api", containersReady: true},
				"kube-controller-manager.yaml": {hash: "controller", containersReady: true},
				"kube-scheduler.yaml":          {hash: "scheduler", containersReady: true},
			}
			api := mirrorPod("kube-apiserver", tc.hash, tc.conditions)
			if tc.deleting {
				api = strings.Replace(api, `"metadata":{`, `"metadata":{"deletionTimestamp":"2026-01-01T00:00:00Z",`, 1)
			}
			pods := mirrorList(
				api,
				mirrorPod("kube-controller-manager", "controller", ready),
				mirrorPod("kube-scheduler", "scheduler", ready),
			)
			// Bound a failed convergence without asserting polling intervals or
			// elapsed time. The native response itself remains successful.
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			executor := Executor{Root: root, Store: store, RunTool: func(context.Context, []string, func(int)) ToolResult {
				return ToolResult{Stdout: []byte(pods)}
			}}

			err := executor.waitControlPlaneManifests(ctx, record, before)
			if tc.wantError {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("unconverged native pod was accepted or lost cancellation: %v", err)
				}
			} else if err != nil {
				t.Fatalf("converged native manifests rejected: %v", err)
			}
		})
	}
}

func TestControlPlaneMirrorEvidence(t *testing.T) {
	const ready = `[{"type":"ContainersReady","status":"True"}]`
	api := mirrorPod("kube-apiserver", "api", ready)
	controller := mirrorPod("kube-controller-manager", "controller", ready)
	scheduler := mirrorPod("kube-scheduler", "scheduler", ready)
	valid := mirrorList(api, controller, scheduler)
	for _, tc := range []struct {
		name, pods string
		toolError  error
		wantError  bool
	}{
		{name: "native mirrors", pods: valid},
		{name: "malformed JSON", pods: `{`, wantError: true},
		{name: "missing pod", pods: mirrorList(api, controller), wantError: true},
		{name: "duplicate pod", pods: mirrorList(api, api, controller, scheduler), wantError: true},
		{name: "wrong node", pods: strings.Replace(valid, `"nodeName":"cp-1"`, `"nodeName":"cp-2"`, 1), wantError: true},
		{name: "unexpected pod", pods: strings.Replace(valid, `"name":"kube-apiserver-cp-1"`, `"name":"unrelated"`, 1), wantError: true},
		{name: "missing native hash", pods: strings.Replace(valid, `"kubernetes.io/config.hash":"api"`, `"kubernetes.io/config.hash":""`, 1), wantError: true},
		{name: "mismatched mirror identity", pods: strings.Replace(valid, `"kubernetes.io/config.mirror":"api"`, `"kubernetes.io/config.mirror":"other"`, 1), wantError: true},
		{name: "repeated container readiness", pods: strings.Replace(valid, ready, `[{"type":"ContainersReady","status":"True"},{"type":"ContainersReady","status":"False"}]`, 1), wantError: true},
		{name: "invalid container readiness", pods: strings.Replace(valid, ready, `[{"type":"ContainersReady","status":"invalid"}]`, 1), wantError: true},
		{name: "failed request with plausible stdout", pods: valid, toolError: errors.New("API connection failed"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := Executor{Root: t.TempDir(), RunTool: func(context.Context, []string, func(int)) ToolResult {
				return ToolResult{Stdout: []byte(tc.pods), Err: tc.toolError}
			}}

			mirrors, err := executor.controlPlaneMirrors(context.Background(), "cp-1")
			if (err != nil) != tc.wantError {
				t.Fatalf("mirror evidence error = %v, want error %v", err, tc.wantError)
			}
			if !tc.wantError && (mirrors["kube-apiserver.yaml"].hash != "api" || !mirrors["kube-apiserver.yaml"].containersReady) {
				t.Fatalf("native mirror identity/readiness lost: %+v", mirrors)
			}
		})
	}
}

func mirrorPod(component, hash, conditions string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"annotations":{"kubernetes.io/config.hash":%q,"kubernetes.io/config.mirror":%q}},"spec":{"nodeName":"cp-1"},"status":{"conditions":%s}}`, component+"-cp-1", hash, hash, conditions)
}

func mirrorList(pods ...string) string {
	return `{"apiVersion":"v1","kind":"List","items":[` + strings.Join(pods, ",") + `]}`
}
