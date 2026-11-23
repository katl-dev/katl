package agent

import (
	"context"
	"strings"
	"testing"
)

func TestBootRootMounter(t *testing.T) {
	notMounted := func(argv []string) ToolResult {
		if argv[0] == "/usr/bin/mountpoint" {
			return ToolResult{ExitStatus: 32}
		}
		return ToolResult{}
	}
	tests := []struct {
		name      string
		installed bool
		result    func([]string) ToolResult
		wantErr   string
		wantCalls []string
	}{
		{
			name:      "installed node starts the generated unit",
			installed: true,
			result:    notMounted,
			wantCalls: []string{"/usr/bin/mountpoint -q /efi", "/usr/bin/systemctl start efi.mount"},
		},
		{
			// A missing or failed efi.mount must not fall back to a label that
			// another disk could carry.
			name:      "installed node reports a failed unit",
			installed: true,
			result: func(argv []string) ToolResult {
				if argv[0] == "/usr/bin/systemctl" {
					return ToolResult{ExitStatus: 5, Stderr: []byte("Unit efi.mount not found.")}
				}
				return notMounted(argv)
			},
			wantErr:   "start efi.mount",
			wantCalls: []string{"/usr/bin/mountpoint -q /efi", "/usr/bin/systemctl start efi.mount"},
		},
		{
			name:      "direct runtime uses its development ESP image",
			result:    notMounted,
			wantCalls: []string{"/usr/bin/mountpoint -q /efi", "/usr/bin/mount /dev/disk/by-label/KATLEFI /efi"},
		},
		{
			name:      "mounted ESP is left alone",
			installed: true,
			result:    func([]string) ToolResult { return ToolResult{} },
			wantCalls: []string{"/usr/bin/mountpoint -q /efi"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			mounter := bootRootMounter{installed: test.installed, run: func(_ context.Context, argv []string) ToolResult {
				calls = append(calls, strings.Join(argv, " "))
				return test.result(argv)
			}}

			err := mounter.mount(context.Background(), "/efi")

			if test.wantErr == "" && err != nil || test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("mount() error = %v, want %q", err, test.wantErr)
			}
			if strings.Join(calls, "; ") != strings.Join(test.wantCalls, "; ") {
				t.Fatalf("calls = %q, want %q", calls, test.wantCalls)
			}
		})
	}
}

func TestInstalledCommandLine(t *testing.T) {
	for cmdline, want := range map[string]bool{
		"root=PARTUUID=1234 rootfstype=squashfs ro":    true,
		"BOOT_IMAGE=x root=PARTUUID=1234 ro":           true,
		"root=/dev/vda rootfstype=squashfs ro":         false,
		"katl.root=PARTUUID=1234 root=/dev/vda ro":     false,
		"root=gpt-auto systemd.mount-extra=PARTUUID=x": false,
	} {
		if got := installedCommandLine(cmdline); got != want {
			t.Errorf("installedCommandLine(%q) = %v, want %v", cmdline, got, want)
		}
	}
}
