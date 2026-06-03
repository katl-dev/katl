package generation

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type LoaderRequest struct {
	Record    Record
	MachineID string
	Title     string
}

type LoaderEntry struct {
	Name    string
	Content string
}

func RenderEntry(request LoaderRequest) (LoaderEntry, error) {
	record := request.Record
	generationID, err := cleanSegment("generation id", record.GenerationID)
	if err != nil {
		return LoaderEntry{}, err
	}
	runtimeVersion, err := cleanToken("runtime version", record.RuntimeVersion)
	if err != nil {
		return LoaderEntry{}, err
	}
	rootSlot, err := cleanToken("root slot", record.Root.Slot)
	if err != nil {
		return LoaderEntry{}, err
	}
	rootUUID, err := cleanToken("root partition UUID", record.Root.PartitionUUID)
	if err != nil {
		return LoaderEntry{}, err
	}
	ukiPath, err := cleanUKIPath(record.Boot.UKIPath)
	if err != nil {
		return LoaderEntry{}, err
	}
	machineID, err := cleanMachineID(request.MachineID)
	if err != nil {
		return LoaderEntry{}, err
	}
	options, err := entryOptions(record, machineID, generationID, rootSlot, rootUUID)
	if err != nil {
		return LoaderEntry{}, err
	}

	title, err := cleanTitle(request.Title)
	if err != nil {
		return LoaderEntry{}, err
	}
	if title == "" {
		title = "Katl " + generationID
	}
	content := strings.Join([]string{
		"title " + title,
		"version " + runtimeVersion,
		"sort-key katl",
		"machine-id " + machineID,
		"efi " + entryPath(ukiPath),
		"options " + strings.Join(options, " "),
		"",
	}, "\n")

	return LoaderEntry{
		Name:    "katl-" + generationID + ".conf",
		Content: content,
	}, nil
}

func WriteEntry(root string, request LoaderRequest) (string, error) {
	entry, err := RenderEntry(request)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, "loader", "entries", entry.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create loader entry directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(entry.Content), 0o644); err != nil {
		return "", fmt.Errorf("write loader entry: %w", err)
	}
	return path, nil
}

func entryOptions(record Record, machineID string, generationID string, rootSlot string, rootUUID string) ([]string, error) {
	root := "root=PARTUUID=" + rootUUID
	machine := "systemd.machine_id=" + machineID
	generation := "katl.generation=" + generationID
	slot := "katl.root-slot=" + rootSlot
	base := []string{root, "rootfstype=squashfs", "ro", machine, generation, slot}
	extra := make([]string, 0, len(record.KernelCommandLine))
	for _, option := range record.KernelCommandLine {
		option, err := cleanOption(option)
		if err != nil {
			return nil, err
		}
		if option == "" {
			continue
		}
		switch {
		case strings.HasPrefix(option, "root="):
			if option != root {
				return nil, fmt.Errorf("loader root option %q does not match selected root PARTUUID", option)
			}
		case strings.HasPrefix(option, "rootfstype="):
			if option != "rootfstype=squashfs" {
				return nil, fmt.Errorf("loader rootfstype option %q is unsupported", option)
			}
		case option == "ro":
		case option == "rw":
			return nil, fmt.Errorf("loader option rw is unsupported")
		case strings.HasPrefix(option, "systemd.machine_id="):
			if option != machine {
				return nil, fmt.Errorf("loader machine-id option does not match install machine-id")
			}
		case strings.HasPrefix(option, "katl.generation="):
			if option != generation {
				return nil, fmt.Errorf("loader generation option does not match generation metadata")
			}
		case strings.HasPrefix(option, "katl.root-slot="):
			if option != slot {
				return nil, fmt.Errorf("loader root-slot option does not match generation metadata")
			}
		default:
			extra = append(extra, option)
		}
	}
	return append(base, extra...), nil
}

func cleanMachineID(machineID string) (string, error) {
	machineID = strings.TrimSpace(machineID)
	if len(machineID) != 32 {
		return "", fmt.Errorf("machine id must be 32 lowercase hex characters")
	}
	if machineID != strings.ToLower(machineID) {
		return "", fmt.Errorf("machine id must be lowercase hex")
	}
	if _, err := hex.DecodeString(machineID); err != nil {
		return "", fmt.Errorf("machine id is invalid: %w", err)
	}
	return machineID, nil
}

func cleanSegment(name string, value string) (string, error) {
	value, err := cleanToken(name, value)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(value, `/\`) || value == "." || value == ".." || filepath.Clean(value) != value {
		return "", fmt.Errorf("%s %q must be a single path segment", name, value)
	}
	return value, nil
}

func cleanToken(name string, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if strings.ContainsAny(value, " \t\n\r") {
		return "", fmt.Errorf("%s %q must not contain whitespace", name, value)
	}
	return value, nil
}

func cleanTitle(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, "\n\r") {
		return "", fmt.Errorf("loader title must not contain newlines")
	}
	return value, nil
}

func cleanOption(option string) (string, error) {
	option = strings.TrimSpace(option)
	if option == "" {
		return "", nil
	}
	if strings.ContainsAny(option, " \t\n\r") {
		return "", fmt.Errorf("loader option %q must not contain whitespace", option)
	}
	return option, nil
}

func cleanUKIPath(path string) (string, error) {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" {
		return "", fmt.Errorf("UKI path is required")
	}
	if strings.ContainsAny(path, " \t\n\r") {
		return "", fmt.Errorf("UKI path must not contain whitespace")
	}
	if !strings.HasPrefix(path, "/efi/EFI/Linux/") && !strings.HasPrefix(path, "/EFI/Linux/") {
		return "", fmt.Errorf("UKI path %q must be under /efi/EFI/Linux or /EFI/Linux", path)
	}
	if strings.Contains(path, "/../") || strings.HasSuffix(path, "/..") || filepath.Clean(path) != path {
		return "", fmt.Errorf("UKI path %q must be clean", path)
	}
	return path, nil
}

func entryPath(path string) string {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if strings.HasPrefix(path, "/efi/") {
		return strings.TrimPrefix(path, "/efi")
	}
	return path
}
