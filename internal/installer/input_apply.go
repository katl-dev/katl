package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type InputApplyRequest struct {
	Context      context.Context
	PreseedDirs  []string
	MediaDevices []string
	MediaMount   string
	SeedDevices  []string
	SeedMount    string
	SeedWait     time.Duration
	Commands     CommandRunner
	RunDir       string
	EtcDir       string
	NetworkDir   string
	Stdout       io.Writer
}

const (
	DefaultSeedMount        = "/run/katl/preseed"
	DefaultMediaMount       = "/run/katl/media"
	KubeadmConfigObjectsDir = "kubeadm-configs"
	KubeadmConfigFilesDir   = "kubeadm"
)

var DefaultSeedDevices = []string{
	"/dev/disk/by-label/KATLSEED",
	"/dev/disk/by-id/virtio-katl-seed",
}

var DefaultMediaDevices = []string{
	"/dev/disk/by-label/KATL_INSTALLER",
}

func DefaultPreseedDirs() []string {
	return []string{
		"/usr/lib/katl/preseed",
		"/run/katl/preseed",
		"/etc/katl/preseed",
	}
}

func ApplyInput(request InputApplyRequest) error {
	ctx := request.Context
	if ctx == nil {
		ctx = context.Background()
	}
	runDir := request.RunDir
	if runDir == "" {
		runDir = "/run/katl"
	}
	etcDir := request.EtcDir
	if etcDir == "" {
		etcDir = "/etc/katl"
	}
	networkDir := request.NetworkDir
	if networkDir == "" {
		networkDir = "/etc/systemd/network"
	}
	stdout := request.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	if err := mountInstallMedia(ctx, request, stdout); err != nil {
		return err
	}
	if err := mountSeedDevice(ctx, request, stdout); err != nil {
		return err
	}

	applied := 0
	for _, dir := range request.PreseedDirs {
		n, err := applyDir(dir, runDir, etcDir, networkDir, stdout)
		if err != nil {
			return err
		}
		applied += n
	}
	if applied == 0 {
		fmt.Fprintln(stdout, "katl input: no preseed files found")
	}
	return nil
}

func mountInstallMedia(ctx context.Context, request InputApplyRequest, stdout io.Writer) error {
	device := firstExistingDevice(request.MediaDevices)
	if device == "" {
		return nil
	}
	mountPoint := request.MediaMount
	if mountPoint == "" {
		mountPoint = DefaultMediaMount
	}
	commands := request.Commands
	if commands == nil {
		commands = NewExecCommandRunner()
	}
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("create install media mount %s: %w", mountPoint, err)
	}
	if err := commands.Run(ctx, "mount", "-o", "ro", device, mountPoint); err != nil {
		return fmt.Errorf("mount install media %s: %w", device, err)
	}
	fmt.Fprintf(stdout, "katl input: mounted install media %s at %s\n", device, mountPoint)
	return nil
}

func firstExistingDevice(devices []string) string {
	for _, device := range devices {
		if _, err := os.Stat(device); err == nil {
			return device
		}
	}
	return ""
}

func mountSeedDevice(ctx context.Context, request InputApplyRequest, stdout io.Writer) error {
	devices := request.SeedDevices
	if len(devices) == 0 {
		return nil
	}
	device, err := waitSeedDevice(ctx, devices, request.SeedWait)
	if err != nil {
		return err
	}
	if device == "" {
		writeMissingSeedDevice(stdout, devices, request.SeedWait)
		return nil
	}
	mountPoint := request.SeedMount
	if mountPoint == "" {
		mountPoint = DefaultSeedMount
	}
	commands := request.Commands
	if commands == nil {
		commands = NewExecCommandRunner()
	}
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("create seed mount %s: %w", mountPoint, err)
	}
	if err := commands.Run(ctx, "mount", "-o", "ro", device, mountPoint); err != nil {
		return fmt.Errorf("mount seed device %s: %w", device, err)
	}
	fmt.Fprintf(stdout, "katl input: mounted seed device %s at %s\n", device, mountPoint)
	return nil
}

func writeMissingSeedDevice(stdout io.Writer, devices []string, wait time.Duration) {
	checked := compactDeviceList(devices)
	if len(checked) == 0 {
		return
	}
	fmt.Fprintf(stdout, "katl input: seed device not found after %s; checked %s\n", wait, joinComma(checked))
	for _, dir := range seedDeviceParents(checked) {
		exists := true
		if _, err := os.Stat(dir); err != nil {
			exists = false
		}
		fmt.Fprintf(stdout, "katl input: seed device directory %s exists=%t\n", dir, exists)
	}
}

