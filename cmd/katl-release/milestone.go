package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/releaseversion"
)

const mergeMilestoneUsage = "scripts/katl-release merge-milestone [--apply] VERSION FOLLOWING_VERSION"

type versionMilestone struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Description string `json:"description"`
	URL         string `json:"html_url"`
}

type milestoneItem struct {
	Number      int       `json:"number"`
	State       string    `json:"state"`
	Title       string    `json:"title"`
	PullRequest *struct{} `json:"pull_request"`
}

type milestoneMerge struct {
	source, target versionMilestone
	items          []milestoneItem
	plan           int
	comments       string
	pendingComment string
}

func mergeMilestone(ctx context.Context, args []string, stdout, stderr io.Writer, r *reader, call func(context.Context, string, ...string) ([]byte, error)) error {
	flags := flag.NewFlagSet("merge-milestone", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, "usage:", mergeMilestoneUsage) }
	apply := flags.Bool("apply", false, "move open work and close the source milestone")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: " + mergeMilestoneUsage)
	}

	versions := make([]releaseversion.Version, 2)
	for i, arg := range flags.Args() {
		v, err := validateVersion(arg)
		if err != nil {
			return err
		}
		if !v.IsFinal() {
			return fmt.Errorf("%s must be a final version", arg)
		}
		versions[i] = v
	}
	from, to := versions[0], versions[1]
	if compareFinal(to, from) <= 0 {
		return fmt.Errorf("following version %s must be later than %s", to, from)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	published, _, err := r.releasesForTag(ctx, from.Tag())
	if err != nil {
		return err
	}
	if published != nil {
		return fmt.Errorf("%s is already published at %s", from.Tag(), published.HTMLURL)
	}

	source, target, err := r.milestonesForMerge(ctx, from.Tag(), to.Tag())
	if err != nil {
		return err
	}
	planIssue, err := r.milestonePlanIssue(ctx, source)
	if err != nil {
		return err
	}
	plan := planIssue.Number
	comments := r.repo(fmt.Sprintf("issues/%d/comments", plan))
	prefix := fmt.Sprintf("Milestone %s was merged forward into ", source.Title)
	comment := fmt.Sprintf("%s[%s](%s). Follow that milestone for the remaining work.", prefix, target.Title, target.URL)
	var commented, differentTarget bool
	if err := pages(ctx, r, comments+"?per_page=100", func(page []struct{ Body string }) {
		for _, c := range page {
			commented = commented || c.Body == comment
			differentTarget = differentTarget || (strings.HasPrefix(c.Body, prefix) && c.Body != comment)
		}
	}); err != nil {
		return err
	}
	if differentTarget {
		return fmt.Errorf("%s already records a merge into a different milestone on plan issue #%d", source.Title, plan)
	}
	if source.State == "closed" {
		if !commented {
			return fmt.Errorf("%s is already closed without a record of this merge on plan issue #%d", source.Title, plan)
		}
		fmt.Fprintf(stdout, "%s is already merged into %s (plan issue #%d).\n", source.Title, target.Title, plan)
		return nil
	}
	// The abandoned version's own release issues must not move into the
	// following milestone, so the merge waits until they are closed.
	if planIssue.State != "closed" {
		return fmt.Errorf("%s still has an open release issue: issue #%d %q; close each as not planned, then rerun", source.Title, plan, planIssue.Title)
	}

	items, err := r.openMilestoneItems(ctx, source.Number)
	if err != nil {
		return err
	}
	if !*apply {
		for _, item := range items {
			fmt.Fprintf(stdout, "Would move %s to %s.\n", item, target.Title)
		}
		if !commented {
			fmt.Fprintf(stdout, "Would comment on plan issue #%d.\n", plan)
		}
		fmt.Fprintf(stdout, "Would close %s.\n", source.Title)
		return nil
	}

	merge := milestoneMerge{source: source, target: target, items: items, plan: plan, comments: comments}
	if !commented {
		merge.pendingComment = comment
	}
	if err := merge.apply(ctx, stdout, r, call); err != nil {
		return fmt.Errorf("%w\nRun scripts/katl-release merge-milestone --apply %s %s to resume", err, from, to)
	}
	return nil
}

func (r *reader) milestonesForMerge(ctx context.Context, from, to string) (source, target versionMilestone, err error) {
	var sources, targets []versionMilestone
	err = pages(ctx, r, r.repo("milestones?state=all&per_page=100"), func(page []versionMilestone) {
		for _, m := range page {
			switch m.Title {
			case from:
				sources = append(sources, m)
			case to:
				targets = append(targets, m)
			}
		}
	})
	if err != nil {
		return source, target, err
	}
	if len(sources) != 1 || sources[0].Number <= 0 {
		return source, target, fmt.Errorf("expected one source milestone named %s", from)
	}
	if len(targets) != 1 || targets[0].Number <= 0 || targets[0].URL == "" {
		return source, target, fmt.Errorf("expected one following milestone named %s; create it before merging", to)
	}
	source, target = sources[0], targets[0]
	if source.State != "open" && source.State != "closed" {
		return source, target, fmt.Errorf("GitHub returned an unknown state for %s", from)
	}
	if source.State == "open" && target.State != "open" {
		return source, target, fmt.Errorf("following milestone %s must be open", to)
	}
	return source, target, nil
}

