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
			err := checkBootMoveSource(tc.current, tc.target, tc.client, "cp-1", " --config cluster.yaml")
			if !tc.refused {
				if err != nil {
					t.Fatalf("checkBootMoveSource() = %v, want admitted", err)
				}
				return
			}
			want := "this node runs KatlOS " + tc.current + ", which predates boot-move records; this katlctl does not upgrade it straight to KatlOS " + tc.target +
				". First run 'katlctl node upgrade cp-1 --config cluster.yaml --version 2026.11.6', then retry this upgrade"
			if err == nil || err.Error() != want {
				t.Fatalf("checkBootMoveSource() = %v, want %q", err, want)
			}
		})
	}
}

// The suggested command addresses the node the same way the refused one did.
func TestCheckBootMoveSourceCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts managementTargetOptions
		want string
	}{
		{name: "config", opts: managementTargetOptions{clusterConfigPath: "cluster.yaml"}, want: "'katlctl node upgrade cp-1 --config cluster.yaml --version 2026.11.6'"},
		{name: "context", opts: managementTargetOptions{contextName: "lab"}, want: "'katlctl node upgrade cp-1 --context lab --version 2026.11.6'"},
		{name: "endpoint", opts: managementTargetOptions{endpoint: "10.0.0.2"}, want: "'katlctl node upgrade cp-1 --endpoint 10.0.0.2 --version 2026.11.6'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBootMoveSource("2026.11.5", "2026.12.0", "2026.12.0", "cp-1", tc.opts.guidanceFlags())
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "or later") {
				t.Fatalf("checkBootMoveSource() = %v, want %s and no \"or later\"", err, tc.want)
			}
		})
	}
}
