package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func issueFixtures(t *testing.T) []intakeIssue {
	t.Helper()
	raw, err := os.ReadFile("testdata/issue-intake.json")
	if err != nil {
		t.Fatal(err)
	}
	var issues []intakeIssue
	if err := json.Unmarshal(raw, &issues); err != nil {
		t.Fatal(err)
	}
	return issues
}

func TestIssueRanking(t *testing.T) {
	in := newIssueIntake(issueFixtures(t))
	for _, tc := range []struct {
		name   string
		draft  issueText
		self   int
		first  int
		strong bool
	}{
		{"duplicate above carrier loss", in.issues[862].issueText, 862, 805, true},
		{"body only", issueText{"Keep the diagnostic", "WaitLibvirtLease in internal/vmtest/libvirt_lease.go discards no DHCP lease when the virsh net-dhcp-leases call is killed. Report the MAC and elapsed timeout."}, 0, 805, true},
		{"related upgrade", in.issues[845].issueText, 845, 550, true},
		{"identical siblings", in.issues[664].issueText, 664, 663, true},
		{"supersession", in.issues[949].issueText, 949, 795, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := in.scorer.rank(tc.draft, tc.self)[0]
			if got.Number != tc.first || got.strong() != tc.strong || got.Score < issueMatchFloor {
				t.Errorf("highest match = %+v, want #%d strong=%t", got, tc.first, tc.strong)
			}
		})
	}
}

type issueMemory struct {
	issues   []intakeIssue
	created  []issueDraft
	comments map[int][]string
	labels   []string
	reads    int
}

func (g *issueMemory) intakeIssues(context.Context) ([]intakeIssue, error) {
	g.reads++
	return g.issues, nil
}

func (g *issueMemory) issueLabels(context.Context) ([]string, error) { return g.labels, nil }

func (g *issueMemory) addRecurrenceLabel(context.Context) error {
	g.labels = append(g.labels, recurrenceLabel)
	return nil
}

func (g *issueMemory) createIntakeIssue(_ context.Context, draft issueDraft) (intakeIssue, error) {
	g.created = append(g.created, draft)
	i := intakeIssue{Number: 999 + len(g.created), issueText: draft.issueText, Labels: draft.labels()}
	g.issues = append(g.issues, i)
	return i, nil
}

func (g *issueMemory) comment(_ context.Context, number int, body string) error {
	if g.comments == nil {
		g.comments = map[int][]string{}
	}
	g.comments[number] = append(g.comments[number], body)
	return nil
}

func (g *issueMemory) addSubIssue(_ context.Context, parent, child int) error {
	for i := range g.issues {
		if g.issues[i].Number == child {
			g.issues[i].Parent = parent
			return nil
		}
	}
	return fmt.Errorf("missing issue #%d", child)
}

const criteria = "\n\n## Acceptance criteria\n\n- Verify the behavior in the affected workflow."

func issueFixtureDraft(text issueText, kind string) issueDraft {
	text.Body += criteria
	return issueDraft{issueText: text, Kind: kind, Area: "area: dev-tooling", Priority: "priority: p1"}
}

func newIssueMemory(t *testing.T) *issueMemory {
	return &issueMemory{
		issues: issueFixtures(t),
		labels: []string{"bug", "flaky-test", "enhancement", "area: dev-tooling", "priority: p1"},
	}
}

