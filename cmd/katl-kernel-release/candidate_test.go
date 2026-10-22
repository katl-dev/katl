package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/kernelrelease"
)

func TestCandidateOrigins(t *testing.T) {
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })

	for _, tc := range []struct{ repository, origin string }{
		{"fedora", "https://dl.fedoraproject.org/pub/fedora/linux/releases/44/Everything/x86_64/os/Packages/k/"},
		{"updates", "https://dl.fedoraproject.org/pub/fedora/linux/updates/44/Everything/x86_64/Packages/k/"},
	} {
		t.Run(tc.repository, func(t *testing.T) {
			bin := t.TempDir()
			commands := map[string]string{
				"dnf5": `#!/usr/bin/env bash
set -eu
selector="${!#}"
name="${selector%-7.2.11-200.fc44.x86_64}"
if [[ "$*" == *'%{repoid}'* ]]; then
  printf '%s\t%s-0:7.2.11-200.fc44.x86_64\thttp://mirror.invalid/package.rpm\t%s\n' "$name" "$name" "$KATL_TEST_REPOSITORY"
else
  printf '%s\t%s-0:7.2.11-200.fc44.x86_64\t7.2.11-200.fc44.x86_64\thttp://mirror.invalid/package.rpm\n' "$name" "$name"
fi
`,
				"rpmkeys": "#!/bin/sh\nexit 0\n",
				"rpm": `#!/usr/bin/env bash
set -eu
file="${!#}"
file="${file##*/}"
printf '%s-0:7.2.11-200.fc44.x86_64' "${file%-7.2.11-200.fc44.x86_64.rpm}"
`,
			}
			for name, script := range commands {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("KATL_TEST_REPOSITORY", tc.repository)

			files := []string{
				"kernel-core-7.2.11-200.fc44.x86_64.rpm",
				"kernel-modules-7.2.11-200.fc44.x86_64.rpm",
				"kernel-modules-core-7.2.11-200.fc44.x86_64.rpm",
				"kernel-devel-7.2.11-200.fc44.x86_64.rpm",
			}
			requests := make(map[string]int)
			for _, file := range files {
				requests[tc.origin+file] = 0
			}
			http.DefaultTransport = captureTransport(func(request *http.Request) (*http.Response, error) {
				url := request.URL.String()
				if _, ok := requests[url]; !ok {
					return nil, fmt.Errorf("unexpected capture origin %s", url)
				}
				requests[url]++
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(request.URL.Path)), Header: make(http.Header)}, nil
			})

			output := t.TempDir()
			args := []string{"candidate", "--fedora-release", "44", "--kernel-release", "7.2.11-200.fc44.x86_64", "--output", output}
			if err := run(context.Background(), args, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}

			lock, err := kernelrelease.Read(filepath.Join(output, "lock.json"))
			if err != nil {
				t.Fatal(err)
			}
			for i, pkg := range lock.Packages {
				if pkg.SourceURL != tc.origin+files[i] || requests[pkg.SourceURL] != 1 {
					t.Fatalf("captured origin %q, requests %v", pkg.SourceURL, requests)
				}
			}
		})
	}
}

type captureTransport func(*http.Request) (*http.Response, error)

func (transport captureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
