package configbundle

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

func validateHostDirectory(set SourceHostConfigurationFileSet) error {
	if set.Directory == "" || set.Destination == "" {
		return fmt.Errorf("directory and destination must be specified together")
	}
	if set.State != "" && set.State != manifest.HostConfigurationPresent {
		return fmt.Errorf("directory requires state present")
	}
	if len(set.Files) != 0 {
		return fmt.Errorf("use either directory or files, not both")
	}
	if !path.IsAbs(set.Destination) || path.Clean(set.Destination) != set.Destination {
		return fmt.Errorf("destination must be a normalized absolute path")
	}
	return nil
}

func expandHostDirectory(root, directory, destination string) ([]manifest.HostConfigurationFile, error) {
	source, err := hostSourcePath(root, directory)
	if err != nil {
		return nil, err
	}
	var files []manifest.HostConfigurationFile
	totalBytes := 0
	// WalkDir sorts entries and does not follow symlinks; every member is then
	// read through the same bounded, non-symlink source reader as explicit files.
	err = filepath.WalkDir(source, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == source && !entry.IsDir() {
			return fmt.Errorf("directory %q must be a directory", directory)
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		data, err := readHostConfigurationSource(root, filepath.Join(directory, relative))
		if err != nil {
			return err
		}
		totalBytes += len(data)
		if totalBytes > manifest.MaxHostConfigurationTotalBytes {
			return fmt.Errorf("file content exceeds the %d-byte host configuration limit", manifest.MaxHostConfigurationTotalBytes)
		}
		content := string(data)
		files = append(files, manifest.HostConfigurationFile{Path: path.Join(destination, filepath.ToSlash(relative)), Content: &content, Mode: 0o644})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("directory %q: %w", directory, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("directory %q contains no files; use state: absent to remove a file set", directory)
	}
	// Keep errors about disallowed destinations tied to the directory declaration.
	if err := manifest.ValidateHostConfiguration(manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{"directory": {Files: files}}}, false); err != nil {
		return nil, fmt.Errorf("directory %q: %s", directory, strings.TrimPrefix(err.Error(), `sets["directory"].`))
	}
	return files, nil
}
