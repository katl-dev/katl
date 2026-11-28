package main

import (
	"context"
	"fmt"
	"strings"
)

// patchID supplies reviewrecord selection with the stable identity of the
// PR diff. The merge base keeps target-branch changes outside that diff, and
// -U0 counts only changed lines, so a clean rebase over moved context keeps
// its identity.
func (s *scratchObjects) patchID(ctx context.Context, base, head string) (string, error) {
	diff, err := s.git(ctx, "diff", "--merge-base", "-U0", "--no-color", "--no-ext-diff", "--no-textconv",
		"--no-renames", "--no-relative", "--full-index", "--binary", base, head, "--")
	if err != nil {
		return "", err
	}
	out, err := s.gitWithInput(ctx, strings.NewReader(diff), "patch-id", "--stable")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return "", fmt.Errorf("expected one stable patch ID for %s to %s, got %q", base, head, out)
	}
	return fields[0], nil
}
