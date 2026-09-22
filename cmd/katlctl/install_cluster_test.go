package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/installer/manifest"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestInstallApplyCluster(t *testing.T) {
	for _, state := range []string{"busy", "unavailable", "reject"} {
		t.Run(state, func(t *testing.T) {
			sourcePath := writeClusterConfig(t)
			source := configBundleSource()
			servers := make([]*handoff.HandoffServer, 3)
			for i := range servers {
				servers[i] = handoff.NewHandoffServerWithDefaultImage(nil, manifest.KatlosImage{
					LocalRef: "images/katlos-test-x86_64.squashfs", SHA256: strings.Repeat("a", 64), SizeBytes: 1,
					Version: "test", Architecture: "x86_64", RuntimeInterface: "katl-runtime-1", Role: "install",
				})
				handler := servers[i].Handler()
				if i == 1 {
					handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if state == "reject" {
							if r.Method == http.MethodGet {
								servers[i].Handler().ServeHTTP(w, r)
							} else {
								http.Error(w, "installation refused", http.StatusConflict)
							}
							return
						}
						if r.Method != http.MethodGet {
							t.Error("submitted config to a non-waiting node")
						}
						if state == "unavailable" {
							http.Error(w, "unavailable", http.StatusServiceUnavailable)
							return
						}
						_ = json.NewEncoder(w).Encode(handoff.HandoffStatus{State: handoff.HandoffAccepted, InstallStatus: installstatus.New(installstatus.StateRunning, time.Now())})
					})
				}
				ts := httptest.NewServer(handler)
				t.Cleanup(ts.Close)
				address := strings.TrimPrefix(ts.URL, "http://")
				if i == 0 {
					source = strings.Replace(source, "10.0.0.11", address, 1)
					source = strings.Replace(source, "      install:\n", "      kubernetes:\n        address: 192.0.2.11\n      install:\n", 1)
				} else {
					source += fmt.Sprintf("    - name: worker-%d\n      management:\n        address: %s\n      kubernetes:\n        address: 192.0.2.%d\n      install:\n        systemDisk:\n          byID: /dev/disk/by-id/worker-%d\n", i, address, 11+i, i)
				}
			}
			if err := os.WriteFile(sourcePath, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}

			for attempt := 0; attempt < 2; attempt++ {
				var stdout, stderr bytes.Buffer
				err := run(context.Background(), []string{"install", "apply", "--config", sourcePath, "--no-wait", "--output", "json"}, &stdout, &stderr)
				if state == "reject" {
					if err == nil || !strings.Contains(err.Error(), "node worker-1:") || !strings.Contains(err.Error(), "installation refused") {
						t.Fatalf("apply error = %v", err)
					}
				} else if err != nil {
					t.Fatalf("apply: %v\n%s", err, stderr.String())
				}
				var reports []struct {
					Skipped    bool   `json:"skipped"`
					SkipReason string `json:"skipReason"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &reports); err != nil {
					t.Fatalf("reports: %v\n%s", err, stdout.String())
				}
				wantReports := 3
				if state == "reject" {
					wantReports = 2
				}
				if len(reports) != wantReports {
					t.Fatalf("reports = %d, want %d", len(reports), wantReports)
				}
				if state != "reject" && (!reports[1].Skipped || reports[1].SkipReason == "") {
					t.Fatalf("missing skip report: %+v", reports[1])
				}
				for i, name := range []string{"cp-1", "", "worker-2"} {
					payload := servers[i].Bundle()
					if payload.NodeName != name {
						t.Fatalf("node %d received bundle for %q, want %q", i, payload.NodeName, name)
					}
				}
				if state != "reject" && !strings.Contains(stderr.String(), "skipping node worker-1") {
					t.Fatalf("missing skip: %s", stderr.String())
				}
			}
		})
	}
}

func TestInstallClusterExtensionTargets(t *testing.T) {
	sourcePath := writeClusterConfig(t)
	source := strings.Replace(configBundleSource(), "  defaults:\n", "  defaults:\n    systemExtensions:\n      - release: registry.example/unavailable\n", 1)
	for i, version := range []string{"2026.9.1", "2026.9.2", "busy"} {
		image := manifest.KatlosImage{
			LocalRef:         "images/katlos.squashfs",
			SHA256:           strings.Repeat("a", 64),
			SizeBytes:        1,
			Version:          version,
			Architecture:     "x86_64",
			RuntimeInterface: "katl-runtime-1",
			Role:             "install",
		}
		if version != "busy" {
			image.ExtensionRelease = &extensionrelease.Manifest{
				Target: extensionrelease.Target{
					Version:          version,
					Architecture:     "x86_64",
					Flavour:          "standard",
					RuntimeInterface: "katl-runtime-1",
					Kernel: kernelmodule.Target{
						Release:       "6.12.0",
						RuntimeSHA256: strings.Repeat("b", 64),
					},
				},
				Extensions: map[string]string{},
			}
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/v1/status" {
				t.Errorf("unexpected request before successful preparation: %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}
			state := handoff.HandoffWaiting
			if version == "busy" {
				state = handoff.HandoffAccepted
			}
			_ = json.NewEncoder(w).Encode(handoff.HandoffStatus{
				State:         state,
				Image:         image,
				InstallStatus: installstatus.New(installstatus.StateRunning, time.Now()),
			})
		}))
		t.Cleanup(server.Close)
		address := strings.TrimPrefix(server.URL, "http://")
		if i == 0 {
			source = strings.Replace(source, "10.0.0.11", address, 1)
		} else {
			source += fmt.Sprintf("    - name: worker-%d\n      management:\n        address: %s\n      install:\n        systemDisk:\n          byID: /dev/disk/by-id/worker-%d\n", i, address, i)
		}
	}
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"install", "apply", "--config", sourcePath, "--no-wait"}, &stdout, &stderr)
	for _, version := range []string{"2026.9.1", "2026.9.2"} {
		if err == nil || !strings.Contains(err.Error(), "unavailable for KatlOS "+version) {
			t.Fatalf("missing installer-specific target %s: %v\n%s", version, err, stderr.String())
		}
	}
	if strings.Contains(err.Error(), "node worker-2:") || !strings.Contains(stderr.String(), "skipping node worker-2") {
		t.Fatalf("busy node was not skipped before extension preparation: %v\n%s", err, stderr.String())
	}
}
