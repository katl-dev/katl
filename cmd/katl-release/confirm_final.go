package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/katl-dev/katl/internal/releasestate"
)

func confirmFinal(ctx context.Context, args []string, r *reader) error {
	if len(args) != 3 {
		return errors.New("usage: scripts/katl-release confirm-final VERSION COMMIT DIGEST")
	}
	version, commit, digest := args[0], args[1], args[2]
	attempt, err := actionsAttempt()
	if err != nil {
		return err
	}
	target, err := attempt.run.URL()
	if err != nil {
		return err
	}
	d, err := decide(ctx, r, attempt.ref, attempt.commit, "")
	if err != nil {
		return err
	}
	if d.Action == releasestate.Refuse {
		return errors.New(d.Message)
	}
	if d.Action != releasestate.Final || d.Version != version || d.Commit != commit {
		return fmt.Errorf("the release decision changed to %s %s at %s. Dispatch the release action again.", d.Action, d.Version, d.Commit)
	}
	record := d.FinalRecord
	if record == nil || record.Phase != releasestate.Qualified || record.Digest != digest || record.URL != target {
		return fmt.Errorf("this attempt has no qualified record for %s with digest %s. Dispatch the release action again.", version, digest)
	}
	return nil
}
