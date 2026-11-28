// Command katl-vmtest-ran checks that the tests a go test invocation asked
// for via -run reported a pass in the run's go test log. With -summary it
// instead prints the log's test results, one line each.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/gotestlog"
)

func main() {
	log := flag.String("log", "", "go test log, in text -v or -json form")
	summary := flag.Bool("summary", false, "print the test results in the log instead of checking them")
	flag.Parse()

	f, err := os.Open(*log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer f.Close()

	if *summary {
		if err := writeSummary(os.Stdout, f); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}

	outcomes := parseOutcomes(f)
	if err := check(runPattern(flag.Args()), outcomes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// eachResult calls fn with every test result in a go test log, in log order.
func eachResult(r io.Reader, fn func(gotestlog.Event)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		if event, ok := gotestlog.ParseTestResult(scanner.Text()); ok {
			fn(event)
		}
	}
	return scanner.Err()
}

// writeSummary writes each test result in go test's -v header form, with
// subtests indented under their parents.
func writeSummary(w io.Writer, r io.Reader) error {
	var b strings.Builder
	if err := eachResult(r, func(event gotestlog.Event) {
		indent := strings.Repeat("    ", strings.Count(event.Test, "/"))
		fmt.Fprintf(&b, "%s--- %s: %s (%.2fs)\n", indent, strings.ToUpper(event.Action), event.Test, event.Elapsed)
	}); err != nil {
		return err
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// parseOutcomes returns the terminal action of each top-level test. Subtests
// are not evidence that the test they belong to ran to completion.
func parseOutcomes(r io.Reader) map[string]string {
	outcomes := map[string]string{}
	_ = eachResult(r, func(event gotestlog.Event) {
		if !strings.Contains(event.Test, "/") {
			outcomes[event.Test] = event.Action
		}
	})
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
