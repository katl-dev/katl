// Package buildinputs owns the digest of a committed tree's declared build
// inputs. Callers that decide from those inputs, such as driver qualification
// and extension revisions, supply what to read; the digest format is shared.
package buildinputs

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"path"
	"slices"
	"strings"
)

// Spec declares the inputs of one digest domain.
type Spec struct {
	// Domain separates digests of different decisions over the same tree.
	Domain string
	// Version prefixes every digest. Bump it when changing the format or the
	// interpretation of the inputs.
	Version string
	// Include lists the shared inputs as path.Match patterns. A terminal /**
	// includes the whole subtree.
	Include []string
	// Exclude lists name suffixes that are never inputs, even when included.
	Exclude []string
	// Salts maps each salt to the path of an input appended only to that
	// salt's digest, or to "" for none. A salt whose appended input is not a
	// regular file at the revision has no digest.
	Salts map[string]string
	// Project optionally returns a projection of a regular file's content,
	// so that only the parts that matter to the domain are digested.
	Project func(name string) func([]byte) ([]byte, error)
}

type input struct{ name, mode, content string }

// Digests returns the digest for each salt of spec over revision in the
// repository at dir.
func Digests(dir, revision string, spec Spec) (map[string]string, error) {
	tree, err := git(dir, "ls-tree", "-rz", "--full-tree", revision)
	if err != nil {
		return nil, err
	}

	appended := map[string]bool{}
	for _, name := range spec.Salts {
		appended[name] = true
	}
	files := map[string]input{}
	var selected []string
	for line := range bytes.SplitSeq(bytes.TrimSuffix(tree, []byte{0}), []byte{0}) {
		metadata, name, ok := strings.Cut(string(line), "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("invalid git tree entry %q", line)
		}
		files[name] = input{name: name, mode: fields[0], content: fields[2]}
		if !appended[name] && spec.included(name) {
			selected = append(selected, name)
		}
	}

	var shared []input
	// Follow selected links through the committed tree. Keep link blobs as
	// inputs too, so changing the destination is itself observable.
	seen := map[string]bool{}
	for i := 0; i < len(selected); i++ {
		name := selected[i]
		if seen[name] {
			continue
		}
		seen[name] = true
		file := files[name]
		switch file.mode {
		case "120000":
			link, err := git(dir, "cat-file", "blob", file.content)
			if err != nil {
				return nil, err
			}
			target := path.Join(path.Dir(name), string(link))
			if !path.IsAbs(string(link)) && target != ".." && !strings.HasPrefix(target, "../") {
				if _, ok := files[target]; ok {
					selected = append(selected, target)
				} else {
					for child := range files {
						if target == "." || strings.HasPrefix(child, target+"/") {
							selected = append(selected, child)
						}
					}
				}
			}
		case "100644", "100755":
			if project := spec.projection(name); project != nil {
				data, err := git(dir, "cat-file", "blob", file.content)
				if err != nil {
					return nil, err
				}
				projected, err := project(data)
				if err != nil {
					return nil, fmt.Errorf("project input %q: %w", name, err)
				}
				file.content = fmt.Sprintf("%x", sha256.Sum256(projected))
			}
		default:
			return nil, fmt.Errorf("input %q is not a Git blob", name)
		}
		shared = append(shared, file)
	}

	digests := map[string]string{}
	for salt, name := range spec.Salts {
		inputs := slices.Clone(shared)
		if name != "" {
			file, ok := files[name]
			if !ok || (file.mode != "100644" && file.mode != "100755") {
				continue
			}
			inputs = append(inputs, file)
		}
		slices.SortFunc(inputs, func(a, b input) int { return strings.Compare(a.name, b.name) })
		hash := sha256.New()
		fmt.Fprintf(hash, "%s\x00%s\x00%s\x00", spec.Domain, spec.Version, salt)
		for _, input := range inputs {
			fmt.Fprintf(hash, "%s\x00%s\x00%s\x00", input.name, input.mode, input.content)
		}
		digests[salt] = fmt.Sprintf("%s:%x", spec.Version, hash.Sum(nil))
	}
	return digests, nil
}

func (spec Spec) included(name string) bool {
	for _, suffix := range spec.Exclude {
		if strings.HasSuffix(name, suffix) {
			return false
		}
	}
	for _, pattern := range spec.Include {
		// A terminal /** includes the whole subtree. path.Match handles the
		// remaining pattern, including directory names with wildcards.
		candidate := name
		if base, recursive := strings.CutSuffix(pattern, "/**"); recursive {
			pattern = base
			depth := strings.Count(pattern, "/") + 1
			parts := strings.SplitN(name, "/", depth+1)
			if len(parts) <= depth {
				continue
			}
			candidate = strings.Join(parts[:depth], "/")
		}
		if matched, _ := path.Match(pattern, candidate); matched {
			return true
		}
	}
	return false
}

func (spec Spec) projection(name string) func([]byte) ([]byte, error) {
	if spec.Project == nil {
		return nil
	}
	return spec.Project(name)
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
