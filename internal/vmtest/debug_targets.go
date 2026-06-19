package vmtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type DebugTargetReport struct {
	ResultPath     string
	Source         string
	Preserved      bool
	Reason         string
	DomainName     string
	LibvirtURI     string
	SerialLog      string
	ConsoleCommand string
	CleanupCommand string
	ShellMode      string
	VSock          VSockPlan
}

type debugTargetResult struct {
	DomainName string         `json:"domainName,omitempty"`
	LibvirtURI string         `json:"libvirtURI,omitempty"`
	Artifacts  debugArtifacts `json:"artifacts,omitempty"`
	Debug      *DebugMetadata `json:"debug,omitempty"`
	VSock      VSockPlan      `json:"vsock,omitempty"`
}

type debugArtifacts struct {
	RuntimeSerial string `json:"runtimeSerial,omitempty"`
	Serial        string `json:"serial,omitempty"`
}

func FindDebugResultFiles(target string) ([]string, error) {
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("target not found: %s", target)
		}
		return nil, err
	}
	if !info.IsDir() {
		return []string{target}, nil
	}
	var results []string
	err = filepath.WalkDir(target, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Name() == "result.json" {
			results = append(results, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(results)
	if len(results) == 0 {
		return nil, fmt.Errorf("no result.json files found under %s", target)
	}
	return results, nil
}

func LoadDebugTargetReports(resultPaths []string) ([]DebugTargetReport, error) {
	var reports []DebugTargetReport
	for _, resultPath := range resultPaths {
		resultReports, err := loadDebugTargetReports(resultPath)
		if err != nil {
			return nil, err
		}
		reports = append(reports, resultReports...)
	}
	return reports, nil
}

func loadDebugTargetReports(resultPath string) ([]DebugTargetReport, error) {
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return nil, err
	}
	var result debugTargetResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("%s: %w", resultPath, err)
	}
	if result.Debug != nil && len(result.Debug.Targets) > 0 {
		reports := make([]DebugTargetReport, 0, len(result.Debug.Targets))
		for _, target := range result.Debug.Targets {
			reports = append(reports, debugTargetReportFromTarget(resultPath, target))
		}
		return reports, nil
	}
	if strings.TrimSpace(result.DomainName) == "" {
		return nil, nil
	}
	shellMode := ""
	if result.Debug != nil && result.Debug.Shell {
		shellMode = "serial-root"
	}
	report := DebugTargetReport{
		ResultPath:     resultPath,
		Source:         "result",
		Preserved:      false,
		Reason:         "vmtest domain recorded in result; it may no longer be running",
		DomainName:     result.DomainName,
		LibvirtURI:     first(result.LibvirtURI, "qemu:///system"),
		SerialLog:      first(result.Artifacts.RuntimeSerial, result.Artifacts.Serial),
		CleanupCommand: cleanupCommandLine(resultPath),
		ShellMode:      shellMode,
		VSock:          result.VSock,
	}
	report.ConsoleCommand = consoleCommandLine(report.LibvirtURI, report.DomainName)
	return []DebugTargetReport{report}, nil
}

func debugTargetReportFromTarget(resultPath string, target DebugTarget) DebugTargetReport {
	report := DebugTargetReport{
		ResultPath:     resultPath,
		Source:         "debug-target",
		Preserved:      target.Preserved,
		Reason:         target.Reason,
		DomainName:     target.DomainName,
		LibvirtURI:     first(target.LibvirtURI, "qemu:///system"),
		SerialLog:      target.SerialLog,
		ConsoleCommand: target.ConsoleCommand,
		CleanupCommand: target.CleanupCommand,
		ShellMode:      target.ShellMode,
		VSock:          target.VSock,
	}
	if report.ConsoleCommand == "" {
		report.ConsoleCommand = consoleCommandLine(report.LibvirtURI, report.DomainName)
	}
	if report.CleanupCommand == "" {
		report.CleanupCommand = cleanupCommandLine(resultPath)
	}
	return report
}

func WriteDebugTargetReport(w io.Writer, reports []DebugTargetReport) error {
	if len(reports) == 0 {
		return errors.New("no vmtest domains recorded")
	}
	if _, err := fmt.Fprintln(w, "vmtest debug targets"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Serial tail is safe during a hung run. Console attaches to ttyS0 and can disrupt active harness serial capture."); err != nil {
		return err
	}
	for _, report := range reports {
		if strings.TrimSpace(report.DomainName) == "" {
			continue
		}
		lines := []string{
			"",
			"- result: " + report.ResultPath,
			"  domain: " + report.DomainName,
			"  libvirt URI: " + report.LibvirtURI,
		}
		if report.Reason != "" {
			lines = append(lines, "  reason: "+report.Reason)
		}
		lines = append(lines,
			"  source: "+report.Source,
			"  preserved: "+strconv.FormatBool(report.Preserved),
		)
		if report.SerialLog != "" {
			lines = append(lines, "  serial tail: "+shellCommand("tail", "-f", report.SerialLog))
		}
		lines = append(lines,
			"  domstate: "+shellCommand("virsh", "-c", report.LibvirtURI, "domstate", report.DomainName),
			"  console (invasive): "+report.ConsoleCommand,
		)
		if report.VSock.GuestCID != 0 && report.VSock.Port != 0 {
			lines = append(lines, fmt.Sprintf("  vsock: cid=%d port=%d", report.VSock.GuestCID, report.VSock.Port))
		}
		if report.ShellMode != "" {
			lines = append(lines, "  shell: "+report.ShellMode)
		}
		lines = append(lines, "  cleanup after preservation: "+report.CleanupCommand)
		for _, line := range lines {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}
	return nil
}