func compactDeviceList(devices []string) []string {
	out := make([]string, 0, len(devices))
	for _, device := range devices {
		if device == "" {
			continue
		}
		out = append(out, device)
	}
	return out
}

func seedDeviceParents(devices []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, device := range devices {
		dir := filepath.Dir(device)
		if dir == "." || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

func joinComma(values []string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += ", "
		}
		out += value
	}
	return out
}

func waitSeedDevice(ctx context.Context, devices []string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		for _, candidate := range devices {
			if candidate == "" {
				continue
			}
			if _, err := os.Stat(candidate); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return "", fmt.Errorf("stat seed device %s: %w", candidate, err)
			}
			return candidate, nil
		}
		if wait <= 0 || !time.Now().Before(deadline) {
			return "", nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

func applyDir(dir, runDir, etcDir, networkDir string, stdout io.Writer) (int, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat preseed dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("preseed path %s is not a directory", dir)
	}

	applied := 0
	for _, item := range preseedItems(dir, runDir, etcDir) {
		ok, err := copyInput(item.src, item.dst, item.manifest)
		if err != nil {
			return applied, err
		}
		if ok {
			applied++
			fmt.Fprintf(stdout, "katl input: copied %s to %s\n", item.src, item.dst)
			if item.manifest {
				if manifestPayloadsSelectedInPlace(dir, item.src) {
					continue
				}
				copiedPayloads, err := CopyManifestPayloads(item.src, filepath.Dir(item.src), filepath.Dir(item.dst))
				if err != nil {
					return applied, err
				}
				for _, copied := range copiedPayloads {
					applied++
					fmt.Fprintf(stdout, "katl input: copied %s to %s\n", copied.Source, copied.Destination)
				}
			}
		}
	}
	ok, err := copyOptionalPreseedPayload(filepath.Join(dir, "etc/systemd/network"), networkDir)
	if err != nil {
		return applied, fmt.Errorf("copy preseed systemd network: %w", err)
	}
	if ok {
		applied++
		fmt.Fprintf(stdout, "katl input: copied %s to %s\n", filepath.Join(dir, "etc/systemd/network"), networkDir)
	}
	return applied, nil
}

func manifestPayloadsSelectedInPlace(dir, manifestPath string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "install-input.json"))
	if err != nil {
		return false
	}
	var values bootInputValues
	if err := json.Unmarshal(data, &values); err != nil {
		return false
	}
	selected := strings.TrimSpace(values.ManifestPath)
	if selected == "" {
		return false
	}
	return filepath.Clean(selected) == filepath.Clean(manifestPath)
}

type preseedItem struct {
	src      string
	dst      string
	manifest bool
}

func preseedItems(dir, runDir, etcDir string) []preseedItem {
	return []preseedItem{
		{src: filepath.Join(dir, "install-input.json"), dst: filepath.Join(runDir, "install-input.json")},
		{src: filepath.Join(dir, "install-manifest.yaml"), dst: filepath.Join(runDir, "install-manifest.yaml"), manifest: true},
		{src: filepath.Join(dir, "install-manifest.yml"), dst: filepath.Join(runDir, "install-manifest.yml"), manifest: true},
		{src: filepath.Join(dir, "install-manifest.json"), dst: filepath.Join(runDir, "install-manifest.json"), manifest: true},
		{src: filepath.Join(dir, "run/katl/install-input.json"), dst: filepath.Join(runDir, "install-input.json")},
		{src: filepath.Join(dir, "run/katl/install-manifest.yaml"), dst: filepath.Join(runDir, "install-manifest.yaml"), manifest: true},
		{src: filepath.Join(dir, "run/katl/install-manifest.yml"), dst: filepath.Join(runDir, "install-manifest.yml"), manifest: true},
		{src: filepath.Join(dir, "run/katl/install-manifest.json"), dst: filepath.Join(runDir, "install-manifest.json"), manifest: true},
		{src: filepath.Join(dir, "etc/katl/install-input.json"), dst: filepath.Join(etcDir, "install-input.json")},
		{src: filepath.Join(dir, "etc/katl/install-manifest.yaml"), dst: filepath.Join(etcDir, "install-manifest.yaml"), manifest: true},
		{src: filepath.Join(dir, "etc/katl/install-manifest.yml"), dst: filepath.Join(etcDir, "install-manifest.yml"), manifest: true},
		{src: filepath.Join(dir, "etc/katl/install-manifest.json"), dst: filepath.Join(etcDir, "install-manifest.json"), manifest: true},
	}
}

