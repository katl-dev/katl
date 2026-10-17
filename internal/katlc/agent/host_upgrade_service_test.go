package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPreparationReleaseRequiresInactiveUnit(t *testing.T) {
	for _, test := range []struct {
		name, properties               string
		stopFails, showFails, retained bool
	}{
		{name: "stopped", properties: "LoadState=loaded\nActiveState=inactive\n"},
		{name: "collected", properties: "LoadState=not-found\nActiveState=inactive\n", stopFails: true},
		{name: "stop failed but inactive", properties: "ActiveState=inactive\nLoadState=loaded\n", stopFails: true},
		{name: "running", properties: "LoadState=loaded\nActiveState=active\n", retained: true},
		{name: "stopping", properties: "LoadState=loaded\nActiveState=deactivating\n", retained: true},
		{name: "missing property", properties: "ActiveState=inactive\n", retained: true},
		{name: "duplicate property", properties: "LoadState=loaded\nActiveState=active\nActiveState=inactive\n", retained: true},
		{name: "inspection failed", showFails: true, retained: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			showed := false
			cause := errors.New("manager unavailable")
			run := func(ctx context.Context, argv []string, _ func(int)) ToolResult {
				if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
					t.Fatal("service cleanup lacks an independent bounded context")
				}
				if argv[0] != "systemctl" || argv[len(argv)-1] != "prepare-test.service" {
					t.Fatalf("unexpected service command %v", argv)
				}
				if argv[1] == "stop" {
					if test.stopFails {
						return ToolResult{ExitStatus: 1, Err: cause}
					}
					return ToolResult{}
				}
				showed = true
				if test.showFails {
					return ToolResult{ExitStatus: 1, Err: cause}
				}
				return ToolResult{Stdout: []byte(test.properties)}
			}
			err := stopUpgradePreparation("prepare-test.service", run)
			if !showed || (err != nil) != test.retained || test.showFails && !errors.Is(err, cause) {
				t.Fatalf("authoritative service release = %v, inspected = %v", err, showed)
			}
		})
	}
}

func TestStalePreparationRetainsBackingUntilStopped(t *testing.T) {
	root := t.TempDir()
	base := hostUpgradeArtifactRoot(root)
	work := filepath.Join(base, "prepare-crash")
	backing := filepath.Join(base, "downloads/target.squashfs")
	for _, path := range []string{work, filepath.Dir(backing), filepath.Join(base, "acquisitions/image-crash/mount")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(backing, []byte("target image"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if argv[0] != "systemctl" {
			t.Fatalf("attempted image release while target namespace remains active: %v", argv)
		}
		if argv[1] == "show" {
			return ToolResult{Stdout: []byte("LoadState=loaded\nActiveState=active\n")}
		}
		return ToolResult{ExitStatus: 1, Err: errors.New("stop failed")}
	}
	err := cleanupHostUpgradeStorage(context.Background(), root, "", run, time.Now())
	if err == nil || !strings.Contains(err.Error(), "not proven inactive") {
		t.Fatalf("uncertain service was treated as released: %v", err)
	}
	if data, err := os.ReadFile(backing); err != nil || string(data) != "target image" {
		t.Fatalf("active target lost backing: %q, %v", data, err)
	}
	prepared := preparedUpgrade{work: work, closeErr: errors.New("target still owns source")}
	if err := prepared.close(); err == nil {
		t.Fatal("uncertain preparation workspace reported successful release")
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("uncertain preparation workspace was deleted: %v", err)
	}
}
