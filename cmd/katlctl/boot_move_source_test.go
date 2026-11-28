package main

import (
	"strings"
	"testing"
)

func TestCheckBootMoveSource(t *testing.T) {
	for _, tc := range []struct {
		name, current, target, client string
		refused                       bool
	}{
		{name: "pre-reader source to this release", current: "2026.11.5", target: "2026.12.0", client: "2026.12.0", refused: true},
		{name: "pre-reader source to a candidate of this release", current: "2026.11.5", target: "2026.12.0-rc.1", client: "v2026.12.0", refused: true},
		{name: "pre-reader source to a later release", current: "2026.11.5", target: "2027.1.0", client: "2026.12.0", refused: true},
		{name: "pre-reader source to an older release", current: "2026.11.5", target: "2026.11.6", client: "2026.12.0"},
		{name: "reader source", current: "2026.11.6", target: "2026.12.0", client: "2026.12.0"},
		{name: "reader pre-release source", current: "2026.11.6-rc.1", target: "2026.12.0", client: "2026.12.0"},
		{name: "pre-reader candidate source", current: "2026.11.5-rc.1", target: "2026.12.0", client: "2026.12.0", refused: true},
		{name: "development source", current: "0.0.0-dev", target: "2026.12.0", client: "2026.12.0"},
		{name: "stamped development source", current: "2026.11.5-dev.0", target: "2026.12.0", client: "2026.12.0"},
		{name: "pre-reader source from a development katlctl", current: "2026.11.5", target: "2026.11.7-dev.0", client: "2026.11.7-dev.0", refused: true},
		{name: "development katlctl", current: "2026.11.5", target: "2026.12.0", client: "git-1234abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBootMoveSource(tc.current, tc.target, tc.client)
			if !tc.refused {
				if err != nil {
					t.Fatalf("checkBootMoveSource() = %v, want admitted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "upgrade to 2026.11.6 or later first") || !strings.Contains(err.Error(), "KatlOS "+tc.current) {
				t.Fatalf("checkBootMoveSource() = %v, want refusal naming %s and the first reader release", err, tc.current)
			}
		})
	}
}
