package vmtest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindDebugResultFiles(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "b", "result.json"),
		filepath.Join(root, "a", "nested", "result.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", path, err)
		}
		if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	results, err := FindDebugResultFiles(root)
	if err != nil {
		t.Fatalf("FindDebugResultFiles() error = %v", err)
	}
	want := []string{
		filepath.Join(root, "a", "nested", "result.json"),
		filepath.Join(root, "b", "result.json"),
	}
	if strings.Join(results, "\n") != strings.Join(want, "\n") {
		t.Fatalf("results = %#v, want %#v", results, want)
	}
}

func TestLoadDebugTargetReportsUsesPreservedTargets(t *testing.T) {
	result := filepath.Join(t.TempDir(), "result.json")
	data := []byte(`{
  "debug": {
    "targets": [{
      "preserved": true,
      "reason": "debug-on-failure preserved live libvirt domain",
      "domainName": "katl-debug-run",
      "libvirtURI": "qemu:///system",
      "serialLog": "/tmp/katl-debug/serial.log",
      "consoleCommand": "'virsh' '-c' 'qemu:///system' 'console' 'katl-debug-run' '--force'",
      "cleanupCommand": "'scripts/vmtest-clean' '/tmp/katl-debug/result.json'",
      "shellMode": "serial-root",
      "vsock": {"enabled": true, "guestCid": 2048, "port": 10240}
    }]
  }
}`)
	if err := os.WriteFile(result, data, 0o644); err != nil {
		t.Fatalf("WriteFile(result) error = %v", err)
	}
	reports, err := LoadDebugTargetReports([]string{result})
	if err != nil {
		t.Fatalf("LoadDebugTargetReports() error = %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("reports = %#v", reports)
	}
	report := reports[0]
	if !report.Preserved || report.Source != "debug-target" || report.DomainName != "katl-debug-run" || report.VSock.GuestCID != 2048 {
		t.Fatalf("report = %#v", report)
	}
	var out bytes.Buffer
	if err := WriteDebugTargetReport(&out, reports); err != nil {
		t.Fatalf("WriteDebugTargetReport() error = %v", err)
	}
	for _, want := range []string{
		"domain: katl-debug-run",
		"source: debug-target",
		"preserved: true",
		"serial tail: 'tail' '-f' '/tmp/katl-debug/serial.log'",
		"vsock: cid=2048 port=10240",
		"cleanup after preservation: 'scripts/vmtest-clean' '/tmp/katl-debug/result.json'",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestLoadDebugTargetReportsFallsBackToResultDomain(t *testing.T) {
	result := filepath.Join(t.TempDir(), "result.json")
	data := []byte(`{
  "domainName": "katl-live-debug",
  "artifacts": {"runtimeSerial": "/tmp/katl-debug/runtime-serial.log"},
  "vsock": {"enabled": true, "guestCid": 2048, "port": 10240},
  "debug": {"onFailure": true, "shell": true}
}`)
	if err := os.WriteFile(result, data, 0o644); err != nil {
		t.Fatalf("WriteFile(result) error = %v", err)
	}
	reports, err := LoadDebugTargetReports([]string{result})
	if err != nil {
		t.Fatalf("LoadDebugTargetReports() error = %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("reports = %#v", reports)
	}
	report := reports[0]
	if report.Preserved || report.Source != "result" || report.LibvirtURI != "qemu:///system" || report.ShellMode != "serial-root" {
		t.Fatalf("report = %#v", report)
	}
	if report.ConsoleCommand != "'virsh' '-c' 'qemu:///system' 'console' 'katl-live-debug' '--force'" {
		t.Fatalf("console command = %q", report.ConsoleCommand)
	}
}
