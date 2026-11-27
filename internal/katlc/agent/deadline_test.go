package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/deadline"
	"github.com/katl-dev/katl/internal/installer/operation"
)

// Unit tests bound kubelet health with their own contexts. Arming the
// production deadline would start a systemd timer on the test host.
func init() {
	armDeadline = func(ctx context.Context, _ deadline.Kind, _ string) (context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithCancel(ctx)
		return ctx, cancel, nil
	}
}

// Post-apply kubelet health is bounded by the kubelet-config-health timer for
// this operation, and its expiry ends the health check.
func TestKubeletHealthArmsConfigHealthDeadline(t *testing.T) {
	root := t.TempDir()
	store, record := kubeletHealthOperation(t, root, &operation.KubeadmControlPlaneConfig{
		Component: "kubelet", NodeName: "worker-1", RolloutID: "rollout-1",
	})
	executor := NewExecutor(root, store, "agent-1")
	executor.RunTool = func(context.Context, []string, func(int)) ToolResult {
		return ToolResult{ExitStatus: 1, Stderr: []byte("unavailable")}
	}

	var kinds []deadline.Kind
	var subject string
	production := armDeadline
	t.Cleanup(func() { armDeadline = production })
	armDeadline = func(ctx context.Context, kind deadline.Kind, s string) (context.Context, context.CancelFunc, error) {
		kinds, subject = append(kinds, kind), s
		ctx, cancel := context.WithCancelCause(ctx)
		cancel(context.DeadlineExceeded)
		return ctx, func() {}, nil
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result := executor.runKubeletConfigHealth(ctx, record)
	if len(kinds) != 1 || kinds[0] != deadline.KubeletConfigHealth {
		t.Fatalf("kubelet health armed %q, want one %s deadline", kinds, deadline.KubeletConfigHealth)
	}
	if !strings.Contains(subject, "operation="+record.OperationID) || !strings.Contains(subject, "rollout=rollout-1") {
		t.Fatalf("armed deadline subject = %q, want this operation and rollout", subject)
	}
	if !errors.Is(result.Err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("health result = %+v (parent %v), want the armed deadline's expiry", result, ctx.Err())
	}
}
