// Package kubelethealth implements the checkpoint fault's instrumented agent control.
package kubelethealth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
)

const (
	AgentPath               = "/usr/lib/katl/vmtest/katlc-instrumented"
	CheckpointRolloutPrefix = "vmtest-kubelet-checkpoint-policy-"
	ArmedMessage            = "vmtest kubelet health armed "
	ExpireCommand           = "expire-kubelet-health"
)

type Control struct {
	SocketDir string
}

var errForcedExpiry = errors.New("vmtest forced kubelet health expiry")

type deadlineContext struct {
	context.Context
}

func (c deadlineContext) Err() error {
	err := c.Context.Err()
	if err == nil {
		return nil
	}
	// The cause is stable once the underlying context reports cancellation.
	if context.Cause(c.Context) == errForcedExpiry {
		return context.DeadlineExceeded
	}
	return err
}

func (c deadlineContext) Value(key any) any {
	// Child contexts must propagate this context's Err rather than find the
	// wrapped context's cancellation state through context's private key.
	return context.WithoutCancel(c.Context).Value(key)
}

func (c Control) WithDeadline(parent context.Context, record operation.OperationRecord, timeout time.Duration) (context.Context, context.CancelFunc) {
	health, cancel := context.WithTimeout(parent, timeout)
	request := record.KubeadmControlPlaneConfig
	if request == nil || request.Component != "kubelet" || !strings.HasPrefix(request.RolloutID, CheckpointRolloutPrefix) ||
		record.Terminal || record.Phase != "kubelet-restart-running" || !slices.Contains(record.CompletedPhases, record.Phase) {
		return health, cancel
	}

	path := filepath.Join(c.SocketDir, record.OperationID)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		log.Printf("arm kubelet health expiry for %s: %v", record.OperationID, err)
		return health, cancel
	}

	armed, expire := context.WithCancelCause(health)
	go func() {
		defer listener.Close()
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		expire(errForcedExpiry)
		if context.Cause(armed) == errForcedExpiry {
			_, _ = conn.Write([]byte{1})
		}
	}()
	fmt.Fprintf(os.Stderr, "%s%s %s\n", ArmedMessage, record.OperationID, request.RolloutID)
	return deadlineContext{armed}, func() {
		cancel()
		_ = listener.Close()
	}
}

func (c Control) Expire(operationID string) error {
	if operationID == "" || filepath.Base(operationID) != operationID || operationID == "." || operationID == ".." {
		return fmt.Errorf("invalid operation ID %q", operationID)
	}
	deadline := time.Now().Add(5 * time.Second)
	conn, err := (&net.Dialer{Deadline: deadline}).Dial("unix", filepath.Join(c.SocketDir, operationID))
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 1 {
		return fmt.Errorf("kubelet health expiry was not acknowledged for %s", operationID)
	}
	return nil
}
