package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewAdditions(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("packages.tsv", "base\t1:1.0-1.x86_64\n", 0o644)
	write("usr/bin/tool", "original program", 0o755)
	write("usr/lib/systemd/system/tool.service", "[Service]\nExecStart=/usr/bin/tool\n", 0o644)
	var recorded bytes.Buffer
	args := []string{"--root", root, "--packages", filepath.Join(root, "packages.tsv"), "--lock", filepath.Join(root, "components.lock")}
	if err := run(append([]string{"record"}, args...), &recorded); err != nil {
		t.Fatal(err)
	}
	const want = "package base\nprogram /usr/bin/tool\nsystem-unit tool.service\n"
	if recorded.String() != want {
		t.Fatalf("recorded inventory:\n%s\nwant:\n%s", &recorded, want)
	}
	write("components.lock", want, 0o644)

	// Package updates and non-entrypoint payload changes must not require review.
	write("packages.tsv", "base\t1:2.0-3.x86_64\n", 0o644)
	write("usr/bin/tool", "updated program", 0o755)
	write("usr/lib64/library.so.2.0", "library", 0o755)
	write("usr/lib/modules/7.4.0/kernel/driver.ko", "module", 0o755)
	write("usr/lib/firmware/device/revision.bin", "firmware", 0o755)
	write("usr/share/locale/en/messages", "translation", 0o755)
	if err := run(append([]string{"verify"}, args...), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "usr/bin/tool")); err != nil {
		t.Fatal(err)
	}
	if err := run(append([]string{"verify"}, args...), &bytes.Buffer{}); err != nil {
		t.Fatalf("removing a component requires no approval: %v", err)
	}

	// A distribution update can add entrypoints without adding a package.
	write("usr/libexec/new-helper", "program", 0o755)
	write("usr/lib/systemd/system/new-helper.socket", "[Socket]\nListenStream=/run/helper\n", 0o644)
	write("packages.tsv", "base\t1:2.0-3.x86_64\nnew-library\t0:1-1.x86_64\n", 0o644)
	err := run(append([]string{"verify"}, args...), &bytes.Buffer{})
	for _, addition := range []string{"package new-library", "program /usr/libexec/new-helper", "system-unit new-helper.socket"} {
		if err == nil || !strings.Contains(err.Error(), addition) {
			t.Fatalf("addition %q was not reported: %v", addition, err)
		}
	}
	write("components.lock", want+"package new-library\nprogram /usr/libexec/new-helper\nsystem-unit new-helper.socket\n", 0o644)
	if err := run(append([]string{"verify"}, args...), &bytes.Buffer{}); err != nil {
		t.Fatalf("explicitly reviewed additions refused: %v", err)
	}
}

func TestInstrumentedImageReview(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"packages.tsv":                           "base\t0:1-1.x86_64\n",
		"components.lock":                        "package base\n",
		"test.lock":                              "system-unit fixture.service\n",
		"usr/lib/systemd/system/fixture.service": "[Service]\nExecStart=/usr/bin/true\n",
	} {
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"verify", "--root", root, "--packages", filepath.Join(root, "packages.tsv"), "--lock", filepath.Join(root, "components.lock")}
	if err := run(args, &bytes.Buffer{}); err == nil {
		t.Fatal("production verification accepted a test-only addition")
	}
	if err := run(append(args, "--test-lock", filepath.Join(root, "test.lock")), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}
