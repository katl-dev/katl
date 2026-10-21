package vmtest

import (
	"context"
	"strings"
	"testing"

	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

func TestFirstInstallHealth(t *testing.T) {
	const selection = `{"recordType":"katl.boot.selection","recordVersion":1,"payload":{
		"apiVersion":"katl.dev/v1alpha1","kind":"BootSelection",
		"defaultGenerationID":"0","bootedGenerationID":"0",
		"pendingHealthValidation":false,"recoveryRequired":false,
		"persistentDefaultPromotion":"promoted","updatedAt":"2026-10-21T12:00:00Z"}}`
	const status = `{"apiVersion":"katl.dev/v1alpha1","kind":"InstallToBootstrapStatus",
		"finalHandoff":"waiting-for-cluster-bootstrap","updatedAt":"2026-10-21T12:00:00Z"}`

	for _, tc := range []struct {
		name      string
		inactive  string
		health    string
		selection string
		status    string
		wantError string
	}{
		{name: "healthy"},
		{name: "agent before OS ready", inactive: "katl-boot-complete.target", wantError: "katl-boot-complete.target"},
		{name: "SSH unavailable", inactive: "sshd.service", wantError: "sshd.service"},
		{name: "management unavailable", inactive: "katlc-agent.service", wantError: "katlc-agent.service"},
		{name: "failed health", health: "exit-code", wantError: "boot-health result"},
		{name: "handoff not ready", status: strings.ReplaceAll(status, "waiting-for-cluster-bootstrap", "runtime-failed-needs-repair"), wantError: "handoff"},
		{name: "wrong generation", selection: strings.ReplaceAll(selection, `"defaultGenerationID":"0"`, `"defaultGenerationID":"1"`), wantError: "not healthy and promoted"},
		{name: "recovery required", selection: strings.ReplaceAll(selection, `"recoveryRequired":false`, `"recoveryRequired":true`), wantError: "not healthy and promoted"},
		{name: "promotion pending", selection: strings.ReplaceAll(selection, `"persistentDefaultPromotion":"promoted"`, `"persistentDefaultPromotion":"pending"`), wantError: "not healthy and promoted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &firstInstallHealthClient{
				inactive: tc.inactive, health: first(tc.health, "success"),
				selection: first(tc.selection, selection), status: first(tc.status, status),
			}
			guest := NewGuestControl(guestResult(t), client)

			err := checkFirstInstallHealth(context.Background(), guest)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

type firstInstallHealthClient struct {
	fakeGuestClient
	inactive, health, selection, status string
}

func (c *firstInstallHealthClient) RunCommand(_ context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	if req.Argv[1] == "show" {
		return &vmtestpb.CommandResult{Stdout: []byte(c.health + "\n")}, nil
	}
	if req.Argv[len(req.Argv)-1] == c.inactive {
		return &vmtestpb.CommandResult{ExitStatus: 3}, nil
	}
	return &vmtestpb.CommandResult{}, nil
}

func (c *firstInstallHealthClient) ReadFile(_ context.Context, req *vmtestpb.ReadFileRequest) (*vmtestpb.FileResult, error) {
	content := c.status
	if req.Path == "/var/lib/katl/boot/selection.json" {
		content = c.selection
	}
	return &vmtestpb.FileResult{Content: []byte(content)}, nil
}
