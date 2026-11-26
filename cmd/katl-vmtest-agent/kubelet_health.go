package main

import (
	"context"
	"errors"

	"github.com/katl-dev/katl/internal/katlc/agent"
	"github.com/katl-dev/katl/internal/vmtest/image/kubelethealth"
)

func runKubeletHealth(args []string) error {
	control := kubelethealth.Control{SocketDir: "/run/katl-vmtest"}
	switch {
	case len(args) == 0:
		return agent.Serve(context.Background(), agent.ServeConfig{KubeletHealthDeadline: control.WithDeadline})
	case len(args) == 2 && args[0] == kubelethealth.ExpireCommand:
		return control.Expire(args[1])
	default:
		return errors.New("expected no arguments to serve, or expire OPERATION_ID")
	}
}
