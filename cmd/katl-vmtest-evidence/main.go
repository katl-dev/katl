// katl-vmtest-evidence stages redacted copies of hosted VM diagnostics for upload.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/katl-dev/katl/internal/redact"
)

// The manifest and identity schemas and the management API use these names
// for string fields. Match the JSON string grammar to preserve escaped quotes.
var privateKeyField = regexp.MustCompile(`("(?:privateKey|serverPrivateKey|clientPrivateKey|server_private_key|client-key-data)"\s*:\s*)"(?:[^"\\]|\\.)*"`)

func redactEvidence(text string) string {
	text = redact.PrivateKeys(text)
	return privateKeyField.ReplaceAllString(text, `${1}"`+redact.PrivateKeyMarker+`"`)
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: katl-vmtest-evidence HOST_DIAGNOSTICS VM_RUN_DIR UPLOAD_DIR")
		os.Exit(2)
	}
	if err := collectEvidence(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "collect VM evidence:", err)
		os.Exit(1)
	}
}

func collectEvidence(hostDir, runDir, uploadDir string) error {
	// Require a fresh output directory so an upload can only contain files
	// that this collection pass has redacted.
	if err := os.Mkdir(uploadDir, 0o700); err != nil {
		return err
	}

	for _, source := range []struct {
		dir             string
		hostDiagnostics bool
	}{
		{dir: hostDir, hostDiagnostics: true},
		{dir: runDir},
	} {
		err := fs.WalkDir(os.DirFS(source.dir), ".", func(path string, entry fs.DirEntry, err error) error {
			// A job can fail before the VM run directory is created.
			if path == "." && os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if path != "." && strings.HasPrefix(entry.Name(), ".") {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() || (!source.hostDiagnostics && !isRunEvidence(path)) {
				return nil
			}

			data, err := os.ReadFile(filepath.Join(source.dir, path))
			if err != nil {
				return err
			}
			target := filepath.Join(uploadDir, filepath.Base(source.dir), path)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			return os.WriteFile(target, []byte(redactEvidence(string(data))), 0o600)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func isRunEvidence(path string) bool {
	switch filepath.Ext(path) {
	case ".log", ".json", ".xml", ".txt", ".stdout", ".stderr":
		return true
	}
	switch filepath.Base(path) {
	case "stdout", "stderr":
		return true
	}
	for _, directory := range strings.Split(filepath.Dir(path), "/") {
		if strings.HasSuffix(directory, "-network-evidence") {
			return true
		}
	}
	return false
}
