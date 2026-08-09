package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/katlc/agent"
	"github.com/katl-dev/katl/internal/katlc/firewall"
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
	return fmt.Errorf("unsupported command %q", strings.Join(args, " "))
}

func helpText() string {
	return `Usage: katlc <command> [args]

Commands:
  version                 Print build version metadata.
  agent serve             Run the KatlOS node management agent.
  agent firewall          Restrict management ingress to boot-time host interfaces.
  kubeadm plan            Compare selected desired kubeadm input with read-only live state.

`
}

func runAgent(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("agent command is required")
	}
	switch args[0] {
	case "serve":
		return runAgentServe(ctx, args[1:], stdout, stderr)
	case "firewall":
		return runAgentFirewall(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unsupported agent command %q", args[0])
	}
}

func runAgentFirewall(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katlc agent firewall", flag.ContinueOnError)
	flags.SetOutput(stderr)
	port := flags.Uint("port", 9443, "management TCP port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *port == 0 || *port > 65535 {
		return fmt.Errorf("--port must be between 1 and %s", strconv.Itoa(65535))
	}
	if err := firewall.Ensure(ctx, firewall.Config{Port: uint16(*port)}); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "katlc agent firewall port=%d status=ready\n", *port)
	return err
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
