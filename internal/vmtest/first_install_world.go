package vmtest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	installmanifest "github.com/katl-dev/katl/internal/installer/manifest"
	"gopkg.in/yaml.v3"
)

type FirstInstallWorldRun struct {
	Scenario *WorldScenario
	Node     Node
	Runner   Runner
	Config   FirstInstallConfig
	Repo     string
	Mode     FirstInstallWorldMode
}

type FirstInstallWorldInput struct {
	Installer       InstallerBootConfig
	RuntimeArtifact string
	RuntimeESP      string
	NodeMetadata    string
	InstallManifest string
	ConfigBundle    string
	Mode            FirstInstallWorldMode
	UseInstalledESP bool
	TargetDiskSize  string
}

type FirstInstallWorldMode string

const (
	FirstInstallWorldPreseed      FirstInstallWorldMode = "preseed"
	FirstInstallWorldGuestHandoff FirstInstallWorldMode = "guest-handoff"
)

type firstInstallWorldRun = FirstInstallWorldRun
type firstInstallWorldInput = FirstInstallWorldInput
type firstInstallWorldMode = FirstInstallWorldMode

const (
	firstInstallWorldPreseed      = FirstInstallWorldPreseed
	firstInstallWorldGuestHandoff = FirstInstallWorldGuestHandoff
)

func DefaultFirstInstallWorldInputFromEnv(mode FirstInstallWorldMode, useInstalledESP bool) FirstInstallWorldInput {
	return FirstInstallWorldInput{
		Installer:       FirstInstallInstallerBootFromEnv(),
		InstallManifest: strings.TrimSpace(os.Getenv("KATL_INSTALL_MANIFEST")),
		Mode:            mode,
		UseInstalledESP: useInstalledESP,
		TargetDiskSize:  first(os.Getenv("KATL_FIRST_INSTALL_TARGET_DISK_SIZE"), "32G"),
	}
}

func loadWorldManifestPath() string {
	return strings.TrimSpace(os.Getenv(WorldManifestEnv))
}

func firstKVM(value, fallback KVMPolicy) KVMPolicy {
	if value != "" {
		return value
	}
	return fallback
}

func planFirstInstallWorldRun(world World, name, repo string, spec NodeSpec, input firstInstallWorldInput, kvm KVMPolicy) (firstInstallWorldRun, error) {
	return PlanFirstInstallWorldRun(world, name, repo, spec, input, kvm)
}

func PlanFirstInstallWorldRun(world World, name, repo string, spec NodeSpec, input FirstInstallWorldInput, kvm KVMPolicy) (FirstInstallWorldRun, error) {
	scenario, err := world.PlanScenario(name)
	if err != nil {
		return FirstInstallWorldRun{}, err
	}
	run := FirstInstallWorldRun{Scenario: scenario, Repo: repo}
	node, err := scenario.AddNode(spec)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	run.Node = node
	factory := scenario.NodeFixtures(node)
	input, err = ResolveFirstInstallWorldInput(scenario, repo, spec, input)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	installer, err := factory.InstallerBoot(input.Installer)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	installManifest, err := factory.InstallManifest(input.InstallManifest)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	configBundle := FixtureRecord{}
	if strings.TrimSpace(input.ConfigBundle) != "" {
		configBundle, err = factory.ConfigBundle(input.ConfigBundle)
		if err != nil {
			_ = scenario.WriteSetupFailure(err)
			return run, err
		}
	}
	nodeMetadata := ""
	if strings.TrimSpace(input.NodeMetadata) != "" {
		metadata, err := factory.NodeMetadata(input.NodeMetadata)
		if err != nil {
			_ = scenario.WriteSetupFailure(err)
			return run, err
		}
		nodeMetadata = metadata.Path
	}
	target, err := factory.FirstInstallTargetDisk("root", DiskQCOW2, input.TargetDiskSize)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	mode := input.Mode
	if mode == "" {
		mode = FirstInstallWorldPreseed
	}
	run.Mode = mode
	run.Runner = NewRunner(Options{
		Enabled:   true,
		StateRoot: filepath.Join(scenario.Dir, "vm-runs"),
		Keep:      KeepAlways,
		KVM:       firstKVM(kvm, KVMAuto),
		Missing:   MissingFails,
	})
	run.Config = FirstInstallConfig{
		Installer: installer,
		Runtime: InstalledRuntimeConfig{
			NodeMetadata: nodeMetadata,
		},
		UseInstalledESP: input.UseInstalledESP,
		ManifestPath:    installManifest.Path,
		ConfigBundle:    configBundle.Path,
		SelectedNode:    spec.Name,
		TargetDisk:      target,
	}
	if err := verifyGenericInstallerArtifactsOmitExternalConfig(run.Config.Installer, run.Config.ManifestPath, run.Config.Runtime.NodeMetadata); err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	switch mode {
	case FirstInstallWorldPreseed:
		run.Config.PreseedManifest = true
	case FirstInstallWorldGuestHandoff:
		run.Config.GuestHandoff = true
	default:
		err := errors.New("unsupported first-install world mode: " + string(mode))
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	return run, nil
}

func verifyGenericInstallerArtifactsOmitExternalConfig(installer InstallerBootConfig, manifestPath, nodeMetadataPath string) error {
	values, err := externalConfigLiterals(manifestPath, nodeMetadataPath)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}
	for _, path := range genericInstallerArtifactPaths(installer, manifestPath) {
		if err := scanGenericInstallerArtifact(path, values); err != nil {
			return err
		}
	}
	return nil
}

