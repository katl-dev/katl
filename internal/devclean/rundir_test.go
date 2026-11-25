package devclean

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRunDir(t *testing.T) {
	write := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("accepts a run directory with a result at the top", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "katl-vmtest", "run-1")
		write(t, filepath.Join(dir, "result.json"))

		got, err := CheckRunDir(dir)
		if err != nil || !got.Present || got.Path != dir {
			t.Fatalf("CheckRunDir() = %+v, %v; want present %s", got, err, dir)
		}
	})

	t.Run("accepts a run directory whose result is below it", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "katl-vmtest", "run-1")
		write(t, filepath.Join(dir, "nodes", "cp-1", "result.json"))

		if got, err := CheckRunDir(dir); err != nil || !got.Present {
			t.Fatalf("CheckRunDir() = %+v, %v; want present", got, err)
		}
	})

	t.Run("accepts a run directory that is already gone", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "katl-vmtest", "run-1")

		got, err := CheckRunDir(dir)
		if err != nil || got.Present {
			t.Fatalf("CheckRunDir() = %+v, %v; want absent and no error", got, err)
		}
	})

	refused := []struct {
		name  string
		setup func(t *testing.T) string
		want  string
	}{
		{
			name: "outside katl-vmtest",
			setup: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "projects", "run-1")
				write(t, filepath.Join(dir, "result.json"))
				return dir
			},
			want: "not named katl-vmtest",
		},
		{
			name: "the katl-vmtest parent itself",
			setup: func(t *testing.T) string {
				root := filepath.Join(t.TempDir(), "katl-vmtest")
				write(t, filepath.Join(root, "run-1", "result.json"))
				return root
			},
			want: "not named katl-vmtest",
		},
		{
			name: "a directory nested deeper than the run",
			setup: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "katl-vmtest", "run-1", "nodes")
				write(t, filepath.Join(dir, "result.json"))
				return dir
			},
			want: "not named katl-vmtest",
		},
		{
			name: "no result.json",
			setup: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "katl-vmtest", "mine")
				write(t, filepath.Join(dir, "notes.txt"))
				return dir
			},
			want: "no result.json",
		},
		{
			name: "a symbolic link to a run directory",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				real := filepath.Join(root, "elsewhere", "run-1")
				write(t, filepath.Join(real, "result.json"))
				link := filepath.Join(root, "katl-vmtest", "run-1")
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, link); err != nil {
					t.Fatal(err)
				}
				return link
			},
			want: "symbolic links are refused",
		},
		{
			name: "a file",
			setup: func(t *testing.T) string {
				file := filepath.Join(t.TempDir(), "katl-vmtest", "result.json")
				write(t, file)
				return file
			},
			want: "not a directory",
		},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			dir := tc.setup(t)

			_, err := CheckRunDir(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckRunDir(%s) error = %v; want it to contain %q", dir, err, tc.want)
			}
			if _, statErr := os.Lstat(dir); statErr != nil {
				t.Fatalf("a refused path must be left alone: %v", statErr)
			}
		})
	}
}
