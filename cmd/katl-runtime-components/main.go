package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "katl-runtime-components:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 || (args[0] != "record" && args[0] != "verify") {
		return fmt.Errorf("usage: katl-runtime-components record|verify --root DIR --packages FILE [--lock FILE] [--test-lock FILE]")
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	root := flags.String("root", "", "assembled runtime root")
	packages := flags.String("packages", "", "resolved RPM package inventory")
	lock := flags.String("lock", "mkosi.profiles/runtime/components.lock", "reviewed production component names")
	testLock := flags.String("test-lock", "", "additional reviewed components for an instrumented VM-test image")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *root == "" || *packages == "" {
		return fmt.Errorf("--root and --packages are required; no positional arguments are accepted")
	}
	components, err := inventory(*root, *packages)
	if err != nil {
		return err
	}
	if command == "record" {
		_, err := fmt.Fprintln(stdout, strings.Join(components, "\n"))
		return err
	}
	approved := map[string]bool{}
	for _, path := range []string{*lock, *testLock} {
		if path == "" {
			continue
		}
		entries, err := readLock(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			approved[entry] = true
		}
	}
	var added []string
	for _, component := range components {
		if !approved[component] {
			added = append(added, component)
		}
	}
	if len(added) != 0 {
		return fmt.Errorf("unreviewed runtime components:\n  %s\nreview each addition and update %s intentionally; use the record command to inspect the assembled inventory", strings.Join(added, "\n  "), *lock)
	}
	_, err = fmt.Fprintf(stdout, "runtime components: %d reviewed names verified\n", len(components))
	return err
}

func inventory(root, packages string) ([]string, error) {
	data, err := os.ReadFile(packages)
	if err != nil {
		return nil, err
	}
	components := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid package inventory line %q", line)
		}
		components["package "+fields[0]] = true
	}
	usr := filepath.Join(root, "usr")
	err = filepath.WalkDir(usr, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(usr, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			switch relative {
			case "share", "src", "lib/firmware", "lib/modules", "lib/debug", "lib/sysimage":
				return filepath.SkipDir
			}
			return nil
		}
		for _, scope := range []string{"system", "user"} {
			if strings.HasPrefix(relative, "lib/systemd/"+scope+"/") {
				// Wants/requires links select existing units; they do not add components.
				if filepath.Dir(relative) == "lib/systemd/"+scope {
					switch filepath.Ext(entry.Name()) {
					case ".service", ".socket", ".target", ".timer", ".path", ".mount", ".automount", ".slice", ".scope", ".device":
						components[scope+"-unit "+entry.Name()] = true
					}
				}
				return nil
			}
		}
		if strings.HasSuffix(entry.Name(), ".so") || strings.Contains(entry.Name(), ".so.") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		directory := filepath.ToSlash(filepath.Dir(relative))
		generator := strings.HasPrefix(directory, "lib/systemd/") && strings.HasSuffix(directory, "generators")
		commandLink := entry.Type()&os.ModeSymlink != 0 && (directory == "bin" || directory == "sbin" || directory == "libexec" || strings.HasPrefix(directory, "libexec/") || generator)
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 || commandLink {
			components["program /usr/"+relative] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inventory runtime: %w", err)
	}
	result := make([]string, 0, len(components))
	for component := range components {
		result = append(result, component)
	}
	slices.Sort(result)
	return result, nil
}

func readLock(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []string
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[0] != "package" && fields[0] != "program" && fields[0] != "system-unit" && fields[0] != "user-unit") || seen[line] {
			return nil, fmt.Errorf("invalid or duplicate component in %s: %q", path, line)
		}
		seen[line] = true
		entries = append(entries, line)
	}
	return entries, scanner.Err()
}
