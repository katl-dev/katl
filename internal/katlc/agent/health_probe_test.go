package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHealthProbeBudget(t *testing.T) {
	t.Run("each command has its own budget", func(t *testing.T) {
		t.Parallel()
		run := boundedHealthRunner(func(ctx context.Context, _ []string, _ func(int)) ToolResult {
			select {
			case <-time.After(3 * time.Second):
				return ToolResult{}
			case <-ctx.Done():
				return ToolResult{Err: ctx.Err(), ExitStatus: 1}
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for range 2 {
			if result := run(ctx, []string{"native-health"}, nil); result.Err != nil || result.ExitStatus != 0 {
				t.Fatalf("individually bounded observation failed: %+v", result)
			}
		}
	})
	t.Run("command timeout survives process result", func(t *testing.T) {
		t.Parallel()
		run := boundedHealthRunner(func(ctx context.Context, _ []string, _ func(int)) ToolResult {
			<-ctx.Done()
			return ToolResult{Err: errors.New("signal: killed"), ExitStatus: -1}
		})
		result := run(context.Background(), []string{"native-health"}, nil)
		if !errors.Is(result.Err, context.DeadlineExceeded) || kubeletProbeExecutionError(context.Background(), result) == nil {
			t.Fatalf("timeout misclassified as negative health: %+v", result)
		}
	})
	t.Run("parent cancellation wins", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		run := boundedHealthRunner(func(context.Context, []string, func(int)) ToolResult { return ToolResult{} })
		result := run(ctx, []string{"native-health"}, nil)
		if !errors.Is(result.Err, context.Canceled) || result.ExitStatus == 0 {
			t.Fatalf("canceled observation accepted: %+v", result)
		}
	})
	t.Run("completed negative result remains negative", func(t *testing.T) {
		run := boundedHealthRunner(func(context.Context, []string, func(int)) ToolResult { return ToolResult{ExitStatus: 3} })
		result := run(context.Background(), []string{"native-health"}, nil)
		if result.ExitStatus != 3 || kubeletProbeExecutionError(context.Background(), result) != nil {
			t.Fatalf("completed negative observation became unavailable: %+v", result)
		}
	})
}
