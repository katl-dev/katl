package main

import (
	"context"
	"fmt"
	"os"

	"github.com/katl-dev/katl/internal/katlc/agent"
	"github.com/katl-dev/katl/internal/vmtest/image/kubelethealth"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "katlc-instrumented:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	control := kubelethealth.Control{SocketDir: "/run/katl-vmtest"}
	switch {
	case len(args) == 0:
		return agent.Serve(context.Background(), agent.ServeConfig{KubeletHealthDeadline: control.WithDeadline})
	case len(args) == 2 && args[0] == kubelethealth.ExpireCommand:
		return control.Expire(args[1])
	default:
		return fmt.Errorf("expected no arguments to serve, or %s OPERATION_ID", kubelethealth.ExpireCommand)
	}
}
