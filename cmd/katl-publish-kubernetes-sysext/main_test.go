package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRequiresMetadata(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := run([]string{"--metadata", "/missing", "--output-dir", t.TempDir()}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "read Kubernetes sysext metadata") {
		t.Fatalf("run() error = %v", err)
	}
}
