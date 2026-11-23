package configapply

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// sysctlDirs lists the sysctl.d directories in systemd-sysctl precedence
// order: a file replaces any file with the same name in a later directory.
var sysctlDirs = []string{"/etc/sysctl.d", "/run/sysctl.d", "/usr/local/lib/sysctl.d", "/usr/lib/sysctl.d"}

// irreversibleSysctls are kernel parameters whose runtime value cannot be
// restored after some writes, so a live apply could not roll them back.
var irreversibleSysctls = map[string]struct{}{
	"kernel.kexec_load_disabled":       {},
	"kernel.kexec_load_limit_panic":    {},
	"kernel.kexec_load_limit_reboot":   {},
	"kernel.modules_disabled":          {},
	"kernel.unprivileged_bpf_disabled": {},
	"kernel.yama.ptrace_scope":         {},
}

type sysctlFile struct {
	Path    string
	Content string
}

// sysctlAssignment is the entry systemd-sysctl keeps for a key. Excluded means
// the entry is an exclusion, so systemd-sysctl does not write the key.
type sysctlAssignment struct {
	Path     string
	Value    string
	Excluded bool
}

// effectiveSysctlFiles returns the sysctl.d files systemd-sysctl would read
// when the files in managed replace Katl's managed files, in application
// order. Host files at a path in owned belong to Katl and are left out.
func effectiveSysctlFiles(root string, managed map[string]string, owned map[string]struct{}) ([]sysctlFile, error) {
	byName := make(map[string]sysctlFile)
	var errs []error
	for i := len(sysctlDirs) - 1; i >= 0; i-- {
		dir := sysctlDirs[i]
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			filePath := path.Join(dir, name)
			// systemd-sysctl ignores hidden files.
			if !strings.HasSuffix(name, ".conf") || strings.HasPrefix(name, ".") {
				continue
			}
			if _, ok := owned[filePath]; ok {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, filePath))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			byName[name] = sysctlFile{Path: filePath, Content: string(data)}
		}
	}
	for filePath, content := range managed {
		byName[path.Base(filePath)] = sysctlFile{Path: filePath, Content: content}
	}
	files := make([]sysctlFile, 0, len(byName))
	for _, file := range byName {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return path.Base(files[i].Path) < path.Base(files[j].Path) })
	if err := errors.Join(errs...); err != nil {
		return files, fmt.Errorf("read host sysctl configuration: %w", err)
	}
	return files, nil
}

// effectiveSysctl returns the entry systemd-sysctl keeps for a concrete key:
// the last explicit assignment or exclusion in file order. An exclusion
// replaces earlier assignments, so systemd-sysctl leaves the key unset. Globs
// never override an explicit entry, so they do not affect a key that has one.
func effectiveSysctl(files []sysctlFile, key string) (sysctlAssignment, bool) {
	var winner sysctlAssignment
	found := false
	for _, file := range files {
		for _, line := range sysctlAssignmentLines(file.Content) {
			if line.key == key {
				winner = sysctlAssignment{Path: file.Path, Value: line.value, Excluded: line.excluded}
				found = true
			}
		}
	}
	return winner, found
}

type sysctlLine struct {
	key      string
	value    string
	excluded bool
}

// sysctlAssignmentLines returns the explicit assignments in a sysctl.d file,
// including those prefixed with "-", and the exclusions of concrete keys,
// written as "-key" without a value. It skips globs and lines systemd-sysctl
// ignores.
func sysctlAssignmentLines(content string) []sysctlLine {
	var lines []sysctlLine
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, found := strings.Cut(strings.TrimPrefix(line, "-"), "=")
		if !found && !strings.HasPrefix(line, "-") {
			continue
		}
		key = normalizeSysctlKey(strings.TrimSpace(key))
		if key == "" || strings.ContainsAny(key, "*?[") {
			continue
		}
		lines = append(lines, sysctlLine{key: key, value: strings.TrimSpace(value), excluded: !found})
	}
	return lines
}

// normalizeSysctlKey writes a key with "." separators, following sysctl.d:
// when the first separator is "/", slashes and dots are interchanged.
func normalizeSysctlKey(key string) string {
	if i := strings.IndexAny(key, "./"); i < 0 || key[i] == '.' {
		return key
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '/':
			return '.'
		case '.':
			return '/'
		}
		return r
	}, key)
}

// sysctlRuntimeRestriction explains why a kernel parameter cannot be
// snapshotted, written, verified, and restored on the running host.
func sysctlRuntimeRestriction(root, key string) string {
	if _, irreversible := irreversibleSysctls[key]; irreversible {
		return fmt.Sprintf("sysctl %s cannot be restored after it is set", key)
	}
	// Disabling IPv6 flushes the interface's addresses, which restoring the
	// value does not bring back.
	if strings.HasPrefix(key, "net.ipv6.conf.") && strings.HasSuffix(key, ".disable_ipv6") {
		return fmt.Sprintf("sysctl %s removes IPv6 addresses that restoring it cannot bring back", key)
	}
	info, err := os.Stat(filepath.Join(root, "/proc/sys", strings.ReplaceAll(key, ".", "/")))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("sysctl %s does not exist in the running kernel", key)
	case err != nil:
		return fmt.Sprintf("sysctl %s cannot be inspected: %v", key, err)
	case !info.Mode().IsRegular() || info.Mode().Perm()&0o600 != 0o600:
		return fmt.Sprintf("sysctl %s cannot be both read and written at runtime", key)
	}
	return ""
}