func scanGenericInstallerArtifact(path string, values []string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("scan generic installer artifact %s: %w", path, err)
	}
	value, ok, err := scanReaderForExternalConfig(file, values)
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("scan generic installer artifact %s: %w", path, err)
	}
	if closeErr != nil {
		return fmt.Errorf("scan generic installer artifact %s: %w", path, closeErr)
	}
	if ok {
		return fmt.Errorf("generic installer artifact %s embeds external node config value %q", path, value)
	}
	magic, err := fileMagic(path)
	if err != nil {
		return err
	}
	if err := scanDecodedPayloads("generic installer artifact "+path, path, magic, values); err != nil {
		return err
	}
	if isSquashFS(magic) {
		if err := scanSquashFSForExternalConfig(path, values); err != nil {
			return err
		}
	}
	if isPECOFF(magic) {
		if err := scanPECOFFSectionsForExternalConfig(path, values); err != nil {
			return err
		}
	}
	return nil
}

func fileMagic(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	magic := make([]byte, 6)
	n, err := io.ReadFull(file, magic)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return magic[:n], nil
}

func scanReaderForExternalConfig(reader io.Reader, values []string) (string, bool, error) {
	overlap := maxExternalConfigLiteralLen(values) - 1
	if overlap < 0 {
		overlap = 0
	}
	tail := []byte{}
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := append(append([]byte{}, tail...), buf[:n]...)
			if value, ok := scanBytesForExternalConfig(chunk, values); ok {
				return value, true, nil
			}
			if overlap > 0 && len(chunk) > overlap {
				tail = append(tail[:0], chunk[len(chunk)-overlap:]...)
			} else {
				tail = append(tail[:0], chunk...)
			}
		}
		if errors.Is(err, io.EOF) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
	}
}

func maxExternalConfigLiteralLen(values []string) int {
	maxLen := 0
	for _, value := range values {
		if len(value) > maxLen {
			maxLen = len(value)
		}
	}
	return maxLen
}

func scanBytesForExternalConfig(data []byte, values []string) (string, bool) {
	for _, value := range values {
		if bytes.Contains(data, []byte(value)) {
			return value, true
		}
	}
	return "", false
}

func isGzip(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

func isXZ(data []byte) bool {
	return len(data) >= 6 && bytes.Equal(data[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0x00})
}

func isZstd(data []byte) bool {
	return len(data) >= 4 && bytes.Equal(data[:4], []byte{0x28, 0xb5, 0x2f, 0xfd})
}

func scanDecodedPayloads(label, path string, magic []byte, values []string) error {
	switch {
	case isGzip(magic):
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		reader, err := gzip.NewReader(file)
		if err != nil {
			_ = file.Close()
			return fmt.Errorf("decompress %s: %w", label, err)
		}
		value, ok, scanErr := scanReaderForExternalConfig(reader, values)
		closeErr := reader.Close()
		fileErr := file.Close()
		if scanErr != nil {
			return fmt.Errorf("decompress %s: %w", label, scanErr)
		}
		if closeErr != nil {
			return fmt.Errorf("decompress %s: %w", label, closeErr)
		}
		if fileErr != nil {
			return fmt.Errorf("decompress %s: %w", label, fileErr)
		}
		if ok {
			return fmt.Errorf("%s embeds external node config value %q", label, value)
		}
	case isXZ(magic):
		return scanCommandForExternalConfig(exec.Command("xz", "-dc", path), label, values)
	case isZstd(magic):
		return scanCommandForExternalConfig(exec.Command("zstd", "-dc", path), label, values)
	default:
		return nil
	}
	return nil
}

func isSquashFS(data []byte) bool {
	return len(data) >= 4 && string(data[:4]) == "hsqs"
}

func isPECOFF(data []byte) bool {
	return len(data) >= 2 && string(data[:2]) == "MZ"
}

func scanPECOFFSectionsForExternalConfig(path string, values []string) error {
	sections, err := peSectionNames(path)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "katl-uki-sections-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, section := range sections {
		if !scanPESection(section) {
			continue
		}
		out := filepath.Join(tmp, strings.TrimPrefix(section, "."))
		if err := exec.Command("objcopy", "--dump-section", section+"="+out, path).Run(); err != nil {
			return fmt.Errorf("dump generic installer artifact %s section %s: %w", path, section, err)
		}
		if err := scanGenericInstallerArtifact(out, values); err != nil {
			return fmt.Errorf("scan generic installer artifact %s section %s: %w", path, section, err)
		}
	}
	return nil
}

func peSectionNames(path string) ([]string, error) {
	output, err := exec.Command("objdump", "-h", path).Output()
	if err != nil {
		return nil, fmt.Errorf("list generic installer artifact %s sections: %w", path, err)
	}
	var sections []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if _, err := fmt.Sscanf(fields[0], "%d", new(int)); err != nil {
			continue
		}
		sections = append(sections, fields[1])
	}
	return sections, nil
}

func scanPESection(section string) bool {
	switch section {
	case ".cmdline", ".initrd", ".linux", ".osrel", ".uname", ".sbat":
		return true
	default:
		return false
	}
}

func scanSquashFSForExternalConfig(path string, values []string) error {
	files, err := squashFSFiles(path)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := scanCommandForExternalConfig(exec.Command("unsquashfs", "-cat", path, file), fmt.Sprintf("generic installer artifact %s member %s", path, file), values); err != nil {
			return err
		}
	}
	return nil
}

