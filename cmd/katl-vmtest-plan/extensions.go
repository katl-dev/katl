package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/katl-dev/katl/internal/extensionrevision"
)

// extensionAction is the planner's answer for one extension definition.
type extensionAction struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	extensionrevision.Decision
}

func (action extensionAction) rejects() bool {
	// Unproven rejects until the gate can prove that the bytes are unchanged.
	return action.Answer == extensionrevision.Invalid || action.Answer == extensionrevision.Unproven
}

func runExtensions(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-vmtest-plan extensions", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baseRef := flags.String("base-ref", "", "branch the change is applied to")
	base := flags.String("base", "", "revision the change is applied to")
	head := flags.String("head", "HEAD", "candidate revision")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *baseRef == "" || *base == "" {
		return errors.New("usage: katl-vmtest-plan extensions --base-ref BRANCH --base REVISION [--head REVISION]")
	}

	actions, err := planExtensions(".", *baseRef, *base, *head)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(struct {
		Extensions []extensionAction `json:"extensions"`
	}{actions}); err != nil {
		return err
	}
	var rejected bool
	for _, action := range actions {
		fmt.Fprintf(stderr, "extension-revision name=%s version=%q decision=%s reason=%q\n", action.Name, action.Version, action.Answer, action.Reason)
		rejected = rejected || action.rejects()
	}
	if rejected {
		return errors.New("extension revision rejected")
	}
	return nil
}

// planExtensions returns one action per extension defined at base or head.
// The revision decision applies only to changes into main; any other base
// gets no extension action.
func planExtensions(repo, baseRef, base, head string) ([]extensionAction, error) {
	actions := []extensionAction{}
	if baseRef != "main" {
		return actions, nil
	}
	names, err := extensionrevision.Names(repo, base, head)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		before, err := extensionrevision.Read(repo, base, name)
		if err != nil {
			return nil, err
		}
		after, err := extensionrevision.Read(repo, head, name)
		if err != nil {
			return nil, err
		}
		actions = append(actions, extensionAction{Name: name, Version: after.Version, Decision: extensionrevision.Decide(before, after)})
	}
	return actions, nil
}