func copyInput(src, dst string, manifest bool) (bool, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read preseed file %s: %w", src, err)
	}
	if manifest {
		var value any
		if err := yaml.Unmarshal(data, &value); err != nil {
			return false, fmt.Errorf("preseed manifest %s is not valid YAML: %w", src, err)
		}
	} else if !json.Valid(data) {
		return false, fmt.Errorf("preseed file %s is not valid JSON", src)
	}
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat destination %s: %w", dst, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, fmt.Errorf("create destination dir %s: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return false, fmt.Errorf("write destination %s: %w", dst, err)
	}
	return true, nil
}

type CopiedManifestPayload struct {
	Source      string
	Destination string
}

var preseedLocalRefRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+(/[A-Za-z0-9._+-]+)*$`)

// CopyManifestPayloads copies local payloads referenced by a manifest from srcRoot to dstRoot.
func CopyManifestPayloads(manifestPath, srcRoot, dstRoot string) ([]CopiedManifestPayload, error) {
	var copied []CopiedManifestPayload
	localRef, err := manifestLocalRef(manifestPath)
	if err != nil {
		return nil, err
	}
	if localRef != "" {
		src := filepath.Join(srcRoot, filepath.FromSlash(localRef))
		dst := filepath.Join(dstRoot, filepath.FromSlash(localRef))
		ok, err := copyPreseedPayload(src, dst)
		if err != nil {
			return nil, fmt.Errorf("copy preseed KatlOS image localRef: %w", err)
		}
		if ok {
			copied = append(copied, CopiedManifestPayload{Source: src, Destination: dst})
		}
	}
	copiedDirs, err := copyManifestKubeadmDirs(srcRoot, dstRoot)
	if err != nil {
		return nil, err
	}
	copied = append(copied, copiedDirs...)
	return copied, nil
}

func copyManifestKubeadmDirs(srcRoot, dstRoot string) ([]CopiedManifestPayload, error) {
	var copied []CopiedManifestPayload
	for _, name := range []string{KubeadmConfigObjectsDir, KubeadmConfigFilesDir} {
		src := filepath.Join(srcRoot, name)
		dst := filepath.Join(dstRoot, name)
		ok, err := copyOptionalPreseedPayload(src, dst)
		if err != nil {
			return nil, fmt.Errorf("copy preseed %s: %w", name, err)
		}
		if ok {
			copied = append(copied, CopiedManifestPayload{Source: src, Destination: dst})
		}
	}
	return copied, nil
}

func manifestLocalRef(manifestPath string) (string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("read preseed manifest %s: %w", manifestPath, err)
	}
	var input struct {
		KatlosImage struct {
			LocalRef string `yaml:"localRef"`
		} `yaml:"katlosImage"`
	}
	if err := yaml.Unmarshal(data, &input); err != nil {
		return "", fmt.Errorf("decode preseed manifest %s: %w", manifestPath, err)
	}
	localRef := strings.TrimSpace(input.KatlosImage.LocalRef)
	if localRef == "" {
		return "", nil
	}
	if input.KatlosImage.LocalRef != localRef || filepath.IsAbs(localRef) || path.Clean(localRef) != localRef || !preseedLocalRefRE.MatchString(localRef) {
		return "", fmt.Errorf("preseed KatlOS image localRef %q must be a clean relative path", input.KatlosImage.LocalRef)
	}
	for _, segment := range strings.Split(localRef, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("preseed KatlOS image localRef %q must not contain dot segments", localRef)
		}
	}
	return localRef, nil
}

func copyPreseedPayload(src, dst string) (bool, error) {
	info, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	return copyPreseedPayloadWithInfo(src, dst, info)
}

func copyOptionalPreseedPayload(src, dst string) (bool, error) {
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return copyPreseedPayloadWithInfo(src, dst, info)
}

func copyPreseedPayloadWithInfo(src, dst string, info os.FileInfo) (bool, error) {
	if info.IsDir() {
		if existing, err := os.Stat(dst); err == nil && !existing.IsDir() {
			return false, fmt.Errorf("destination %s exists and is not a directory", dst)
		} else if err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("stat destination %s: %w", dst, err)
		}
		return true, copyPreseedDir(src, dst, info.Mode().Perm())
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file or directory", src)
	}
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat destination %s: %w", dst, err)
	}
	return true, copyPreseedFile(src, dst, info.Mode().Perm())
}

func copyPreseedDir(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(dst, mode); err != nil {
		return fmt.Errorf("create preseed payload dir %s: %w", dst, err)
	}
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file or directory", path)
		}
		return copyPreseedFile(path, target, info.Mode().Perm())
	})
}

func copyPreseedFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create preseed payload dir %s: %w", filepath.Dir(dst), err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