func TestIssueIntakeDecisions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		number      int
		options     intakeOptions
		wantErr     string
		wantOut     string
		wantCreated bool
		commented   int
	}{
		{name: "identical", number: 805, wantErr: "identical to open issue #805"},
		{name: "strong", number: 862, wantErr: "strong open matches"},
		{name: "confirmation", number: 862, options: intakeOptions{distinct: []int{805}}, wantCreated: true},
		{name: "duplicate comment", number: 862, options: intakeOptions{duplicate: 805}, commented: 805, wantOut: "Commented on #805."},
		{name: "supersession", number: 862, options: intakeOptions{supersedes: 805}, wantCreated: true, commented: 805},
		{name: "family", number: 664, options: intakeOptions{parent: 618}, wantCreated: true, wantOut: "Family #663"},
		{name: "weak", number: 795, wantCreated: true, wantOut: "#949"},
		{name: "below floor", wantCreated: true, wantOut: "No matches above the reporting floor."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newIssueMemory(t)
			in := newIssueIntake(g.issues)
			opt := tc.options
			opt.draft = issueFixtureDraft(in.issues[tc.number].issueText, "enhancement")
			switch tc.name {
			case "identical":
				opt.draft.issueText = in.issues[805].issueText
			case "strong", "confirmation", "duplicate comment", "supersession":
				g.issues = slices.DeleteFunc(g.issues, func(i intakeIssue) bool { return i.Number == 862 })
				if tc.name == "supersession" {
					opt.draft.Kind = "bug"
					for idx := range g.issues {
						if g.issues[idx].Number == 805 {
							g.issues[idx].Labels = []string{"bug"}
						}
					}
				}
			case "weak":
				g.issues = slices.DeleteFunc(g.issues, func(i intakeIssue) bool { return i.Number == tc.number })
			case "below floor":
				opt.draft = issueFixtureDraft(issueText{"Desktop icon alignment", "Space the toolbar glyphs evenly on small displays."}, "enhancement")
			}
			var out strings.Builder
			err := fileIssue(t.Context(), g, opt, &out)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("file issue = %v, want %q; output %s", err, tc.wantErr, &out)
			}
			if (len(g.created) == 1) != tc.wantCreated || (len(g.comments[tc.commented]) == 1) != (tc.commented != 0) ||
				!strings.Contains(out.String(), tc.wantOut) || g.reads != 1 {
				t.Fatalf("created %d, comments %v, reads %d, output %s", len(g.created), g.comments, g.reads, &out)
			}
			if tc.options.supersedes != 0 {
				if !strings.Contains(g.created[0].Body, "Supersedes: #805") || g.comments[805][0] != "Superseded by #1000." {
					t.Fatalf("supersession links = %v, %v", g.created, g.comments)
				}
			}
			if tc.options.parent != 0 && g.issues[len(g.issues)-1].Parent != tc.options.parent {
				t.Error("issue was not added as a GitHub sub-issue")
			}
		})
	}
}

func TestIssueRecurrenceAudit(t *testing.T) {
	g := newIssueMemory(t)
	opt := intakeOptions{
		draft: issueFixtureDraft(issueText{
			"bootstrap: resolve GitHub repository identity once",
			"Bootstrap resolves repository identity independently. A fixed repository and a remote lookup disagree outside the checkout. Bind one repository once and reuse the selection for every request.",
		}, "bug"),
	}
	var out strings.Builder
	if err := fileIssue(t.Context(), g, opt, &out); err == nil || !strings.Contains(err.Error(), "require --recurrence-of or --unrelated") {
		t.Fatalf("unclassified recurrence = %v", err)
	}
	for _, want := range []string{"#994", "#995", "Recurrence"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s from %s", want, &out)
		}
	}
	if len(g.created) != 0 {
		t.Fatal("created an issue before recurrence confirmation")
	}

	draft := intakeIssue{Number: 1000, issueText: opt.draft.issueText, Labels: opt.draft.labels()}
	var report strings.Builder
	reportIssues(newIssueIntake(append(slices.Clone(g.issues), draft)), &report)
	if !strings.Contains(report.String(), "Unaudited recurrence cluster #994, #995, #1000") || strings.Contains(report.String(), "#996") {
		t.Fatalf("report disagrees with the intake recurrence matches: %s", &report)
	}
	if !strings.Contains(report.String(), "Supersession #949 supersedes #795") {
		t.Fatalf("report missed a recorded supersession: %s", &report)
	}

	opt.recurrence = []int{994, 995}
	opt.failureClass = "two owners of GitHub repository identity"
	out.Reset()
	if err := fileIssue(t.Context(), g, opt, &out); err != nil {
		t.Fatalf("file recurrence: %v\n%s", err, &out)
	}
	if len(g.created) != 2 || !slices.Contains(g.created[0].labels(), recurrenceLabel) {
		t.Fatalf("created %+v", g.created)
	}
	audit := g.created[1]
	if audit.Title != "audit: two owners of GitHub repository identity" || audit.Area != opt.draft.Area ||
		audit.Priority != opt.draft.Priority || !strings.Contains(audit.Body, "Audits: #994, #995, #1000") {
		t.Fatalf("audit = %+v", audit)
	}
	report.Reset()
	reportIssues(newIssueIntake(g.issues), &report)
	if strings.Contains(report.String(), "Unaudited recurrence cluster") {
		t.Fatalf("report still flags audited cluster: %s", &report)
	}
	manual := slices.Clone(g.issues)
	manual[len(manual)-1].Body = "## Acceptance criteria\n- Verify ownership.\n\nAudits: #994, #995, #1000"
	report.Reset()
	reportIssues(newIssueIntake(manual), &report)
	if strings.Contains(report.String(), "Unaudited recurrence cluster") {
		t.Fatalf("report missed the audit membership: %s", &report)
	}

	opt.draft.Title = "publisher: bind repository identity for dependency lookups"
	opt.distinct = []int{1000}
	opt.recurrence = []int{994, 995, 1000}
	if err := fileIssue(t.Context(), g, opt, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(g.created) != 3 || !strings.Contains(g.created[2].Body, "Recurrence audit: #1001") ||
		len(g.comments[1001]) != 1 {
		t.Fatalf("did not attach to existing audit: created %+v, comments %v", g.created, g.comments)
	}
}

