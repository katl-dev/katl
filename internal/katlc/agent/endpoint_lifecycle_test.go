package agent

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
)

func TestManagedEndpointLifecycleFollowsGeneratedEnablement(t *testing.T) {
	root := t.TempDir()
	var calls [][]string
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		calls = append(calls, append([]string(nil), argv...))
		return ToolResult{}
	}

	paused, err := pauseManagedEndpoint(context.Background(), root, run)
	if err != nil || paused {
		t.Fatalf("pause without managed endpoint = %v, %v", paused, err)
	}
	if err := resumeManagedEndpoint(context.Background(), root, run); err != nil {
		t.Fatalf("resume without managed endpoint: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("commands without managed endpoint = %#v", calls)
	}

	writeTestFile(t, filepath.Join(root, bgpapivip.AdvertisementEnabledPath), "enabled\n")
	paused, err = pauseManagedEndpoint(context.Background(), root, run)
	if err != nil || !paused {
		t.Fatalf("pause managed endpoint = %v, %v", paused, err)
	}
	if err := resumeManagedEndpoint(context.Background(), root, run); err != nil {
		t.Fatalf("resume managed endpoint: %v", err)
	}
	want := [][]string{
		{"systemctl", "stop", endpointAdvertiserUnit},
		{endpointAdvertiserCommand, "withdraw"},
		{"systemctl", "start", endpointAdvertiserUnit},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("endpoint lifecycle commands = %#v, want %#v", calls, want)
	}
}

func TestManagedEndpointPauseFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, bgpapivip.AdvertisementEnabledPath), "enabled\n")
	run := func(_ context.Context, _ []string, _ func(int)) ToolResult {
		return ToolResult{Err: errors.New("service failed"), ExitStatus: 1}
	}

	paused, err := pauseManagedEndpoint(context.Background(), root, run)
	if err == nil || paused {
		t.Fatalf("pauseManagedEndpoint() = %v, %v", paused, err)
	}
}
