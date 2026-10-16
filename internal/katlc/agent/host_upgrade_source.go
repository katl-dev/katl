package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
)

func completeUpgradeSourceConfiguration(snapshot, host, source string) error {
	if _, err := configapply.ReadLocalGenerationConfiguration(snapshot, source); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if filepath.Clean(snapshot) == filepath.Clean(host) {
		return fmt.Errorf("upgrade source configuration requires a private snapshot")
	}
	// Published sources exported only the selected generation. Their sandbox
	// leaves host state readable; anchor that view before filling private inputs.
	for _, root := range []string{snapshot, host} {
		selected, err := currentGenerationID(root)
		if err != nil {
			return err
		}
		if selected != source {
			return fmt.Errorf("upgrade source %q does not match current generation %q", source, selected)
		}
	}
	privateSpec, _, err := generation.ReadGeneration(snapshot, source)
	if err != nil {
		return err
	}
	liveSpec, _, err := generation.ReadGeneration(host, source)
	if err != nil {
		return err
	}
	privateDigest, err := generation.CanonicalSpecDigest(privateSpec)
	if err != nil {
		return err
	}
	liveDigest, err := generation.CanonicalSpecDigest(liveSpec)
	if err != nil {
		return err
	}
	if privateDigest != liveDigest {
		return fmt.Errorf("upgrade source generation differs from the host")
	}
	privateID, err := os.ReadFile(filepath.Join(runtimeRoot(snapshot), "etc/machine-id"))
	if err != nil {
		return err
	}
	liveID, err := os.ReadFile(filepath.Join(runtimeRoot(host), "etc/machine-id"))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(privateID)) == 0 || !bytes.Equal(bytes.TrimSpace(privateID), bytes.TrimSpace(liveID)) {
		return fmt.Errorf("upgrade source machine identity differs from the host")
	}
	configuration, err := configapply.ReadGenerationConfiguration(host, source)
	if err != nil {
		return err
	}
	remaining := maxHostUpgradePreparationInput
	if err := filepath.WalkDir(snapshot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || uint64(info.Size()) > remaining {
			return fmt.Errorf("upgrade preparation input exceeds its supported size or file types")
		}
		remaining -= uint64(info.Size())
		return nil
	}); err != nil {
		return err
	}
	// Only the private generation is writable. The frozen source and its
	// durable configuration remain untouched throughout target preparation.
	return materializeUpgradeSourceConfiguration(snapshot, source, configuration, remaining, filesystemAvailable)
}

func materializeUpgradeSourceConfiguration(root, source string, configuration configapply.GenerationConfiguration, remaining uint64, available availableStorageFunc) error {
	size, err := configuration.SizeBytes()
	if err != nil {
		return err
	}
	if size > remaining {
		return fmt.Errorf("upgrade source configuration exceeds the remaining preparation input budget")
	}
	if err := requireHostUpgradeStorage(available, root, size, "upgrade source configuration"); err != nil {
		return err
	}
	return configapply.WriteGenerationConfiguration(root, source, configuration)
}
