package bootid

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRead(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
		fails               bool
	}{
		{name: "trims the newline", content: "0f4c8b8a-1d2e-4f3a-9b5c-6d7e8f9a0b1c\n", want: "0f4c8b8a-1d2e-4f3a-9b5c-6d7e8f9a0b1c"},
		{name: "empty", content: "\n", fails: true},
		{name: "missing", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.content != "" {
				file := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Read(root)
			if tc.fails != (err != nil) || got != tc.want {
				t.Fatalf("Read() = %q, %v; want %q, failure %t", got, err, tc.want, tc.fails)
			}
		})
	}
}
