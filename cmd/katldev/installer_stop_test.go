package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInstallerStop(t *testing.T) {
	for _, test := range []struct {
		name    string
		initial string
		force   bool
		want    []string
	}{
		{name: "graceful", initial: "running", want: []string{"shutdown", "katl-dev-test", "--mode", "acpi"}},
		{name: "already off", initial: "shut off"},
		{name: "already shutting down", initial: "in shutdown"},
		{name: "force", initial: "running", force: true, want: []string{"destroy", "katl-dev-test"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := test.initial
			var observed string
			var stdout bytes.Buffer
			var mutation []string
			manager := installerManager{repoRoot: t.TempDir(), stdout: &stdout, stderr: io.Discard}
			writeStopState(t, manager)
			manager.runVirsh = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte("katl-dev-test\n"), nil
				case "metadata":
					return []byte(`<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">katl/katldev-installer</vmtest>`), nil
				case "domstate":
					observed = state
					if state == "in shutdown" {
						state = "shut off"
					}
					return []byte(observed), nil
				case "shutdown", "destroy":
					mutation = append([]string(nil), args...)
					state = "in shutdown"
					if args[0] == "destroy" {
						state = "shut off"
					}
					return nil, nil
				default:
					return nil, fmt.Errorf("unexpected command: %v", args)
				}
			}

			if err := manager.stop(context.Background(), 5*time.Second, test.force); err != nil {
				t.Fatal(err)
			}

			if state != "shut off" || (!test.force && observed != "shut off") {
				t.Fatalf("reported success without confirming poweroff: state=%s observed=%s", state, observed)
			}
			if !reflect.DeepEqual(mutation, test.want) {
				t.Fatalf("power action = %v, want %v", mutation, test.want)
			}
			if stdout.Len() == 0 {
				t.Fatal("missing completion message")
			}
		})
	}
}

func TestInstallerStopFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		state   string
		owner   string
		force   bool
		failure error
		want    string
	}{
		{name: "timeout", state: "running", owner: installerDomainMetadata, want: "--timeout"},
		{name: "paused", state: "paused", owner: installerDomainMetadata, want: "resume"},
		{name: "unowned", state: "running", owner: "katl/vmtest", want: "refusing"},
		{name: "unowned force", state: "running", owner: "katl/vmtest", force: true, want: "refusing"},
		{name: "request failed", state: "running", owner: installerDomainMetadata, failure: errors.New("ACPI unavailable"), want: "ACPI unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			manager := installerManager{repoRoot: t.TempDir(), stdout: &stdout, stderr: io.Discard}
			writeStopState(t, manager)
			manager.runVirsh = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte("katl-dev-test\n"), nil
				case "metadata":
					return []byte(`<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">` + test.owner + `</vmtest>`), nil
				case "domstate":
					return []byte(test.state), nil
				case "shutdown":
					if test.state != "running" || test.owner != installerDomainMetadata {
						t.Fatalf("unexpected shutdown: state=%s owner=%s", test.state, test.owner)
					}
					return nil, test.failure
				default:
					t.Fatalf("unsafe power action: %v", args)
					return nil, nil
				}
			}

			err := manager.stop(context.Background(), 10*time.Millisecond, test.force)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("stop error = %v, want %q", err, test.want)
			}
			if test.name == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost deadline error: %v", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("reported completion after failed shutdown: %s", &stdout)
			}
		})
	}
}

func writeStopState(t *testing.T, manager installerManager) {
	t.Helper()
	if err := manager.writeState(installerState{
		APIVersion: installerStateAPIVersion,
		Kind:       installerStateKind,
		RepoRoot:   manager.repoRoot,
		DomainName: "katl-dev-test",
		LibvirtURI: "qemu:///system",
	}); err != nil {
		t.Fatal(err)
	}
}
