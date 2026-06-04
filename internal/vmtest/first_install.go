package vmtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/zariel/katl/internal/installer/handoff"
)

type FirstInstallConfig struct {
	Installer       InstallerBootConfig
	Runtime         InstalledRuntimeConfig
	Manifest        []byte
	ManifestPath    string
	HandoffToken    string
	HandoffURL      string
	TargetDisk      DiskFixture
	DiskRunner      DiskRunner
	InstallerRunner VMRunner
	RuntimeRunner   VMRunner
}

type handoffLog struct {
	URL          string `json:"url"`
	Token        string `json:"token,omitempty"`
	ManifestPath string `json:"manifestPath"`
	Announcement string `json:"announcement,omitempty"`
	StatusCode   int    `json:"statusCode,omitempty"`
	Body         string `json:"body,omitempty"`
}

func RunFirstInstall(ctx context.Context, runner Runner, scenario Scenario, config FirstInstallConfig) (Result, error) {
	scenario = withTarget(scenario, config.TargetDisk)
	result, err := runner.Plan(scenario)
	if err != nil {
		return Result{}, err
	}
	result.start(runner.time())
	if err := CreateDisks(ctx, diskExec(config.DiskRunner), result.Disks); err != nil {
		return failFirst(runner, scenario, result, "prepare-fixtures", err)
	}
	manifest, err := loadManifest(config)
	if err != nil {
		return failFirst(runner, scenario, result, "install-manifest", err)
	}
	if err := writeManifest(result, manifest); err != nil {
		return failFirst(runner, scenario, result, "install-manifest", err)
	}

	result = BootInstaller(ctx, result, config.Installer, config.InstallerRunner)
	if err := copyArtifact(result.Artifacts.QEMUCommand, result.Artifacts.InstallerQEMUCommand); err != nil {
		return failFirst(runner, scenario, result, "installer", err)
	}
	if result.Status != StatusPassed {
		if err := runner.Write(scenario, result); err != nil {
			return result, err
		}
		return result, nil
	}
	if err := deliverHandoff(ctx, result, config, manifest); err != nil {
		return failFirst(runner, scenario, result, "local-handoff", err)
	}
	now := runner.time()
	result.addPhase("local-handoff", StatusPassed, "", now, now)

	runtime, err := runtimeConfig(result, config.Runtime)
	if err != nil {
		return failFirst(runner, scenario, result, "runtime", err)
	}
	disks := result.Disks
	bootResult := result
	bootResult.Disks = nil
	result = RunInstalledRuntime(ctx, bootResult, runtime, config.RuntimeRunner)
	result.Disks = disks
	if err := copyArtifact(result.Artifacts.QEMUCommand, result.Artifacts.RuntimeQEMUCommand); err != nil {
		return failFirst(runner, scenario, result, "runtime", err)
	}
	if result.Status == StatusPassed {
		if err := CleanupDisks(result); err != nil {
			return failFirst(runner, scenario, result, "cleanup", err)
		}
	}
	if err := runner.Write(scenario, result); err != nil {
		return result, err
	}
	return result, nil
}

func withTarget(scenario Scenario, target DiskFixture) Scenario {
	if len(scenario.Disks) > 0 {
		return scenario
	}
	if target.Name == "" {
		target = TargetDisk("root", string(DiskQCOW2), "20G")
	}
	scenario.Disks = []DiskFixture{target}
	return scenario
}

func diskExec(runner DiskRunner) DiskRunner {
	if runner != nil {
		return runner
	}
	return ExecDiskRunner{}
}

func loadManifest(config FirstInstallConfig) ([]byte, error) {
	if len(config.Manifest) > 0 {
		return append([]byte(nil), config.Manifest...), nil
	}
	if config.ManifestPath == "" {
		return nil, errors.New("install manifest is required")
	}
	data, err := os.ReadFile(config.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("read install manifest: %w", err)
	}
	return data, nil
}

func writeManifest(result Result, manifest []byte) error {
	if err := os.MkdirAll(filepath.Dir(result.Artifacts.InstallManifest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(result.Artifacts.InstallManifest, manifest, 0o600)
}

func deliverHandoff(ctx context.Context, result Result, config FirstInstallConfig, manifest []byte) error {
	url := config.HandoffURL
	token := config.HandoffToken
	var announcement string
	var handler http.Handler
	if url == "" {
		server, err := handoff.NewHandoffServer(token, nil)
		if err != nil {
			return err
		}
		handler = server.Handler()
		url = "http://vmtest.local/v1/install"
		token = server.Token()
		announcement = server.Announcement("http://vmtest.local")
	}
	if token == "" {
		return errors.New("handoff token is required")
	}

	request := handoffLog{
		URL:          url,
		Token:        token,
		ManifestPath: result.Artifacts.InstallManifest,
		Announcement: announcement,
	}
	if err := writeJSON(result.Artifacts.HandoffRequest, request); err != nil {
		return err
	}

	if handler != nil {
		status, body, err := postLocal(ctx, handler, url, token, manifest)
		if err != nil {
			return err
		}
		return writeHandoff(result, url, status, body)
	}
	status, body, err := postRemote(ctx, url, token, manifest)
	if err != nil {
		return err
	}
	return writeHandoff(result, url, status, body)
}

func postLocal(ctx context.Context, handler http.Handler, url, token string, manifest []byte) (int, string, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(manifest))
	if err != nil {
		return 0, "", err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("X-Katl-Install-Token", token)
	response := &responseCapture{header: http.Header{}}
	handler.ServeHTTP(response, httpRequest)
	if response.status == 0 {
		response.status = http.StatusOK
	}
	return response.status, response.body.String(), nil
}

func postRemote(ctx context.Context, url, token string, manifest []byte) (int, string, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(manifest))
	if err != nil {
		return 0, "", err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("X-Katl-Install-Token", token)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(httpRequest)
	if err != nil {
		return 0, "", fmt.Errorf("post handoff manifest: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, "", fmt.Errorf("read handoff response: %w", err)
	}
	return response.StatusCode, string(body), nil
}

func writeHandoff(result Result, url string, statusCode int, body string) error {
	log := handoffLog{
		URL:          url,
		ManifestPath: result.Artifacts.InstallManifest,
		StatusCode:   statusCode,
		Body:         body,
	}
	if err := writeJSON(result.Artifacts.HandoffResponse, log); err != nil {
		return err
	}
	if statusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("handoff failed: status=%d body=%s", statusCode, body)
	}
	return nil
}

func runtimeConfig(result Result, config InstalledRuntimeConfig) (InstalledRuntimeConfig, error) {
	if config.Disk != "" {
		return config, nil
	}
	for _, disk := range result.Disks {
		if disk.Kind == DiskTarget {
			config.Disk = disk.HostPath
			config.DiskFormat = disk.Format
			return config, nil
		}
	}
	return config, errors.New("target disk fixture is required")
}

func failFirst(runner Runner, scenario Scenario, result Result, phase string, err error) (Result, error) {
	now := runner.time()
	result.finish(StatusFailed, err.Error(), now)
	if len(result.Phases) > 0 {
		result.Phases[len(result.Phases)-1].Name = phase
	}
	if writeErr := runner.Write(scenario, result); writeErr != nil {
		return result, writeErr
	}
	return result, nil
}

func copyArtifact(src, dst string) error {
	if src == "" || dst == "" {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

type responseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *responseCapture) Header() http.Header {
	return r.header
}

func (r *responseCapture) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(data)
}

func (r *responseCapture) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
