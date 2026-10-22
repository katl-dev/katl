package vmtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

func TestBootComplete(t *testing.T) {
	for _, tc := range []struct {
		name, inactive, health, wantError string
		delay                             time.Duration
	}{
		{name: "previous successful health", delay: time.Second},
		{name: "boot never completes", delay: time.Minute, wantError: "deadline exceeded"},
		{name: "SSH unavailable", inactive: "sshd.service", wantError: "sshd.service"},
		{name: "management unavailable", inactive: "katlc-agent.service", wantError: "katlc-agent.service"},
		{name: "failed health", health: "exit-code", wantError: "boot-health result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				client := &bootHealthClient{
					firstInstallHealthClient: firstInstallHealthClient{health: first(tc.health, "success"), inactive: tc.inactive},
					readyAt:                  start.Add(tc.delay),
				}
				guest := NewGuestControl(guestResult(t), client)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()

				err := guest.WaitBootComplete(ctx)
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("error = %v, want %q", err, tc.wantError)
					}
					if tc.delay > 2*time.Second && !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("deadline cause = %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if time.Now().Before(client.readyAt) {
					t.Fatal("accepted successful health before the current boot completed")
				}
			})
		})
	}
}

type bootHealthClient struct {
	firstInstallHealthClient
	readyAt time.Time
}

func (c *bootHealthClient) RunCommand(ctx context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	if req.Argv[1] == "is-active" && req.Argv[len(req.Argv)-1] == "katl-boot-complete.target" && time.Now().Before(c.readyAt) {
		return &vmtestpb.CommandResult{ExitStatus: 3}, nil
	}
	return c.firstInstallHealthClient.RunCommand(ctx, req)
}