func scanCommandForExternalConfig(cmd *exec.Cmd, label string, values []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	value, ok, scanErr := scanReaderForExternalConfig(stdout, values)
	if ok {
		cancel()
		_ = cmd.Wait()
		return fmt.Errorf("%s embeds external node config value %q", label, value)
	}
	if scanErr != nil {
		cancel()
		_ = cmd.Wait()
		return scanErr
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("scan %s: %w", label, err)
	}
	return nil
}

func squashFSFiles(path string) ([]string, error) {
	output, err := exec.Command("unsquashfs", "-llc", path).Output()
	if err != nil {
		return nil, fmt.Errorf("list generic installer artifact %s: %w", path, err)
	}
	var files []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "-") {
			continue
		}
		name := fields[len(fields)-1]
		name = strings.TrimPrefix(name, "squashfs-root/")
		if name != "" && name != "squashfs-root" {
			files = append(files, name)
		}
	}
	return files, nil
}

func genericInstallerArtifactPaths(installer InstallerBootConfig, manifestPath string) []string {
	var paths []string
	for _, path := range []string{installer.InstallerUKI, installer.InstallerKernel, installer.InstallerInitrd} {
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
	}
	if imagePath, ok := stagedKatlOSImagePath(manifestPath); ok {
		paths = append(paths, imagePath)
	}
	return paths
}

func stagedKatlOSImagePath(manifestPath string) (string, bool) {
	file, err := os.Open(manifestPath)
	if err != nil {
		return "", false
	}
	defer file.Close()
	manifest, err := installmanifest.Decode(file)
	if err != nil {
		return "", false
	}
	localRef := strings.TrimSpace(manifest.KatlosImage.LocalRef)
	if localRef == "" {
		return "", false
	}
	return filepath.Join(filepath.Dir(manifestPath), filepath.FromSlash(localRef)), true
}

func externalConfigLiterals(manifestPath, nodeMetadataPath string) ([]string, error) {
	values := make(map[string]bool)
	file, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("open install manifest for generic artifact scan: %w", err)
	}
	manifest, err := installmanifest.Decode(file)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}

	addHostnameLiterals(values, manifest.Node.Identity.Hostname)
	addSystemRoleLiterals(values, manifest.Node.SystemRole)
	for _, key := range manifest.Node.Identity.SSH.AuthorizedKeys {
		addExternalConfigLiteral(values, key)
		fields := strings.Fields(key)
		if len(fields) > 1 {
			addExternalConfigLiteral(values, fields[1])
		}
	}
	for _, file := range manifest.Node.Networkd.Files {
		addExternalConfigLiteral(values, file.Name)
		addExternalConfigContentLiterals(values, file.Content)
	}
	addExternalConfigLiteral(values, manifest.Install.TargetDisk.ByID)
	addExternalConfigLiteral(values, manifest.Install.TargetDisk.WWN)
	addExternalConfigLiteral(values, manifest.Install.TargetDisk.Serial)
	for _, disk := range manifest.Install.ExtraDisks {
		addExternalConfigLiteral(values, disk.Name)
		addExternalConfigLiteral(values, disk.Selector.ByID)
		addExternalConfigLiteral(values, disk.Selector.WWN)
		addExternalConfigLiteral(values, disk.Selector.Serial)
		addExternalConfigLiteral(values, disk.Mount.Path)
	}
	if manifest.Node.Bootstrap != nil {
		bootstrap := manifest.Node.Bootstrap
		for _, value := range []string{
			bootstrap.ClusterName,
			bootstrap.InventoryNodeName,
			bootstrap.NodeAddress,
			bootstrap.ControlPlaneEndpoint,
			bootstrap.BootstrapProfileRef,
			bootstrap.ProfileResolvedID,
			bootstrap.KubernetesCatalogRef,
			bootstrap.Access.User,
			bootstrap.Access.CredentialRef,
		} {
			addExternalConfigLiteral(values, value)
		}
		for key, value := range bootstrap.Labels {
			addExternalConfigLiteral(values, key)
			addExternalConfigLiteral(values, value)
		}
		for _, taint := range bootstrap.Taints {
			addExternalConfigLiteral(values, taint.Key)
			addExternalConfigLiteral(values, taint.Value)
		}
	}
	if err := addKubeadmConfigLiterals(values, filepath.Dir(manifestPath)); err != nil {
		return nil, err
	}
	if strings.TrimSpace(nodeMetadataPath) != "" {
		if err := addNodeMetadataLiterals(values, nodeMetadataPath); err != nil {
			return nil, err
		}
	}

	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	return out, nil
}

func addKubeadmConfigLiterals(values map[string]bool, manifestDir string) error {
	root := filepath.Join(manifestDir, installer.KubeadmConfigFilesDir)
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := addKubeadmYAMLLiterals(values, data); err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			switch key {
			case "token", "certificateKey":
				addExternalConfigLiteral(values, value)
			}
			if strings.Contains(strings.ToLower(key), "token") ||
				strings.Contains(strings.ToLower(key), "secret") ||
				strings.Contains(strings.ToLower(key), "hash") ||
				strings.Contains(strings.ToLower(key), "sha256") ||
				strings.Contains(strings.ToLower(key), "key") {
				cleanKey := strings.TrimPrefix(strings.TrimSpace(key), "- ")
				value = strings.TrimSpace(value)
				addExternalConfigLiteral(values, value)
				if cleanKey == "sha256" && value != "" {
					addExternalConfigLiteral(values, cleanKey+":"+value)
				}
			}
		}
		return nil
	})
}

