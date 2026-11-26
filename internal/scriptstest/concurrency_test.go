package scriptstest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Pull request runs supersede older runs of the same pull request. Every other
// trigger must keep running to completion, so two candidate dispatches or
// pushes on main both finish, and merge-group runs are distinct per queue ref.
func TestWorkflowConcurrency(t *testing.T) {
	type event map[string]string
	pr := func(number, runID string) event {
		return event{"github.event_name": "pull_request", "github.event.pull_request.number": number, "github.ref": "refs/pull/" + number + "/merge", "github.run_id": runID}
	}
	other := func(name, ref, runID string) event {
		return event{"github.event_name": name, "github.ref": ref, "github.run_id": runID}
	}

	for _, file := range []string{"ci.yml", "hosted-vmtests.yml"} {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows", file))
			if err != nil {
				t.Fatal(err)
			}
			var workflow struct {
				Concurrency struct {
					Group            string
					CancelInProgress string `yaml:"cancel-in-progress"`
				}
			}
			if err := yaml.Unmarshal(data, &workflow); err != nil {
				t.Fatal(err)
			}
			group := func(e event) string {
				e["github.workflow"] = "wf"
				return expandExpressions(t, workflow.Concurrency.Group, e)
			}
			cancels := func(e event) bool {
				return expandExpressions(t, workflow.Concurrency.CancelInProgress, e) == "true"
			}

			if a, b := group(pr("7", "1")), group(pr("7", "2")); a != b {
				t.Errorf("runs of one pull request use groups %q and %q, want one group", a, b)
			}
			if !cancels(pr("7", "2")) {
				t.Error("a newer pull request run does not cancel the older run")
			}
			if a, b := group(pr("7", "1")), group(pr("8", "2")); a == b {
				t.Errorf("different pull requests share group %q", a)
			}

			for _, name := range []string{"push", "workflow_dispatch", "workflow_call"} {
				a, b := other(name, "refs/heads/main", "1"), other(name, "refs/heads/main", "2")
				if ga, gb := group(a), group(b); ga == gb {
					t.Errorf("%s runs on main share group %q and cancel or queue each other", name, ga)
				}
				if cancels(a) {
					t.Errorf("%s run cancels other runs in its group", name)
				}
			}

			a, b := other("merge_group", "refs/heads/gh-readonly-queue/main/pr-1", "1"), other("merge_group", "refs/heads/gh-readonly-queue/main/pr-2", "2")
			if ga, gb := group(a), group(b); ga == gb {
				t.Errorf("distinct merge-group refs share group %q", ga)
			}
			if cancels(a) {
				t.Error("merge-group run cancels other runs")
			}
		})
	}
}

var expressionPattern = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

// expandExpressions substitutes each ${{ }} in text with its value under the
// given context. Values are strings or booleans; empty strings and false are
// falsy, and || and && return an operand as GitHub Actions does.
func expandExpressions(t *testing.T, text string, ctx map[string]string) string {
	t.Helper()
	return expressionPattern.ReplaceAllStringFunc(text, func(match string) string {
		p := &expressionParser{src: strings.TrimSpace(expressionPattern.FindStringSubmatch(match)[1]), ctx: ctx}
		v := p.or()
		if p.skip(); p.pos != len(p.src) {
			t.Fatalf("unsupported expression %q at %d", p.src, p.pos)
		}
		return fmt.Sprint(v)
	})
}

type expressionParser struct {
	src string
	pos int
	ctx map[string]string
}

func (p *expressionParser) skip() {
	for p.pos < len(p.src) && p.src[p.pos] == ' ' {
		p.pos++
	}
}

func (p *expressionParser) take(op string) bool {
	p.skip()
	if strings.HasPrefix(p.src[p.pos:], op) {
		p.pos += len(op)
		return true
	}
	return false
}

func truthy(v any) bool { return v != "" && v != false }

func (p *expressionParser) or() any {
	v := p.and()
	for p.take("||") {
		if r := p.and(); !truthy(v) {
			v = r
		}
	}
	return v
}

func (p *expressionParser) and() any {
	v := p.equal()
	for p.take("&&") {
		if r := p.equal(); truthy(v) {
			v = r
		}
	}
	return v
}

func (p *expressionParser) equal() any {
	v := p.primary()
	if p.take("!=") {
		return v != p.primary()
	}
	if p.take("==") {
		return v == p.primary()
	}
	return v
}

func (p *expressionParser) primary() any {
	if p.take("(") {
		v := p.or()
		p.take(")")
		return v
	}
	p.skip()
	if p.take("'") {
		end := strings.IndexByte(p.src[p.pos:], '\'')
		v := p.src[p.pos : p.pos+end]
		p.pos += end + 1
		return v
	}
	start := p.pos
	for p.pos < len(p.src) && (p.src[p.pos] == '.' || p.src[p.pos] == '_' || p.src[p.pos] == '-' || p.src[p.pos] >= 'a' && p.src[p.pos] <= 'z' || p.src[p.pos] >= '0' && p.src[p.pos] <= '9') {
		p.pos++
	}
	name := p.src[start:p.pos]
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		return name
	}
	return p.ctx[name]
}
