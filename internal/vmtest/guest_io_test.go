package vmtest

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeGuestFile(t *testing.T, ctx context.Context, guest *GuestControl, path string, content []byte, mode fs.FileMode) {
	t.Helper()
	if _, err := guest.WriteFile(ctx, GuestFileRequest{
		Name:     filepath.Base(path),
		Path:     path,
		Content:  content,
		Mode:     mode,
		Truncate: true,
	}); err != nil {
		t.Fatalf("write guest file %s: %v", path, err)
	}
}

func guestCommand(t *testing.T, ctx context.Context, guest *GuestControl, name string, argv ...string) GuestCommandArtifact {
	t.Helper()
	record, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name:         name,
		Argv:         argv,
		Timeout:      2 * time.Minute,
		StdoutLimit:  1 << 20,
		StderrLimit:  1 << 20,
		AllowFailure: false,
	})
	if err != nil {
		t.Fatalf("%s failed: %v\nstdout:\n%s\nstderr:\n%s", name, err, readOptionalFile(t, record.Stdout), readOptionalFile(t, record.Stderr))
	}
	return record
}

func guestCommandOutput(t *testing.T, ctx context.Context, guest *GuestControl, name string, argv ...string) string {
	t.Helper()
	record := guestCommand(t, ctx, guest, name, argv...)
	return readFile(t, record.Stdout)
}

func readOptionalFile(t *testing.T, path string) string {
	t.Helper()
	if path == "" {
		return ""
	}
	return readFile(t, path)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
