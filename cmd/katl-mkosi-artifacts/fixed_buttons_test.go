package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixedButtonScope(t *testing.T) {
	for _, test := range []struct {
		release string
		want    bool
	}{
		{"6.18.56-200.fc44.x86_64", false},
		{"6.18.57-200.fc44.x86_64", true},
		{"6.18.58-200.fc44.x86_64", true},
		{"6.18.59-200.fc44.x86_64", true},
		{"6.18.60-200.fc44.x86_64", false},
		{"7.2.11-200.fc44.x86_64", false},
	} {
		t.Run(test.release, func(t *testing.T) {
			if got := needsFixedButtons(test.release); got != test.want {
				t.Fatalf("needs fixed buttons = %v, want %v", got, test.want)
			}
		})
	}
}

func TestFixedButtonsUnaffectedKernel(t *testing.T) {
	root, _ := kernelInputFixture(t)
	destination := t.TempDir()
	if err := runBuildFixedButtons([]string{root, destination, t.TempDir()}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("unaffected kernel acquired compatibility module content")
	}
}

func TestFixedButtonsRequireSelectedHeaders(t *testing.T) {
	root, _ := kernelInputFixture(t)
	path := filepath.Join(root, embeddedKernelLock)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.ReplaceAll(string(data), "6.18.1-", "6.18.58-"))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	err = runBuildFixedButtons([]string{root, t.TempDir(), t.TempDir()}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "prepared kernel inputs do not match 6.18.58-") {
		t.Fatalf("build with different headers = %v", err)
	}
}