func addKubeadmYAMLLiterals(values map[string]bool, data []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var node yaml.Node
		err := decoder.Decode(&node)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		addKubeadmYAMLLiteral(values, &node, false)
	}
}

func addKubeadmYAMLLiteral(values map[string]bool, node *yaml.Node, collect bool) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if collect && node.Tag == "!!str" {
			addExternalConfigLiteral(values, node.Value)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := ""
			if node.Content[i] != nil {
				key = node.Content[i].Value
			}
			addYAMLKeyContextLiterals(values, key, node.Content[i+1])
			addKubeadmYAMLLiteral(values, node.Content[i+1], collect || kubeadmSensitiveLiteralKey(key))
		}
	default:
		for _, child := range node.Content {
			addKubeadmYAMLLiteral(values, child, collect)
		}
	}
}

func addYAMLKeyContextLiterals(values map[string]bool, key string, node *yaml.Node) {
	if !kubeadmLiteralKey(key) || node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return
	}
	key = strings.TrimSpace(key)
	value := strings.TrimSpace(node.Value)
	if key == "" || value == "" {
		return
	}
	if genericKubeadmDefaultLiteral(key, value) {
		return
	}
	addExternalConfigLiteral(values, key+": "+value)
	addExternalConfigLiteral(values, key+`: "`+value+`"`)
}

func kubeadmLiteralKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "name", "clustername", "podsubnet", "servicesubnet", "controlplaneendpoint", "apiserverendpoint", "token", "certificatekey", "cacerthashes", "cacertificateshashes":
		return true
	}
	return strings.Contains(key, "token") ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "hash") ||
		strings.Contains(key, "sha256") ||
		strings.Contains(key, "key")
}

func kubeadmSensitiveLiteralKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "bootstraptoken" {
		return false
	}
	switch key {
	case "token", "certificatekey", "cacerthashes", "cacertificateshashes":
		return true
	}
	return strings.Contains(key, "token") ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "hash") ||
		strings.Contains(key, "sha256") ||
		strings.Contains(key, "key")
}

func genericKubeadmDefaultLiteral(key, value string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	value = strings.TrimSpace(value)
	switch key {
	case "podsubnet":
		return value == "10.244.0.0/16"
	case "servicesubnet":
		return value == "10.96.0.0/12"
	default:
		return false
	}
}

