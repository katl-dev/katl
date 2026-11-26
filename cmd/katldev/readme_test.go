package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestREADMEUsage(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(readme)
	// Match option columns only. Help descriptions can mention flags from
	// other commands.
	flags := regexp.MustCompile(`^\s+(-[a-zA-Z], )?(--[a-zA-Z][a-zA-Z0-9-]*)\s`)
	subcommands := regexp.MustCompile(`(?m)^  ([a-zA-Z0-9][a-zA-Z0-9-]*)[ \t]{2,}`)

	var checkHelp func([]string)
	checkHelp = func(path []string) {
		command := strings.Join(append([]string{"katldev"}, path...), " ")
		if len(path) > 0 && !strings.Contains(doc, "`"+command+"`") {
			t.Errorf("README.md is missing command %q", command)
		}

		var stdout, stderr bytes.Buffer
		args := append(append([]string{}, path...), "--help")
		if err := run(t.Context(), args, &stdout, &stderr); err != nil {
			t.Fatalf("%s --help: %v\n%s", command, err, &stderr)
		}
		help := stdout.String()
		_, options, found := strings.Cut(help, "\nFlags:\n")
		if !found {
			t.Fatalf("%s --help output has no flags:\n%s", command, help)
		}
		for line := range strings.SplitSeq(options, "\n") {
			match := flags.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			for _, flag := range []string{strings.TrimSuffix(match[1], ", "), match[2]} {
				if flag != "" && !strings.Contains(doc, "`"+flag+"`") {
					t.Errorf("README.md is missing %s flag %q", command, flag)
				}
			}
		}
		if _, children, found := strings.Cut(help, "\nAvailable Commands:\n"); found {
			children, _, _ = strings.Cut(children, "\n\n")
			for _, match := range subcommands.FindAllStringSubmatch(children, -1) {
				checkHelp(append(path, match[1]))
			}
		}
	}
	checkHelp(nil)
}