func TestIssueFamilyDoesNotTriggerRecurrence(t *testing.T) {
	g := newIssueMemory(t)
	in := newIssueIntake(g.issues)
	for _, n := range []int{994, 995} {
		for idx := range g.issues {
			if g.issues[idx].Number == n {
				g.issues[idx].Parent = 618
			}
		}
	}
	opt := intakeOptions{parent: 618, draft: issueFixtureDraft(in.issues[994].issueText, "bug")}
	var out strings.Builder
	if err := fileIssue(t.Context(), g, opt, &out); err != nil {
		t.Fatal(err)
	}
	if len(g.created) != 1 || !strings.Contains(out.String(), "Family #994") || strings.Contains(out.String(), "Recurrence") {
		t.Fatalf("family did not bypass recurrence: %s", &out)
	}
	out.Reset()
	reportIssues(newIssueIntake(g.issues), &out)
	if strings.Contains(out.String(), "Unaudited recurrence cluster") {
		t.Fatalf("family triggered a batch audit: %s", &out)
	}
}

func TestIssueUnrelated(t *testing.T) {
	g := newIssueMemory(t)
	draft := issueFixtureDraft(issueText{
		"bootstrap: resolve GitHub repository identity once",
		"Bootstrap resolves repository identity independently. A fixed repository and a remote lookup disagree outside the checkout. Bind one repository once and reuse the selection for every request.",
	}, "bug")
	opt := intakeOptions{draft: draft, alone: []int{994, 995}, reason: "The incorrect remote is selected by the bootstrap configuration."}
	if err := fileIssue(t.Context(), g, opt, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(g.created) != 1 || g.created[0].Recurrence ||
		!strings.Contains(g.created[0].Body, "Unrelated: #994, #995 — "+opt.reason) {
		t.Fatalf("unrelated decision not recorded on a single bug: %+v", g.created)
	}
}

func TestIssueIntakeInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issue.md")
	if err := os.WriteFile(path, []byte("Issue description."+criteria), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{"kind", "area", "priority"} {
		t.Run(missing, func(t *testing.T) {
			args := []string{"--title", "Issue", "--body-file", path}
			for _, label := range []string{"kind", "area", "priority"} {
				if label != missing {
					args = append(args, "--"+label, label)
				}
			}
			if _, err := parseIntake(args, io.Discard); err == nil || !strings.Contains(err.Error(), "--"+missing) {
				t.Fatalf("missing %s: %v", missing, err)
			}
		})
	}
	for _, body := range []string{"Issue without criteria", "## Acceptance criteria\n\n## Design\n- build", "```markdown\n## Acceptance criteria\n- example\n```"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		args := []string{"--title", "Issue", "--body-file", path, "--kind", "bug", "--area", "ci", "--priority", "p1"}
		if _, err := parseIntake(args, io.Discard); err == nil || !strings.Contains(err.Error(), "Acceptance criteria") {
			t.Fatalf("body %q: %v", body, err)
		}
	}
}
