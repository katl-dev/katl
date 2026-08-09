package scriptstest

import (
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var markdownLinkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
var markdownHeadingPattern = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*#*[ \t]*$`)
var markdownAnchorPunctuation = regexp.MustCompile(`[^\pL\pN _-]+`)

func TestPublicDocumentationLocalLinks(t *testing.T) {
	repo := repoRoot(t)
	public := publicMarkdownFiles(t, repo)
	public[filepath.Join(repo, "README.md")] = true

	for path := range public {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, match := range markdownLinkPattern.FindAllStringSubmatch(string(content), -1) {
			target := strings.TrimSpace(match[1])
			if strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
				target = strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
			}
			if fields := strings.Fields(target); len(fields) > 0 {
				target = fields[0]
			}
			if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			fragment := ""
			if index := strings.IndexByte(target, '#'); index >= 0 {
				fragment = target[index+1:]
				target = target[:index]
			}
			if target == "" {
				target = filepath.Base(path)
			}
			decoded, err := url.PathUnescape(target)
			if err != nil {
				t.Errorf("%s has invalid escaped link %q: %v", relativePath(repo, path), target, err)
				continue
			}
			linked := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(decoded)))
			if _, err := os.Stat(linked); err != nil {
				t.Errorf("%s links to missing %q", relativePath(repo, path), target)
				continue
			}
			if fragment != "" && strings.EqualFold(filepath.Ext(linked), ".md") && !markdownFileHasAnchor(t, linked, fragment) {
				t.Errorf("%s links to missing anchor #%s in %s", relativePath(repo, path), fragment, relativePath(repo, linked))
			}
		}
	}
}

func markdownFileHasAnchor(t *testing.T, path, target string) bool {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	counts := map[string]int{}
	for _, match := range markdownHeadingPattern.FindAllStringSubmatch(string(content), -1) {
		base := strings.ToLower(strings.TrimSpace(match[1]))
		base = markdownAnchorPunctuation.ReplaceAllString(base, "")
		base = strings.ReplaceAll(base, " ", "-")
		anchor := base
		if count := counts[base]; count > 0 {
			anchor += "-" + strconv.Itoa(count)
		}
		counts[base]++
		if anchor == target {
			return true
		}
	}
	return false
}

func TestDocumentationLandingReachesEveryPublicPage(t *testing.T) {
	repo := repoRoot(t)
	public := publicMarkdownFiles(t, repo)
	start := filepath.Join(repo, "docs", "README.md")
	seen := map[string]bool{}
	queue := []string{start}

	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		if seen[path] {
			continue
		}
		seen[path] = true
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, match := range markdownLinkPattern.FindAllStringSubmatch(string(content), -1) {
			target := strings.Fields(strings.TrimSpace(match[1]))[0]
			if index := strings.IndexByte(target, '#'); index >= 0 {
				target = target[:index]
			}
			if target == "" || strings.Contains(target, "://") || !strings.HasSuffix(strings.ToLower(target), ".md") {
				continue
			}
			linked := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))
			if public[linked] && !seen[linked] {
				queue = append(queue, linked)
			}
		}
	}

	for path := range public {
		if !seen[path] {
			t.Errorf("docs/README.md does not reach public page %s", relativePath(repo, path))
		}
	}
}

func publicMarkdownFiles(t *testing.T, repo string) map[string]bool {
	t.Helper()
	root := filepath.Join(repo, "docs")
	files := map[string]bool{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path == filepath.Join(root, "internal") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			files[path] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walk public docs: %v", err)
	}
	return files
}

func relativePath(repo, path string) string {
	relative, err := filepath.Rel(repo, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}
