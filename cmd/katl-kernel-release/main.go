package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/katl-dev/katl/internal/kernelrelease"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "katl-kernel-release:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("command required: candidate, prepare, publish, or verify")
	}
	if args[0] == "candidate" {
		return candidate(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "propose" {
		return propose(ctx, args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("katl-kernel-release "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	lockPath := flags.String("lock", "", "reviewed kernel lock")
	layout := flags.String("layout", "", "optional local retained OCI layout")
	cache := flags.String("cache", "_build/kernel-rpms", "verified RPM cache")
	profile := flags.String("profile", "runtime", "mkosi profile")
	config := flags.String("config", "", "write native mkosi configuration")
	visible := flags.String("visible-directory", "", "RPM cache directory as seen by mkosi")
	flavour := flags.String("flavour", "", "expected build flavour")
	fedora := flags.String("fedora-release", "", "expected Fedora release")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *lockPath == "" {
		return fmt.Errorf("--lock is required and positional arguments are unsupported")
	}
	lock, err := kernelrelease.Read(*lockPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "publish":
		if *layout == "" {
			return fmt.Errorf("publish requires --layout")
		}
		return kernelrelease.Publish(ctx, lock, *layout)
	case "verify":
		_, err := kernelrelease.Prepare(ctx, lock, *cache, *layout)
		return err
	case "prepare":
		if *flavour != lock.Flavour || *fedora != lock.FedoraRelease {
			return fmt.Errorf("kernel lock does not match the selected Fedora release and flavour")
		}
		directory, err := kernelrelease.Prepare(ctx, lock, *cache, *layout)
		if err != nil {
			return err
		}
		identity, err := lock.Digest()
		if err != nil {
			return err
		}
		if *config != "" {
			path, err := filepath.Abs(directory)
			if err != nil {
				return err
			}
			if *visible != "" {
				path = filepath.Join(*visible, identity)
			}
			contents, err := lock.Config(*profile, path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(*config), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(*config, []byte(contents), 0o644); err != nil {
				return err
			}
		}
		return json.NewEncoder(stdout).Encode(struct {
			Directory     string `json:"directory"`
			Identity      string `json:"identity"`
			KernelRelease string `json:"kernelRelease"`
		}{directory, identity, lock.KernelRelease})
	default:
		return fmt.Errorf("unsupported command %q", args[0])
	}
}
