package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/katl-dev/katl/internal/boothealth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) bootHealthReader() boothealth.Reader {
	return boothealth.Reader{
		Root: s.Root,
		RunCommand: func(ctx context.Context, argv []string) ([]byte, error) {
			if s.RunBootHealthStatus == nil {
				return nil, errors.New("systemctl runner is unavailable")
			}
			result := s.RunBootHealthStatus(ctx, argv, nil)
			if result.Err == nil && result.ExitStatus != 0 {
				result.Err = fmt.Errorf("systemctl exited with status %d", result.ExitStatus)
			}
			return result.Stdout, result.Err
		},
		ReadNetwork: s.readManagementNetwork,
	}
}

func (s *Server) nodeBootHealth(ctx context.Context) boothealth.Status {
	return s.bootHealthReader().Read(ctx)
}

// Boot health normally settles seconds after the agent starts. An unproven
// boot can wait 90 seconds for the management network, and the settle target
// stops waiting after 10 minutes.
var (
	bootHealthAdmissionWait     = 5 * time.Minute
	bootHealthAdmissionInterval = 250 * time.Millisecond
	bootHealthReplyMargin       = 2 * time.Second
)

// awaitBootHealth holds a mutation until boot health settles for this boot.
// Boot health rejects a boot whose pending trial changed underneath it and
// resets the default to the booted generation, so a mutation admitted earlier
// could trigger recovery or undo a live promotion. Boot health settles with
// any result, including when it cannot run, so a broken boot never blocks the
// mutations that repair it.
func (s *Server) awaitBootHealth(ctx context.Context) error {
	deadline := time.Now().Add(bootHealthAdmissionWait)
	// Reply before the caller's deadline so it sees why it was refused.
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Add(-bootHealthReplyMargin).Before(deadline) {
		deadline = callerDeadline.Add(-bootHealthReplyMargin)
	}
	reader := s.bootHealthReader()
	for reader.Settling(ctx) {
		if !time.Now().Before(deadline) {
			return status.Error(codes.Unavailable, "node is completing boot health for this boot; retry when `katlctl node status` no longer reports health pending")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bootHealthAdmissionInterval):
		}
	}
	return nil
}
