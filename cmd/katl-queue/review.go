package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/katl-dev/katl/internal/reviewrecord"
)

const reviewUsage = "usage: katl-queue review post PR FILE"

type reviewTarget struct {
	state  string
	head   string
	viewer string
}

type reviewGitHub interface {
	reviewTarget(context.Context, int) (reviewTarget, error)
	mergeBase(context.Context, string, string) (string, error)
	comments(context.Context, int) ([]reviewrecord.Comment, error)
	comment(context.Context, int, string) error
}

func reviewCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(out, reviewUsage)
		return nil
	}
	if len(args) != 3 || args[0] != "post" {
		return errors.New(reviewUsage)
	}
	numbers, err := parseNumbers(args[1:2])
	if err != nil {
		return err
	}
	number := numbers[0]
	if !validPRNumber(number) {
		return errors.New("pull request number must be between 1 and 2147483647")
	}

	src, err := os.Open(args[2])
	if err != nil {
		return fmt.Errorf("open verdict file: %w", err)
	}
	defer src.Close()

	g, err := newGH(ctx)
	if err != nil {
		return err
	}
	return postReview(ctx, g, number, src, out)
}

func postReview(ctx context.Context, g reviewGitHub, number int, src io.Reader, out io.Writer) error {
	file, err := reviewrecord.Decode(src)
	if err != nil {
		return err
	}
	if file.PR() != number {
		return fmt.Errorf("verdict file names PR #%d; pass that PR or use a verdict for PR #%d", file.PR(), number)
	}
	record := file.Record()

	checkTarget := func(target reviewTarget) error {
		if target.state != "OPEN" {
			return fmt.Errorf("PR #%d is not open (state %s); use an open PR", number, target.state)
		}
		if record.Head != target.head {
			return fmt.Errorf("PR #%d is at %s; review its current head and update the verdict file", number, target.head)
		}
		return nil
	}
	target, err := g.reviewTarget(ctx, number)
	if err != nil {
		return err
	}
	if err := checkTarget(target); err != nil {
		return err
	}

	mergeBase, err := g.mergeBase(ctx, record.Base, record.Head)
	if err != nil {
		return fmt.Errorf("verify reviewed base %s against head %s; check both commit SHAs and repository history: %w", record.Base, record.Head, err)
	}
	if mergeBase != record.Base {
		return fmt.Errorf("reviewed base %s is not an ancestor of head %s; use the reviewed ancestor or review the correct diff", record.Base, record.Head)
	}
	comments, err := g.comments(ctx, number)
	if err != nil {
		return fmt.Errorf("read PR #%d comments: %w", number, err)
	}
	body := file.Comment()
	if reviewrecord.AlreadyPosted(comments, target.viewer, record.Head, body) {
		fmt.Fprintf(out, "Review already recorded on PR #%d (%s).\n", number, short(record.Head))
		return nil
	}

	// Ancestry checks and paging through comments can take long enough for
	// someone to close the PR or push. Recheck immediately before the write.
	target, err = g.reviewTarget(ctx, number)
	if err != nil {
		return err
	}
	if err := checkTarget(target); err != nil {
		return err
	}
	if err := g.comment(ctx, number, body); err != nil {
		return fmt.Errorf("post review on PR #%d: %w", number, err)
	}
	fmt.Fprintf(out, "Posted %s review on PR #%d (%s).\n", record.Verdict, number, short(record.Head))
	return nil
}
