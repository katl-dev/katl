package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/katl-dev/katl/internal/deadline"
	"github.com/katl-dev/katl/internal/katlc/agent"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		code := 1
		if exit, ok := err.(commandExitError); ok {
			code = exit.code
			if exit.message == "" {
				os.Exit(code)
			}
		}
		fmt.Fprintf(os.Stderr, "katlc: %v\n", err)
		os.Exit(code)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("command is required")
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_, err := fmt.Fprint(stdout, helpText())
		return err
	}
	if args[0] == "--version" || args[0] == "version" {
		fmt.Fprintf(stdout, "katlc version=%s commit=%s date=%s\n", version, commit, date)
		return nil
	}
	if args[0] == "agent" {
		return runAgent(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "kubeadm" {
		return runKubeadm(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "volumes" {
		return runVolumes(ctx, args[1:], stderr)
	}
	return fmt.Errorf("unsupported command %q", strings.Join(args, " "))
}

func helpText() string {
	return `Usage: katlc <command> [args]

Commands:
  version                 Print build version metadata.
  agent serve             Run the KatlOS node management agent.
  agent rotate-management Replace installed mTLS credentials through root SSH.
  agent management-authority Print the installed mTLS authority fingerprint through root SSH.
  agent management-trial revert
                          Abandon an unconfirmed management transition and
                          restore the installed management mode.
  kubeadm plan            Compare selected desired kubeadm input with read-only live state.
  volumes check           Refuse when an active volume binding does not match the attached disks.

`
}

func runAgent(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("agent command is required")
	}
	switch args[0] {
	case "serve":
		return runAgentServe(ctx, args[1:], stdout, stderr)
	case "management-authority":
		return runAgentManagementAuthority(args[1:], stdout, stderr)
	case "rotate-management":
		return runAgentRotateManagement(ctx, args[1:], os.Stdin, stdout, stderr)
	case "management-trial":
		return runAgentManagementTrial(ctx, args[1:], stdout, stderr)
	case "expire-deadline":
		if len(args) != 2 {
			return fmt.Errorf("expire-deadline requires one deadline instance")
		}
		return deadline.Expire(args[1])
	case "run-extension-activation":
		if len(args) < 2 {
			return fmt.Errorf("run-extension-activation requires a command")
		}
		return deadline.Run(ctx, deadline.ExtensionActivation, args[1:], stdout, stderr)
	case "recover-links":
		if len(args) != 1 {
			return fmt.Errorf("recover-links takes no arguments")
		}
		return runAgentRecoverLinks(ctx, stderr)
	default:
		return fmt.Errorf("unsupported agent command %q", args[0])
	}
}

func runVolumes(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "check" {
		return fmt.Errorf("volumes command must be check")
	}
	flags := flag.NewFlagSet("katlc volumes check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "/", "runtime root containing /var/lib/katl")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return agent.CheckActiveVolumes(ctx, *root)
}

func runAgentServe(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katlc agent serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "/", "runtime root containing /var/lib/katl")
	listen := flags.String("listen", agent.DefaultListen, "TCP listen address such as tcp://0.0.0.0:9443")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	fmt.Fprintf(stdout, "katlc agent serve listen=%s\n", *listen)
	return agent.Serve(ctx, agent.ServeConfig{Root: *root, Listen: *listen})
}