func addNodeMetadataLiterals(values map[string]bool, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var metadata struct {
		Identity struct {
			Hostname string `json:"hostname"`
		} `json:"identity"`
		SystemRole string `json:"systemRole"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return err
	}
	addHostnameLiterals(values, metadata.Identity.Hostname)
	addSystemRoleLiterals(values, metadata.SystemRole)
	return nil
}

func addHostnameLiterals(values map[string]bool, hostname string) {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return
	}
	for _, value := range []string{
		`"hostname":"` + hostname + `"`,
		`"hostname": "` + hostname + `"`,
		`hostname: ` + hostname,
		`hostname: "` + hostname + `"`,
	} {
		addExternalConfigLiteral(values, value)
	}
	addExternalConfigLiteral(values, hostname)
}

func addSystemRoleLiterals(values map[string]bool, role string) {
	role = strings.TrimSpace(role)
	if role == "" {
		return
	}
	for _, value := range []string{
		`"systemRole":"` + role + `"`,
		`"systemRole": "` + role + `"`,
		`systemRole: ` + role,
		`systemRole: "` + role + `"`,
	} {
		addExternalConfigLiteral(values, value)
	}
}

func addExternalConfigContentLiterals(values map[string]bool, content string) {
	for _, field := range strings.FieldsFunc(content, func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\t' || r == ' ' || r == ',' || r == '[' || r == ']'
	}) {
		_, value, hasValue := strings.Cut(field, "=")
		if !hasValue {
			continue
		}
		addExternalConfigLiteral(values, value)
	}
}

func addExternalConfigLiteral(values map[string]bool, value string) {
	value = strings.TrimSpace(value)
	if len(value) < 8 {
		return
	}
	switch value {
	case "yes", "true", "false", "systemd", "control-plane", "worker", "NoSchedule", "PreferNoSchedule", "NoExecute":
		return
	}
	values[value] = true
}

type mkosiArtifactIndex struct {
	Artifacts []mkosiArtifact `json:"artifacts"`
}

type mkosiArtifact struct {
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	MetadataPath string `json:"metadataPath"`
	SHA256       string `json:"sha256"`
	SizeBytes    uint64 `json:"sizeBytes"`
}

type katlosImageMetadata struct {
	Version          string `json:"version"`
	Architecture     string `json:"architecture"`
	RuntimeInterface string `json:"runtimeInterface"`
	Role             string `json:"role"`
	ImageRole        string `json:"imageRole"`
	SHA256           string `json:"sha256"`
	SizeBytes        uint64 `json:"sizeBytes"`
	Components       []struct {
		Name           string `json:"name"`
		Role           string `json:"role"`
		PayloadVersion string `json:"payloadVersion"`
	} `json:"components"`
}

func resolveFirstInstallWorldInput(scenario *WorldScenario, repo string, spec NodeSpec, input firstInstallWorldInput) (firstInstallWorldInput, error) {
	return ResolveFirstInstallWorldInput(scenario, repo, spec, input)
}

func ResolveFirstInstallWorldInput(scenario *WorldScenario, repo string, spec NodeSpec, input FirstInstallWorldInput) (FirstInstallWorldInput, error) {
	index := defaultLocalMkosiArtifacts(repo)
	if indexPath := explicitMkosiArtifactIndexPath(); indexPath != "" {
		var err error
		index, err = readMkosiArtifactIndex(indexPath, repo)
		if err != nil {
			return input, err
		}
	}
	if input.Installer.InstallerUKI == "" && input.Installer.InstallerISO == "" && input.Installer.InstallerKernel == "" && input.Installer.InstallerInitrd == "" {
		if input.Mode == FirstInstallWorldGuestHandoff {
			if artifact, ok := index.artifact("installer-iso"); ok {
				input.Installer.InstallerISO = artifact.Path
			}
		}
		if input.Installer.InstallerISO == "" {
			if artifact, ok := index.artifact("installer-uki"); ok {
				input.Installer.InstallerUKI = artifact.Path
			}
		}
	}
	if strings.TrimSpace(input.RuntimeArtifact) != "" {
		return input, fmt.Errorf("first-install world consumes runtime components from katlosImage; loose runtime artifact input is not supported")
	}
	if strings.TrimSpace(input.RuntimeESP) != "" {
		return input, fmt.Errorf("first-install world consumes runtime UKI from katlosImage; loose runtime ESP input is not supported")
	}
	input.UseInstalledESP = true
	if input.InstallManifest == "" {
		if input.ConfigBundle == "" {
			bundlePath, manifestPath, err := writeFirstInstallWorldBundleSource(scenario, repo, spec, index, input.Mode == FirstInstallWorldGuestHandoff)
			if err != nil {
				return input, err
			}
			input.ConfigBundle = bundlePath
			input.InstallManifest = manifestPath
		} else {
			manifestPath, err := writeSelectedInstallManifestFromBundle(scenario, input.ConfigBundle, spec.Name)
			if err != nil {
				return input, err
			}
			input.InstallManifest = manifestPath
		}
	}
	if strings.TrimSpace(input.NodeMetadata) == "" {
		metadataPath, err := writeFirstInstallWorldNodeMetadataSource(scenario, spec)
		if err != nil {
			return input, err
		}
		input.NodeMetadata = metadataPath
	}
	return input, nil
}

func writeSelectedInstallManifestFromBundle(_ *WorldScenario, bundlePath, nodeName string) (string, error) {
	return writeSelectedInstallManifestFromBundleWithDefault(nil, bundlePath, nodeName, installmanifest.KatlosImage{})
}

func writeSelectedInstallManifestFromBundleWithDefault(_ *WorldScenario, bundlePath, nodeName string, defaultImage installmanifest.KatlosImage) (string, error) {
	selected, err := configbundle.ReadSelectedNodeFile(bundlePath, configbundle.ReadOptions{NodeName: nodeName, DefaultKatlosImage: defaultImage})
	if err != nil {
		return "", err
	}
	out := filepath.Join(filepath.Dir(bundlePath), "install-manifest.json")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(selected.InstallManifest, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := writeSelectedKubeadmSidecars(filepath.Dir(out), selected.KubeadmConfigs); err != nil {
		return "", err
	}
	return out, nil
}

func writeSelectedKubeadmSidecars(dir string, configs map[string]kubeadmconfig.Plan) error {
	for name, plan := range configs {
		configRel := filepath.ToSlash(filepath.Join(installer.KubeadmConfigFilesDir, name+".yaml"))
		objectDir := filepath.Join(dir, installer.KubeadmConfigObjectsDir)
		if err := os.MkdirAll(objectDir, 0o755); err != nil {
			return err
		}
		object := fmt.Sprintf("apiVersion: config.katl.dev/v1alpha1\nkind: KubeadmConfig\nmetadata:\n  name: %s\nspec:\n  configFile: %s\n", name, configRel)
		if err := os.WriteFile(filepath.Join(objectDir, name+".yaml"), []byte(object), 0o644); err != nil {
			return err
		}
		configPath := filepath.Join(dir, filepath.FromSlash(configRel))
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(configPath, plan.Config.Content, 0o644); err != nil {
			return err
		}
		for _, patch := range plan.Patches {
			rel := strings.TrimPrefix(filepath.ToSlash(patch.RenderPath), "/etc/katl/kubeadm/"+name+"/")
			if rel == "" || strings.HasPrefix(rel, "../") {
				return fmt.Errorf("kubeadm patch path %q is outside selected config %q", patch.RenderPath, name)
			}
			path := filepath.Join(dir, installer.KubeadmConfigFilesDir, name, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, patch.Content, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func explicitMkosiArtifactIndexPath() string {
	return strings.TrimSpace(os.Getenv("KATL_MKOSI_ARTIFACT_INDEX"))
}

func defaultLocalMkosiArtifacts(repo string) mkosiArtifactIndex {
	var index mkosiArtifactIndex
	mkosiDir := filepath.Join(repo, "_build", "mkosi")
	if current, err := readMkosiArtifactIndex(filepath.Join(mkosiDir, "artifacts.json"), repo); err == nil {
		index = current
	}
	for _, artifact := range []mkosiArtifact{
		{Kind: "installer-uki", Path: filepath.Join(mkosiDir, "katl-installer.efi")},
		{Kind: "runtime-root", Path: filepath.Join(mkosiDir, "katl-runtime-root.squashfs")},
		{Kind: "kubernetes-sysext", Path: filepath.Join(mkosiDir, "katl-kubernetes.raw"), MetadataPath: filepath.Join(mkosiDir, "katl-kubernetes.raw.json")},
	} {
		if _, exists := index.artifact(artifact.Kind); !exists {
			if _, err := os.Stat(artifact.Path); err != nil {
				continue
			}
			index.Artifacts = append(index.Artifacts, artifact)
		}
	}
	if _, exists := index.artifact("katlos-install-image"); !exists {
		if artifact, err := discoverKatlOSInstallImage(repo); err == nil {
			index.Artifacts = append(index.Artifacts, artifact)
		}
	}
	return index
}

func readMkosiArtifactIndex(path, repo string) (mkosiArtifactIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return mkosiArtifactIndex{}, err
	}
	var index mkosiArtifactIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return mkosiArtifactIndex{}, err
	}
	for i := range index.Artifacts {
		index.Artifacts[i].Path = repoAbs(repo, index.Artifacts[i].Path)
		index.Artifacts[i].MetadataPath = repoAbs(repo, index.Artifacts[i].MetadataPath)
	}
	return index, nil
}

func (index mkosiArtifactIndex) artifact(kind string) (mkosiArtifact, bool) {
	for _, artifact := range index.Artifacts {
		if artifact.Kind == kind {
			return artifact, true
		}
	}
	return mkosiArtifact{}, false
}

func writeFirstInstallWorldBundleSource(scenario *WorldScenario, repo string, spec NodeSpec, index mkosiArtifactIndex, bindInstallMedia bool) (string, string, error) {
	image, ok := index.artifact("katlos-install-image")
	if !ok {
		var err error
		image, err = discoverKatlOSInstallImage(repo)
		if err != nil {
			return "", "", err
		}
	}
	metadata, err := readKatlOSImageMetadata(image)
	if err != nil {
		return "", "", err
	}
	if metadata.Role == "" {
		metadata.Role = metadata.ImageRole
	}
	if metadata.Role == "" {
		metadata.Role = "install"
	}
	if metadata.SHA256 == "" {
		metadata.SHA256 = image.SHA256
	}
	if metadata.SizeBytes == 0 {
		metadata.SizeBytes = image.SizeBytes
	}
	sourceDir := filepath.Join(scenario.Dir, "inputs", "config-bundle-source", spec.Name)
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return "", "", err
	}
	localRef := filepath.Base(image.Path)
	localImage := filepath.Join(sourceDir, localRef)
	if _, err := os.Lstat(localImage); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(image.Path, localImage); err != nil {
			return "", "", err
		}
	} else if err != nil {
		return "", "", err
	}
	kubernetesVersion := metadata.KubernetesPayloadVersion()
	if kubernetesVersion == "" {
		if artifact, ok := index.artifact("kubernetes-sysext"); ok {
			var err error
			kubernetesVersion, err = readKubernetesSysextPayloadVersion(artifact)
			if err != nil {
				return "", "", err
			}
		}
	}
	if kubernetesVersion == "" {
		kubernetesVersion = "v1.36.1"
	}

	kubeadmRef := ""
	kubeadmConfigs := map[string]any{}
	kubeadmConfigs["control-plane"] = map[string]any{"config": controlPlaneKubeadmConfig(firstControlPlaneName(spec), kubernetesVersion)}
	switch spec.Role {
	case ControlPlane:
		kubeadmRef = "control-plane"
	case Worker:
		kubeadmRef = "worker"
		kubeadmConfigs[kubeadmRef] = map[string]any{"config": workerKubeadmConfig(spec.Name)}
	}
	systemRoleDefaults := map[string]any{}
	systemRoleDefaults[string(ControlPlane)] = map[string]any{
		"kubernetes": map[string]any{
			"kubeadm": map[string]any{"configRef": "control-plane"},
		},
	}
	if kubeadmRef != "" && spec.Role != ControlPlane {
		systemRoleDefaults[string(spec.Role)] = map[string]any{
			"kubernetes": map[string]any{
				"kubeadm": map[string]any{"configRef": kubeadmRef},
			},
		}
	}
	nodes := []map[string]any{}
	if spec.Role != ControlPlane {
		nodes = append(nodes, firstInstallWorldSourceNode(firstControlPlaneName(spec), ControlPlane, "/dev/disk/by-id/virtio-katl-control-plane-root"))
	}
	nodes = append(nodes, firstInstallWorldSourceNode(spec.Name, spec.Role, "/dev/disk/by-id/virtio-katl-root"))

	sourceSpec := map[string]any{
		"controlPlaneEndpoint": "api.katl.test:6443",
		"kubernetes": map[string]any{
			"version": kubernetesVersion,
		},
		"defaults": map[string]any{
			"install": map[string]any{
				"wipeTarget": true,
			},
			"identity": map[string]any{
				"ssh": map[string]any{
					"authorizedKeys": []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"},
				},
			},
			"networkd": map[string]any{
				"files": []map[string]any{{
					"name":    "80-katl-vmtest-dhcp.network",
					"content": "[Match]\nName=en*\n\n[Network]\nDHCP=yes\n",
				}},
			},
			"bootstrap": map[string]any{
				"access": map[string]any{
					"method":        "agent",
					"credentialRef": "vsock:1234:10240",
				},
			},
		},
		"systemRoleDefaults": systemRoleDefaults,
		"kubeadmConfigs":     kubeadmConfigs,
		"nodes":              nodes,
	}
	if !bindInstallMedia {
		sourceSpec["katlosImage"] = map[string]any{
			"localRef":         localRef,
			"sha256":           metadata.SHA256,
			"sizeBytes":        metadata.SizeBytes,
			"version":          metadata.Version,
			"architecture":     metadata.Architecture,
			"runtimeInterface": metadata.RuntimeInterface,
			"role":             metadata.Role,
		}
	}
	source := map[string]any{
		"apiVersion": configbundle.APIVersion,
		"kind":       configbundle.Kind,
		"metadata": map[string]any{
			"name": "katl-smoke",
		},
		"spec": sourceSpec,
	}
	sourcePath := filepath.Join(sourceDir, "cluster.yaml")
	sourceData, err := yaml.Marshal(source)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(sourcePath, sourceData, 0o644); err != nil {
		return "", "", err
	}
	bundlePath := filepath.Join(sourceDir, "config.katlcfg")
	if _, err := configbundle.WriteArchive(bundlePath, configbundle.BuildRequest{SourcePath: sourcePath, CreatedBy: "vmtest first-install"}); err != nil {
		return "", "", err
	}
	defaultImage := installmanifest.KatlosImage{}
	if bindInstallMedia {
		defaultImage = installmanifest.KatlosImage{
			LocalRef:         localRef,
			SHA256:           metadata.SHA256,
			SizeBytes:        metadata.SizeBytes,
			Version:          metadata.Version,
			Architecture:     metadata.Architecture,
			RuntimeInterface: metadata.RuntimeInterface,
			Role:             metadata.Role,
		}
	}
	manifestPath, err := writeSelectedInstallManifestFromBundleWithDefault(scenario, bundlePath, spec.Name, defaultImage)
	if err != nil {
		return "", "", err
	}
	return bundlePath, manifestPath, nil
}

func firstControlPlaneName(spec NodeSpec) string {
	if spec.Role == ControlPlane {
		return spec.Name
	}
	return "cp-1"
}

func firstInstallWorldSourceNode(name string, role NodeRole, diskID string) map[string]any {
	return map[string]any{
		"name":       name,
		"systemRole": string(role),
		"overrides": map[string]any{
			"install": map[string]any{
				"targetDisk": map[string]any{
					"byID":       diskID,
					"minSizeMiB": 32,
				},
			},
		},
	}
}

func writeFirstInstallWorldManifestSource(scenario *WorldScenario, repo string, spec NodeSpec, index mkosiArtifactIndex) (string, error) {
	image, ok := index.artifact("katlos-install-image")
	if !ok {
		var err error
		image, err = discoverKatlOSInstallImage(repo)
		if err != nil {
			return "", err
		}
	}
	metadata, err := readKatlOSImageMetadata(image)
	if err != nil {
		return "", err
	}
	if metadata.Role == "" {
		metadata.Role = metadata.ImageRole
	}
	if metadata.Role == "" {
		metadata.Role = "install"
	}
	if metadata.SHA256 == "" {
		metadata.SHA256 = image.SHA256
	}
	if metadata.SizeBytes == 0 {
		metadata.SizeBytes = image.SizeBytes
	}
	sourceDir := filepath.Join(scenario.Dir, "inputs", "install-manifest-source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return "", err
	}
	localRef := filepath.Base(image.Path)
	localImage := filepath.Join(sourceDir, localRef)
	if _, err := os.Lstat(localImage); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(image.Path, localImage); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	kubernetesVersion := metadata.KubernetesPayloadVersion()
	if kubernetesVersion == "" {
		if artifact, ok := index.artifact("kubernetes-sysext"); ok {
			var err error
			kubernetesVersion, err = readKubernetesSysextPayloadVersion(artifact)
			if err != nil {
				return "", err
			}
		}
	}
	kubeadmRef, err := writeFirstInstallWorldKubeadmSource(sourceDir, spec, kubernetesVersion)
	if err != nil {
		return "", err
	}
	node := map[string]any{
		"identity": map[string]any{
			"hostname": spec.Name,
			"ssh": map[string]any{
				"authorizedKeys": []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"},
			},
		},
		"networkd": map[string]any{
			"files": []map[string]any{
				{
					"name":    "80-katl-vmtest-dhcp.network",
					"content": "[Match]\nName=en*\n\n[Network]\nDHCP=yes\n",
				},
			},
		},
		"systemRole": string(spec.Role),
	}
	if kubeadmRef != "" {
		node["kubernetes"] = map[string]any{
			"kubeadm": map[string]any{
				"configRef": kubeadmRef,
			},
		}
	}
	if kubernetesVersion != "" {
		node["bootstrap"] = map[string]any{
			"kubernetesCatalogRef": kubernetesVersion,
		}
	}
	manifest := map[string]any{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind":       "InstallManifest",
		"node":       node,
		"install": map[string]any{
			"wipeTarget": true,
			"targetDisk": map[string]any{
				"byID":       "/dev/disk/by-id/virtio-katl-root",
				"minSizeMiB": 32,
			},
		},
		"katlosImage": map[string]any{
			"localRef":         localRef,
			"sha256":           metadata.SHA256,
			"sizeBytes":        metadata.SizeBytes,
			"version":          metadata.Version,
			"architecture":     metadata.Architecture,
			"runtimeInterface": metadata.RuntimeInterface,
			"role":             metadata.Role,
		},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	manifestPath := filepath.Join(sourceDir, "install-manifest.json")
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return manifestPath, nil
}

func (metadata katlosImageMetadata) KubernetesPayloadVersion() string {
	for _, component := range metadata.Components {
		if component.Name == "kubernetes" || component.Role == "kubernetes-sysext" {
			return strings.TrimSpace(component.PayloadVersion)
		}
	}
	return ""
}

func readKubernetesSysextPayloadVersion(artifact mkosiArtifact) (string, error) {
	metadataPath := strings.TrimSpace(artifact.MetadataPath)
	if metadataPath == "" {
		metadataPath = artifact.Path + ".json"
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", fmt.Errorf("read Kubernetes sysext metadata: %w", err)
	}
	var metadata struct {
		PayloadVersion string `json:"payloadVersion"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("decode Kubernetes sysext metadata: %w", err)
	}
	return strings.TrimSpace(metadata.PayloadVersion), nil
}

