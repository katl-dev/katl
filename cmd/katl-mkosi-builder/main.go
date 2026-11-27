// Command katl-mkosi-builder proposes the committed mkosi builder pin.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/katl-dev/katl/internal/automationpr"
)

//go:embed update-commit.txt
var updateCommit string

// lockPath is the pin scripts/mkosi-builder reads; it also owns the format.
const lockPath = "mkosi-builder.lock"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "katl-mkosi-builder:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "propose" {
		return errors.New("usage: katl-mkosi-builder propose --lock FILE [--repository OWNER/NAME] [--base SHA]")
	}
	return propose(ctx, args[1:], stdout, stderr)
}

// propose opens or advances the branch that commits a new pin. The lock is
// compared as bytes: scripts/mkosi-builder writes it, and any difference is a
// different pin.
func propose(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-mkosi-builder propose", flag.ContinueOnError)
	flags.SetOutput(stderr)
	lock := flags.String("lock", "", "lock written by scripts/mkosi-builder lock")
	repository := flags.String("repository", os.Getenv("GITHUB_REPOSITORY"), "GitHub repository")
	base := flags.String("base", os.Getenv("GITHUB_SHA"), "source commit the image was published from")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *lock == "" || *repository == "" || *base == "" {
		return errors.New("propose requires --lock, --repository and --base")
	}
	content, err := os.ReadFile(*lock)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return fmt.Errorf("%s is empty", *lock)
	}
	committed, err := os.ReadFile(lockPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && bytes.Equal(committed, content) {
		return json.NewEncoder(stdout).Encode(map[string]any{"changed": false})
	}

	identity := sha256.Sum256(content)
	branch := "automation/mkosi-builder-" + hex.EncodeToString(identity[:8])
	commit, err := automationpr.Publish(ctx, automationpr.Proposal{
		Repository: *repository,
		Base:       *base,
		Branch:     branch,
		Message:    updateCommit,
		Path:       lockPath,
		Content:    content,
		Matches: func(existing []byte) error {
			if !bytes.Equal(existing, content) {
				return errors.New("existing proposal pins a different builder; preserve it for inspection")
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"changed": true, "branch": branch, "commit": commit})
}
