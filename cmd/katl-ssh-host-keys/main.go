package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/katl-dev/katl/internal/sshhostkey"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "katl-ssh-host-keys: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("katl-ssh-host-keys", flag.ContinueOnError)
	keyPath := flags.String("key", sshhostkey.DefaultKeyPath, "persistent SSH private host-key path")
	keygenPath := flags.String("ssh-keygen", sshhostkey.DefaultKeygenPath, "ssh-keygen executable path")
	if err := flags.Parse(args); err != nil {
		return err
	}

	result, err := sshhostkey.Ensure(ctx, sshhostkey.Options{
		KeyPath:    *keyPath,
		KeygenPath: *keygenPath,
	})
	if err != nil {
		return err
	}
	if stdout != nil {
		action := "preserved"
		if result.Replaced {
			action = "replaced"
		}
		fmt.Fprintf(stdout, "katl-ssh-host-keys action=%s path=%s\n", action, *keyPath)
	}
	return nil
}