func writeFirstInstallWorldKubeadmSource(sourceDir string, spec NodeSpec, kubernetesVersion string) (string, error) {
	switch spec.Role {
	case ControlPlane:
		return writeFirstInstallWorldKubeadmPlan(sourceDir, "control-plane", controlPlaneKubeadmConfig(spec.Name, kubernetesVersion))
	case Worker:
		return writeFirstInstallWorldKubeadmPlan(sourceDir, "worker", workerKubeadmConfig(spec.Name))
	default:
		return "", nil
	}
}

func writeFirstInstallWorldKubeadmPlan(sourceDir, name, config string) (string, error) {
	configRel := filepath.ToSlash(filepath.Join(installer.KubeadmConfigFilesDir, name+".yaml"))
	objectDir := filepath.Join(sourceDir, installer.KubeadmConfigObjectsDir)
	if err := os.MkdirAll(objectDir, 0o755); err != nil {
		return "", err
	}
	configPath := filepath.Join(sourceDir, filepath.FromSlash(configRel))
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return "", err
	}
	object := fmt.Sprintf(`apiVersion: config.katl.dev/v1alpha1
kind: KubeadmConfig
metadata:
  name: %s
spec:
  configFile: %s
`, name, configRel)
	if err := os.WriteFile(filepath.Join(objectDir, name+".yaml"), []byte(object), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func controlPlaneKubeadmConfig(nodeName string, kubernetesVersion string) string {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		nodeName = "cp-1"
	}
	kubernetesVersion = strings.TrimSpace(kubernetesVersion)
	if kubernetesVersion == "" {
		kubernetesVersion = "v1.36.1"
	}
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  name: ` + nodeName + `
  criSocket: unix:///run/containerd/containerd.sock
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
clusterName: katl-smoke
kubernetesVersion: ` + kubernetesVersion + `
networking:
  podSubnet: 10.244.0.0/16
  serviceSubnet: 10.96.0.0/12
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
`
}

func workerKubeadmConfig(nodeName string) string {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		nodeName = "worker-1"
	}
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
nodeRegistration:
  name: ` + nodeName + `
  criSocket: unix:///run/containerd/containerd.sock
`
}

