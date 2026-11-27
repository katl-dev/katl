package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/katl-dev/katl/internal/automationpr"
	"github.com/katl-dev/katl/internal/kernelrelease"
)

//go:embed update-commit.txt
var updateCommit string

func propose(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-kernel-release propose", flag.ContinueOnError)
	flags.SetOutput(stderr)
	candidates := flags.String("candidates", "_build/kernel-candidates", "captured kernel inputs by flavour")
	flavour := flags.String("flavour", "", "kernel flavour: standard or lts")
	repository := flags.String("repository", os.Getenv("GITHUB_REPOSITORY"), "GitHub repository")
	base := flags.String("base", os.Getenv("GITHUB_SHA"), "captured source commit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *repository == "" || *base == "" {
		return fmt.Errorf("propose requires --repository and --base")
	}
	if _, err := kernelrelease.Names(*flavour); err != nil {
		return err
	}
	path := filepath.Join(*candidates, *flavour, "lock.json")
	lock, err := kernelrelease.Read(path)
	if err != nil {
		return err
	}
	if lock.Flavour != *flavour {
		return fmt.Errorf("candidate must match the selected kernel flavour")
	}
	identity, err := lock.Digest()
	if err != nil {
		return err
	}
	destination := filepath.Join("kernels", "fedora-"+lock.FedoraRelease, lock.Architecture, *flavour+".json")
	previous, err := kernelrelease.Read(destination)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		old, err := previous.Digest()
		if err != nil {
			return err
		}
		if old == identity {
			return json.NewEncoder(stdout).Encode(map[string]any{"changed": false})
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	branch := "automation/kernels-" + *flavour + "-" + identity[:16]
	commit, err := automationpr.Publish(ctx, automationpr.Proposal{
		Repository: *repository,
		Base:       *base,
		Branch:     branch,
		Message:    fmt.Sprintf(updateCommit, *flavour),
		Path:       destination,
		Content:    data,
		Matches: func(existing []byte) error {
			var actual kernelrelease.Lock
			if err := json.Unmarshal(existing, &actual); err != nil {
				return err
			}
			actualID, err := actual.Digest()
			if err != nil {
				return err
			}
			if actualID != identity || actual.Reference != lock.Reference {
				return fmt.Errorf("existing proposal has different kernel inputs; preserve it for inspection")
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"changed": true, "branch": branch, "commit": commit})
}
