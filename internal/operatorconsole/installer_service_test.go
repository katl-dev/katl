package operatorconsole

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestParseInstallerService(t *testing.T) {
	got := parseInstallerService([]byte("ActiveState=failed\nResult=exit-code\nStatusText=installer stopped: no disk matches by-id=x\n"))
	want := InstallerService{ActiveState: "failed", Result: "exit-code", Status: "installer stopped: no disk matches by-id=x"}
	if got != want {
		t.Fatalf("parseInstallerService() = %#v, want %#v", got, want)
	}
}

func TestRenderInstallerService(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		service InstallerService
		record  func(*installstatus.Record)
		handoff string
		want    []string
		absent  []string
	}{
		{
			name:    "waiting for link",
			service: InstallerService{ActiveState: "activating", Status: "waiting for a network link; no interface has carrier, connect a cable or check the switch port (12s elapsed)"},
			want:    []string{"State:Startinginstaller", "Progress:waitingforanetworklink;nointerfacehascarrier,connectacableorchecktheswitchport(12selapsed)"},
		},
		{
			name:    "waiting for address",
			service: InstallerService{ActiveState: "activating", Status: "waiting for a DHCP address on enp1s0 (3s elapsed)"},
			want:    []string{"State:Startinginstaller", "Progress:waitingforaDHCPaddressonenp1s0(3selapsed)"},
		},
		{
			name:    "ready at endpoint",
			service: InstallerService{ActiveState: "activating", Status: "waiting for configuration at http://192.0.2.10:8080"},
			record:  func(r *installstatus.Record) { r.State = installstatus.StateWaitingForConfig },
			handoff: "http://192.0.2.10:8080",
			want:    []string{"State:Waitingforconfiguration", "Configure:http://192.0.2.10:8080/v1/config-bundle", "Run:katlctlconfiginitcluster.yaml--installer192.0.2.10"},
			absent:  []string{"Progress:"},
		},
		{
			name:    "automatic install waiting for address",
			service: InstallerService{ActiveState: "activating", Status: "waiting for a DHCP address on enp1s0 (3s elapsed)"},
			record:  func(r *installstatus.Record) { r.InputMode = installstatus.InputModePXEPreseed },
			want:    []string{"State:Preparingautomaticinstall", "Progress:waitingforaDHCPaddressonenp1s0(3selapsed)"},
			absent:  []string{"State:Installing", "Diskchanges"},
		},
		{
			name:    "automatic install waiting for seed device",
			service: InstallerService{ActiveState: "activating", Status: "waiting for the seed device (9s elapsed)"},
			record:  func(r *installstatus.Record) { r.InputMode = installstatus.InputModePXEPreseed },
			want:    []string{"State:Preparingautomaticinstall", "Progress:waitingfortheseeddevice(9selapsed)"},
			absent:  []string{"State:Installing", "Diskchanges"},
		},
		{
			name:    "automatic install with announced handoff",
			service: InstallerService{ActiveState: "activating", Status: "fetching the install manifest"},
			record:  func(r *installstatus.Record) { r.InputMode = installstatus.InputModePXEPreseed },
			handoff: "http://192.0.2.10:8080",
			want:    []string{"State:Preparingautomaticinstall", "Progress:fetchingtheinstallmanifest"},
			absent:  []string{"State:Installing", "State:Waitingforconfiguration"},
		},
		{
			name:    "automatic install running",
			service: InstallerService{ActiveState: "activating", Status: "install step InstallRootSlot"},
			record: func(r *installstatus.Record) {
				r.InputMode = installstatus.InputModePXEPreseed
				r.CurrentStep = "InstallRootSlot"
				r.Progress = &installstatus.Progress{Operation: "Writing root image", StartedAt: now.Add(-30 * time.Second)}
			},
			want:   []string{"State:Installing", "Progress:Writingrootimage·30selapsed", "Diskchanges:notstarted"},
			absent: []string{"Preparingautomaticinstall", "installstepInstallRootSlot"},
		},
		{
			name:    "installing",
			service: InstallerService{ActiveState: "activating", Status: "install step InstallRootSlot"},
			record: func(r *installstatus.Record) {
				r.CurrentStep = "InstallRootSlot"
				r.Progress = &installstatus.Progress{Operation: "Writing root image", StartedAt: now.Add(-30 * time.Second)}
			},
			want:   []string{"State:Installing", "Progress:Writingrootimage·30selapsed"},
			absent: []string{"installstepInstallRootSlot"},
		},
		{
			name:    "complete and rebooting",
			service: InstallerService{ActiveState: "activating", Status: "installation complete; waiting for scheduled reboot"},
			record:  func(r *installstatus.Record) { r.State = installstatus.StateRebootRequested },
			want:    []string{"State:Installationcomplete;rebooting"},
			absent:  []string{"Progress:", "Error:"},
		},
		{
			name:    "install failed before disk change",
			service: InstallerService{ActiveState: "activating", Status: "install failed before disk mutation; waiting for corrected configuration"},
			record: func(r *installstatus.Record) {
				r.State = installstatus.StateFailedBeforeMutation
				r.LastError = "no disk matches system disk selector"
				r.RetryHint = "correct the failure, then run katlctl install apply --config cluster.yaml --endpoint http://192.0.2.10:8080"
			},
			handoff: "http://192.0.2.10:8080",
			want: []string{
				"State:Installationfailed;diskunchanged",
				"Configure:http://192.0.2.10:8080/v1/config-bundle",
				"Run:katlctlconfiginitcluster.yaml--installer192.0.2.10",
				"Error:nodiskmatchessystemdiskselector",
				"Nextaction:correctthefailure,thenrunkatlctlinstallapply--configcluster.yaml--endpointhttp://192.0.2.10:8080",
			},
		},
		{
			name:    "installer exited",
			service: InstallerService{ActiveState: "failed", Result: "exit-code", Status: "installer stopped: download install manifest: connection refused"},
			handoff: "http://192.0.2.10:8080",
			want: []string{
				"State:Installerstopped",
				"Error:installerstopped:downloadinstallmanifest:connectionrefused",
				"Nextaction:fixthecauseabove,thenreboottorestarttheinstaller",
				"journalctl-ukatlos-install",
			},
			absent: []string{"State:Startinginstaller", "Configure:", "Run:"},
		},
		{
			name:    "installer killed without status",
			service: InstallerService{ActiveState: "failed", Result: "signal"},
			want:    []string{"State:Installerstopped", "Error:katlos-installexitedunexpectedly(signal)"},
		},
		{
			name:    "installer exited after recorded failure keeps the recorded remedy",
			service: InstallerService{ActiveState: "failed", Result: "exit-code", Status: "installer stopped: install failed"},
			record: func(r *installstatus.Record) {
				r.State = installstatus.StateFailedAfterMutation
				r.LastError = "write root slot: input/output error"
				r.RetryHint = "replace the disk and reinstall"
			},
			want:   []string{"State:Installationfailed;repairrequired", "Error:writerootslot:input/outputerror", "Nextaction:replacethediskandreinstall"},
			absent: []string{"journalctl"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			statusPath := filepath.Join(dir, "status.json")
			if test.record != nil {
				record := installstatus.New(installstatus.StateRunning, now)
				test.record(&record)
				if err := installstatus.WriteFile(statusPath, record); err != nil {
					t.Fatal(err)
				}
			}
			handoffPath := filepath.Join(dir, "handoff.json")
			if test.handoff != "" {
				if err := WriteHandoff(handoffPath, test.handoff); err != nil {
					t.Fatal(err)
				}
			}
			collector := Collector{
				Mode:             ModeInstaller,
				Root:             dir,
				StatusPath:       statusPath,
				HandoffPath:      handoffPath,
				Interfaces:       func() ([]net.Interface, error) { return nil, nil },
				InstallerService: func(context.Context) (InstallerService, error) { return test.service, nil },
				Now:              func() time.Time { return now },
			}
			var snapshot Snapshot
			collector.Collect(&snapshot)
			got := string(renderDashboard(&snapshot, nil, 100, 24, false))
			for _, want := range test.want {
				if !containsIgnoringLayout(got, want) {
					t.Errorf("render missing %q:\n%s", want, got)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(strings.ReplaceAll(got, " ", ""), absent) {
					t.Errorf("render contains %q:\n%s", absent, got)
				}
			}
		})
	}
}