func writeFirstInstallWorldNodeMetadataSource(scenario *WorldScenario, spec NodeSpec) (string, error) {
	sourceDir := filepath.Join(scenario.Dir, "inputs", "node-metadata-source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return "", err
	}
	metadata := map[string]any{
		"apiVersion": "katl.dev/v1alpha1",
		"kind":       "NodeMetadata",
		"identity": map[string]any{
			"hostname": spec.Name,
		},
		"systemRole": string(spec.Role),
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", err
	}
	metadataPath := filepath.Join(sourceDir, "node.json")
	if err := os.WriteFile(metadataPath, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return metadataPath, nil
}

func discoverKatlOSInstallImage(repo string) (mkosiArtifact, error) {
	matches, err := filepath.Glob(filepath.Join(repo, "_build", "mkosi", "katlos-install-*.squashfs"))
	if err != nil {
		return mkosiArtifact{}, err
	}
	if len(matches) != 1 {
		return mkosiArtifact{}, fmt.Errorf("install manifest source is required: expected one local KatlOS install image, found %d", len(matches))
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		return mkosiArtifact{}, err
	}
	return mkosiArtifact{Kind: "katlos-install-image", Path: matches[0], MetadataPath: matches[0] + ".json", SizeBytes: uint64(info.Size())}, nil
}

func readKatlOSImageMetadata(image mkosiArtifact) (katlosImageMetadata, error) {
	if strings.TrimSpace(image.MetadataPath) == "" {
		return katlosImageMetadata{}, errors.New("KatlOS install image metadata is required")
	}
	data, err := os.ReadFile(image.MetadataPath)
	if err != nil {
		return katlosImageMetadata{}, err
	}
	var metadata katlosImageMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return katlosImageMetadata{}, err
	}
	return metadata, nil
}

func repoAbs(repo, path string) string {
	if strings.TrimSpace(path) == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(repo, path)
}

func firstInstallInstallerBootFromEnv() InstallerBootConfig {
	return FirstInstallInstallerBootFromEnv()
}

func FirstInstallInstallerBootFromEnv() InstallerBootConfig {
	kernel := strings.TrimSpace(os.Getenv("KATL_INSTALLER_KERNEL"))
	initrd := strings.TrimSpace(os.Getenv("KATL_INSTALLER_INITRD"))
	if kernel != "" || initrd != "" {
		return InstallerBootConfig{
			InstallerKernel: kernel,
			InstallerInitrd: initrd,
			CommandLine: []string{
				"console=ttyS0,115200n8",
				"systemd.log_target=console",
				"loglevel=6",
			},
		}
	}
	return InstallerBootConfig{InstallerUKI: strings.TrimSpace(os.Getenv("KATL_INSTALLER_UKI"))}
}
