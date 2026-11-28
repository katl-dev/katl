package scriptstest

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

var expressionPattern = regexp.MustCompile(`(?s)\$\{\{(.*?)\}\}`)

// expandExpressions substitutes each ${{ }} in text with its value under the
// given context. Values are strings or booleans; empty strings and false are
// falsy, and || and && return an operand as GitHub Actions does. > compares
// operands as integers, with an empty value as zero, and always() is true.
func expandExpressions(t *testing.T, text string, ctx map[string]string) string {
	t.Helper()
	return expressionPattern.ReplaceAllStringFunc(text, func(match string) string {
		v := evaluateExpression(t, expressionPattern.FindStringSubmatch(match)[1], ctx)
		switch v.(type) {
		case nil:
			return ""
		case []any, map[string]any:
			data, _ := json.Marshal(v)
			return string(data)
		default:
			return fmt.Sprint(v)
		}
	})
}

func evaluateExpression(t *testing.T, source string, ctx map[string]string) any {
	t.Helper()
	p := &expressionParser{src: strings.TrimSpace(source), ctx: ctx}
	value := p.or()
	if p.skip(); p.pos != len(p.src) {
		t.Fatalf("%sunsupported expression %q at %d", ctx["expression-location"], p.src, p.pos)
	}
	return value
}

type expressionParser struct {
	src string
	pos int
	ctx map[string]string
}

func (p *expressionParser) skip() {
	for p.pos < len(p.src) && strings.ContainsRune(" \t\n\r", rune(p.src[p.pos])) {
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

func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0 && !math.IsNaN(v)
	case string:
		return v != ""
	default:
		return true
	}
}

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
	switch {
	case p.take("!="):
		return !equalExpressionValues(v, p.primary())
	case p.take("=="):
		return equalExpressionValues(v, p.primary())
	case p.take(">="):
		return expressionNumber(v) >= expressionNumber(p.primary())
	case p.take("<="):
		return expressionNumber(v) <= expressionNumber(p.primary())
	case p.take(">"):
		return expressionNumber(v) > expressionNumber(p.primary())
	case p.take("<"):
		return expressionNumber(v) < expressionNumber(p.primary())
	}
	return v
}

func equalExpressionValues(left, right any) bool {
	a, aString := left.(string)
	b, bString := right.(string)
	if aString && bString {
		return strings.EqualFold(a, b)
	}
	return expressionNumber(left) == expressionNumber(right)
}

func expressionNumber(value any) float64 {
	switch value {
	case true:
		return 1
	case false, "", nil:
		return 0
	}
	number, err := strconv.ParseFloat(fmt.Sprint(value), 64)
	if err != nil {
		return math.NaN()
	}
	return number
}

func expressionNameByte(b byte) bool {
	return b == '.' || b == '_' || b == '-' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func (p *expressionParser) primary() any {
	if p.take("(") {
		v := p.or()
		p.take(")")
		return v
	}
	if p.take("!") {
		return !truthy(p.primary())
	}
	if p.take("'") {
		var value strings.Builder
		for p.pos < len(p.src) {
			ch := p.src[p.pos]
			p.pos++
			if ch == '\'' {
				if p.pos == len(p.src) || p.src[p.pos] != '\'' {
					return value.String()
				}
				p.pos++
			}
			value.WriteByte(ch)
		}
		return value.String()
	}

	start := p.pos
	for p.pos < len(p.src) && expressionNameByte(p.src[p.pos]) {
		p.pos++
	}
	name := strings.ToLower(p.src[start:p.pos])
	if p.take("(") {
		var args []any
		if !p.take(")") {
			args = append(args, p.or())
			for p.take(",") {
				args = append(args, p.or())
			}
			p.take(")")
		}
		if value, ok := p.call(name, args); ok {
			for p.take("[") {
				key := fmt.Sprint(p.or())
				p.take("]")
				object, _ := value.(map[string]any)
				value = object[key]
			}
			return value
		}
		p.pos = start
		return nil
	}
	if name == "true" || name == "false" {
		return name == "true"
	}
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		return expressionNumber(name)
	}
	for p.take("[") {
		name += "." + strings.ToLower(fmt.Sprint(p.or()))
		p.take("]")
	}
	value, ok := p.ctx[name]
	if owner, _, output := strings.Cut(name, ".outputs."); output &&
		(p.ctx[owner+".outcome"] == "skipped" || p.ctx[owner+".result"] == "skipped") {
		return ""
	}
	if !ok && p.ctx["static-workflow"] != "" && (strings.Contains(name, ".outputs.") ||
		strings.HasPrefix(name, "secrets.") || strings.HasPrefix(name, "vars.")) {
		return "@" + name
	}
	switch p.ctx["type:"+name] {
	case "boolean":
		return value == "true"
	case "number":
		return expressionNumber(value)
	}
	return value
}

func (p *expressionParser) call(name string, args []any) (any, bool) {
	switch name {
	case "always":
		return true, len(args) == 0
	case "success", "failure", "cancelled":
		status := p.ctx["status"]
		if status == "" {
			status = "success"
		}
		return status == name, len(args) == 0
	case "fromjson":
		if len(args) == 1 {
			var value any
			if err := json.Unmarshal([]byte(fmt.Sprint(args[0])), &value); err == nil {
				return value, true
			}
		}
	case "tojson":
		if len(args) == 1 {
			data, _ := json.Marshal(args[0])
			return string(data), true
		}
	case "format":
		if len(args) > 0 {
			result := fmt.Sprint(args[0])
			for i, arg := range args[1:] {
				result = strings.ReplaceAll(result, fmt.Sprintf("{%d}", i), fmt.Sprint(arg))
			}
			return result, true
		}
	case "contains", "startswith", "endswith":
		if len(args) == 2 {
			if values, ok := args[0].([]any); ok && name == "contains" {
				return slices.ContainsFunc(values, func(value any) bool { return equalExpressionValues(value, args[1]) }), true
			}
			left, right := strings.ToLower(fmt.Sprint(args[0])), strings.ToLower(fmt.Sprint(args[1]))
			switch name {
			case "contains":
				return strings.Contains(left, right), true
			case "startswith":
				return strings.HasPrefix(left, right), true
			case "endswith":
				return strings.HasSuffix(left, right), true
			}
		}
	case "hashfiles":
		return fmt.Sprint(args), true
	}
	return nil, false
}