func (r *reader) milestonePlanIssue(ctx context.Context, source versionMilestone) (milestoneItem, error) {
	// The source description carries the plan's identity.
	prefix := r.server + "/" + r.repository + "/issues/"
	link := regexp.MustCompile(regexp.QuoteMeta(prefix) + `([1-9][0-9]*)(?:[[:space:])>.,]|$)`)
	var plan milestoneItem
	for _, match := range link.FindAllStringSubmatch(source.Description, -1) {
		number, err := strconv.Atoi(match[1])
		if err != nil {
			return plan, err
		}
		if number == plan.Number {
			continue
		}
		var issue milestoneItem
		if err := r.decode(ctx, r.repo(fmt.Sprintf("issues/%d", number)), &issue); err != nil {
			return plan, err
		}
		if issue.PullRequest != nil || issue.Title != "plan: "+source.Title+" scope and priorities" {
			continue
		}
		if plan.Number != 0 {
			return plan, fmt.Errorf("%s links multiple plan issues", source.Title)
		}
		issue.Number = number
		plan = issue
	}
	if plan.Number == 0 {
		return plan, fmt.Errorf("link the plan issue titled %q in the %s milestone description", "plan: "+source.Title+" scope and priorities", source.Title)
	}
	return plan, nil
}

func (i milestoneItem) String() string {
	if i.PullRequest != nil {
		return fmt.Sprintf("PR #%d", i.Number)
	}
	return fmt.Sprintf("issue #%d", i.Number)
}

func (r *reader) openMilestoneItems(ctx context.Context, number int) ([]milestoneItem, error) {
	var items []milestoneItem
	err := pages(ctx, r, r.repo(fmt.Sprintf("issues?milestone=%d&state=open&per_page=100", number)), func(page []milestoneItem) {
		for _, item := range page {
			if item.State == "open" {
				items = append(items, item)
			}
		}
	})
	return items, err
}

func (m milestoneMerge) apply(ctx context.Context, stdout io.Writer, r *reader, call func(context.Context, string, ...string) ([]byte, error)) error {
	// Read all pages before moving anything, because each move changes the
	// pagination of the source. Send only the milestone field. Setting the
	// same milestone again is idempotent, so a move is retried like a read.
	for _, item := range m.items {
		err := r.retry(ctx, func(ctx context.Context) error {
			_, err := writeGitHub(ctx, call, "PATCH", r.repo(fmt.Sprintf("issues/%d", item.Number)), "-F", fmt.Sprintf("milestone=%d", m.target.Number))
			return err
		})
		if err != nil {
			return fmt.Errorf("move %s: %w", item, err)
		}
		fmt.Fprintf(stdout, "Moved %s to %s.\n", item, m.target.Title)
	}
	// GitHub can still list an item it has just moved. An item this merge
	// did not move is refused at once.
	moved := map[int]bool{}
	for _, item := range m.items {
		moved[item.Number] = true
	}
	remaining, err := awaitReadback(ctx, r, func(ctx context.Context) ([]milestoneItem, bool, error) {
		remaining, err := r.openMilestoneItems(ctx, m.source.Number)
		if err != nil {
			return remaining, false, err
		}
		for _, item := range remaining {
			if !moved[item.Number] {
				return remaining, false, fmt.Errorf("%s still has %d open items, including %s", m.source.Title, len(remaining), item)
			}
		}
		return remaining, len(remaining) == 0, nil
	}, nil)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("%s still lists %d moved items as open after %s", m.source.Title, len(remaining), readbackWait)
	}
	if err != nil {
		return err
	}
	if m.pendingComment != "" {
		// An unacknowledged POST may have succeeded. A fresh invocation
		// reads the plan's comments before making another attempt.
		if _, err := writeGitHub(ctx, call, "POST", m.comments, "-f", "body="+m.pendingComment); err != nil {
			return fmt.Errorf("comment on plan issue #%d: %w", m.plan, err)
		}
		fmt.Fprintf(stdout, "Commented on plan issue #%d.\n", m.plan)
	}
	if _, err := writeGitHub(ctx, call, "PATCH", r.repo(fmt.Sprintf("milestones/%d", m.source.Number)), "-f", "state=closed"); err != nil {
		return fmt.Errorf("close %s: %w", m.source.Title, err)
	}
	fmt.Fprintf(stdout, "Closed %s.\n", m.source.Title)
	return nil
}
