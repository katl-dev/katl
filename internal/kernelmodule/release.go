package kernelmodule

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func validateExtensionRelease(root, imageName, version, runtimeInterface, architecture string) error {
	if imageName == "" || filepath.Base(imageName) != imageName || !strings.HasSuffix(imageName, ".raw") {
		return fmt.Errorf("extension activation filename must be a safe .raw image name")
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer directory.Close()
	path := "usr/lib/extension-release.d/extension-release." + strings.TrimSuffix(imageName, ".raw")
	data, err := directory.ReadFile(path)
	if err != nil {
		return fmt.Errorf("extension %q release metadata: %w", imageName, err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !slices.Contains([]string{"ID", "SYSEXT_LEVEL", "VERSION_ID", "ARCHITECTURE", "SYSEXT_SCOPE"}, key) {
			continue
		}
		value, err := releaseValue(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("extension %q %s: %w", imageName, key, err)
		}
		values[key] = value
	}
	if id := values["ID"]; id != "_any" {
		if id != "katlos" {
			return fmt.Errorf("extension %q targets OS %q, not KatlOS", imageName, id)
		}
		if level, exists := values["SYSEXT_LEVEL"]; exists {
			if level != runtimeInterface {
				return fmt.Errorf("extension %q targets runtime interface %q, not %q", imageName, level, runtimeInterface)
			}
		} else if values["VERSION_ID"] == "" || values["VERSION_ID"] != version {
			return fmt.Errorf("extension %q VERSION_ID does not match target KatlOS %q", imageName, version)
		}
	}
	if value := values["ARCHITECTURE"]; value != "" && value != "_any" && value != architecture {
		return fmt.Errorf("extension %q architecture %q does not match target %q", imageName, value, architecture)
	}
	if scope, exists := values["SYSEXT_SCOPE"]; exists && !slices.Contains(strings.Fields(scope), "system") {
		return fmt.Errorf("extension %q is not available in the system scope", imageName)
	}
	return nil
}

// os-release permits quoted assignments and shell-style escapes, but neither
// expansion nor concatenated quoted strings. Never execute metadata as shell.
func releaseValue(value string) (string, error) {
	var result strings.Builder
	var quote byte
	if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
		quote, value = value[0], value[1:]
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if quote != 0 && c == quote {
			if strings.TrimSpace(value[i+1:]) != "" {
				return "", fmt.Errorf("concatenated release values are unsupported")
			}
			return result.String(), nil
		}
		if c == '\\' && quote != '\'' {
			if i+1 == len(value) {
				return "", fmt.Errorf("unterminated release escape")
			}
			if quote == 0 || strings.ContainsRune("$`\"\\", rune(value[i+1])) {
				i++
				c = value[i]
			}
		} else if quote == 0 && (c == '\'' || c == '"') {
			return "", fmt.Errorf("concatenated release values are unsupported")
		}
		result.WriteByte(c)
	}
	if quote != 0 {
		return "", fmt.Errorf("unterminated release quote")
	}
	return result.String(), nil
}
