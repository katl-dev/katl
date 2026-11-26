package kubelethealth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
)

type errHookContext struct {
	context.Context
	afterErr func()
}

func (c errHookContext) Err() error {
	err := c.Context.Err()
	c.afterErr()
	return err
}

func TestErrDuringForcedExpiry(t *testing.T) {
	control := Control{SocketDir: t.TempDir()}
	record := checkpointRecord()
	armed, cancel := control.WithDeadline(context.Background(), record, time.Minute)
	defer cancel()

	health := deadlineContext{errHookContext{
		Context: armed.(deadlineContext).Context,
		afterErr: sync.OnceFunc(func() {
			// Fire after the first error snapshot, including a read by context.Cause.
			if err := control.Expire(record.OperationID); err != nil {
				t.Fatal(err)
			}
		}),
	}}

	first, second, third := health.Err(), health.Err(), health.Err()
	if first != nil || second != context.DeadlineExceeded || third != second {
		t.Fatalf("successive Err calls = %v, %v, %v, want nil followed by two context.DeadlineExceeded errors", first, second, third)
	}
}

func TestExpireActiveOperation(t *testing.T) {
	control := Control{SocketDir: t.TempDir()}
	record := checkpointRecord()
	health, cancel := control.WithDeadline(context.Background(), record, time.Minute)
	defer cancel()
	probe, cancelProbe := context.WithTimeout(health, time.Minute)
	defer cancelProbe()

	if err := control.Expire("different-operation"); err == nil || health.Err() != nil {
		t.Fatalf("different operation expired the active health check: %v, %v", err, health.Err())
	}
	if err := control.Expire(record.OperationID); err != nil {
		t.Fatal(err)
	}
	if health.Err() != context.DeadlineExceeded {
		t.Fatalf("health context Err = %v, want context.DeadlineExceeded", health.Err())
	}
	select {
	case <-probe.Done():
		if probe.Err() != context.DeadlineExceeded {
			t.Fatalf("health probe Err = %v, want context.DeadlineExceeded", probe.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("health probe did not observe expiry")
	}

	cancel()
	if err := control.Expire(record.OperationID); err == nil {
		t.Fatal("completed health wait was still armed")
	}
}

func checkpointRecord() operation.OperationRecord {
	return operation.OperationRecord{
		OperationID:     "checkpoint",
		Phase:           "kubelet-restart-running",
		CompletedPhases: []string{"kubelet-restart-running"},
		KubeadmControlPlaneConfig: &operation.KubeadmControlPlaneConfig{
			Component: "kubelet",
			RolloutID: CheckpointRolloutPrefix + "1-kubelet",
		},
	}
}

func TestDeadlineArmsOnlyCheckpointRestart(t *testing.T) {
	for _, tc := range []struct {
		name, phase, rollout string
		completed            []string
	}{
		{"unchanged", "preflight-complete", CheckpointRolloutPrefix + "1-kubelet", nil},
		{"incomplete restart", "kubelet-restart-running", CheckpointRolloutPrefix + "1-kubelet", nil},
		{"repair", "kubelet-restart-running", "repair-kubelet", []string{"kubelet-restart-running"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := Control{SocketDir: t.TempDir()}
			record := operation.OperationRecord{
				OperationID:     "operation",
				Phase:           tc.phase,
				CompletedPhases: tc.completed,
				KubeadmControlPlaneConfig: &operation.KubeadmControlPlaneConfig{
					Component: "kubelet",
					RolloutID: tc.rollout,
				},
			}
			parent, stop := context.WithCancel(context.Background())
			defer stop()
			health, cancel := control.WithDeadline(parent, record, time.Minute)
			defer cancel()

			if err := control.Expire(record.OperationID); err == nil || health.Err() != nil {
				t.Fatalf("out-of-scope operation armed expiry: %v, %v", err, health.Err())
			}
			stop()
			if health.Err() != context.Canceled {
				t.Fatalf("after parent cancellation, Err = %v, want context.Canceled", health.Err())
			}
		})
	}
}
