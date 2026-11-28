package main

import (
	"strings"
	"testing"
)

func TestParseUpgradeBaseline(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{name: "final", content: "v2026.11.7\n", want: "v2026.11.7"},
		{name: "no trailing newline", content: "v2026.11.7", want: "v2026.11.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUpgradeBaseline([]byte(tc.content))
			if err != nil || got.Tag() != tc.want {
				t.Fatalf("parseUpgradeBaseline(%q) = %v, %v, want %s", tc.content, got, err, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name, content, offending string
	}{
		{name: "candidate", content: "v2026.11.7-rc.1\n", offending: `"v2026.11.7-rc.1"`},
		{name: "missing v", content: "2026.11.7\n", offending: `"2026.11.7"`},
		{name: "empty", content: "", offending: `""`},
		{name: "blank line", content: "\n", offending: `""`},
		{name: "second tag", content: "v2026.11.7\nv2026.11.6\n", offending: `"v2026.11.7\nv2026.11.6"`},
		{name: "trailing blank line", content: "v2026.11.7\n\n", offending: `"v2026.11.7\n"`},
		{name: "CRLF", content: "v2026.11.7\r\n", offending: `"v2026.11.7\r"`},
		{name: "leading space", content: " v2026.11.7\n", offending: `" v2026.11.7"`},
		{name: "trailing space", content: "v2026.11.7 \n", offending: `"v2026.11.7 "`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUpgradeBaseline([]byte(tc.content))
			if err == nil {
				t.Fatalf("parseUpgradeBaseline(%q) = %v, want an error", tc.content, got)
			}
			for _, part := range []string{upgradeBaselineFile, "final KatlOS release tag", tc.offending} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("parseUpgradeBaseline(%q) error %q does not contain %s", tc.content, err, part)
				}
			}
		})
	}
}
