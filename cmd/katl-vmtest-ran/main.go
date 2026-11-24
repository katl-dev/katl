// Command katl-vmtest-ran checks that the tests a go test invocation asked
// for via -run reported a pass in the run's go test log.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

func main() {
	log := flag.String("log", "", "go test log, in text -v or -json form")
	flag.Parse()

	f, err := os.Open(*log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer f.Close()

	outcomes := parseOutcomes(f)
	if err := check(runPattern(flag.Args()), outcomes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var textResult = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (\S+)`)

// parseOutcomes returns the terminal action of each top-level test. Subtests
// are not evidence that the test they belong to ran to completion.
func parseOutcomes(r io.Reader) map[string]string {
	outcomes := map[string]string{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		line := scanner.Text()
		var name, action string
		if m := textResult.FindStringSubmatch(line); m != nil {
			action, name = strings.ToLower(m[1]), m[2]
		} else if strings.HasPrefix(line, "{") {
			var event struct{ Action, Test string }
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			action, name = event.Action, event.Test
		}
		switch action {
		case "pass", "fail", "skip":
		default:
			continue
		}
		if name != "" && !strings.Contains(name, "/") {
			outcomes[name] = action
		}
	}
	return outcomes
}

// runPattern returns the last -run value in go test arguments, which is the
// one go test applies.
func runPattern(args []string) (pattern string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-args" {
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !strings.HasPrefix(arg, "-") || (name != "run" && name != "test.run") {
			continue
		}
		if !hasValue && i+1 < len(args) {
			i++
			value = args[i]
		}
		pattern = value
	}
	return pattern
}

var testName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// requestedTests returns the test names a -run pattern selects when it is an
// anchored alternation of plain names, either ^(A|B)$ or ^A$|^B$, optionally
// followed by a subtest pattern. Any other pattern names no tests.
func requestedTests(pattern string) []string {
	top, _, _ := strings.Cut(pattern, "/")
	var names []string
	if body, ok := strings.CutPrefix(top, "^("); ok {
		body, ok = strings.CutSuffix(body, ")$")
		if !ok {
			return nil
		}
		names = strings.Split(body, "|")
	} else {
		for _, alt := range strings.Split(top, "|") {
			name, ok := strings.CutPrefix(alt, "^")
			if !ok {
				return nil
			}
			if name, ok = strings.CutSuffix(name, "$"); !ok {
				return nil
			}
			names = append(names, name)
		}
	}
	for _, name := range names {
		if !testName.MatchString(name) {
			return nil
		}
	}
	return names
}

// check fails when a run selected by pattern has no evidence of the tests it
// asked for. Without a pattern the run selects everything and nothing is
// checked.
func check(pattern string, outcomes map[string]string) error {
	if pattern == "" {
		return nil
	}
	if len(outcomes) == 0 {
		return fmt.Errorf("vmtest: -run %q matched no tests; check the test names and packages", pattern)
	}
	var missing []string
	for _, name := range requestedTests(pattern) {
		switch outcomes[name] {
		case "pass":
		case "":
			missing = append(missing, name+" (not found in the given packages)")
		default:
			missing = append(missing, name+" ("+outcomes[name]+")")
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("vmtest: requested tests did not pass:\n  %s", strings.Join(missing, "\n  "))
}
