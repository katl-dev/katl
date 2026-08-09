package main

import (
	"bufio"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDocumentedKatlctlCommandsAndFlagsExist(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(repo, "README.md")}
	if err := filepath.WalkDir(filepath.Join(repo, "docs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path == filepath.Join(repo, "docs", "internal") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	root := newKatlctlCommand(context.Background(), io.Discard, io.Discard)
	for _, path := range paths {
		content, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(content)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "katlctl ") {
				continue
			}
			startLine := lineNumber
			invocation := strings.TrimSuffix(line, "\\")
			for strings.HasSuffix(line, "\\") && scanner.Scan() {
				lineNumber++
				line = strings.TrimSpace(scanner.Text())
				invocation += " " + strings.TrimSuffix(line, "\\")
			}
			checkDocumentedInvocation(t, root, path, startLine, invocation)
		}
		if err := scanner.Err(); err != nil {
			t.Errorf("scan %s: %v", path, err)
		}
		content.Close()
	}
}

func checkDocumentedInvocation(t *testing.T, root *cobra.Command, path string, line int, invocation string) {
	t.Helper()
	fields := strings.Fields(invocation)
	if len(fields) < 2 {
		return
	}
	command, remaining, err := root.Find(fields[1:])
	if err != nil {
		t.Errorf("%s:%d documents an unknown command in %q: %v", path, line, invocation, err)
		return
	}
	if command.HasSubCommands() && len(remaining) > 0 && !strings.HasPrefix(remaining[0], "-") {
		t.Errorf("%s:%d documents unknown %s subcommand %q", path, line, command.CommandPath(), remaining[0])
		return
	}
	for _, field := range remaining {
		if !strings.HasPrefix(field, "--") {
			continue
		}
		name := strings.TrimPrefix(strings.SplitN(field, "=", 2)[0], "--")
		if command.Flags().Lookup(name) == nil && command.InheritedFlags().Lookup(name) == nil {
			t.Errorf("%s:%d documents unknown %s flag --%s", path, line, command.CommandPath(), name)
		}
	}
}
