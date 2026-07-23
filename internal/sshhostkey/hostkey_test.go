package sshhostkey

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsurePreservesValidKeyAndRepairsPublicKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "host-keys", "ssh_host_ed25519_key")
	writeTestFile(t, keyPath, "valid:stable\n", 0o600)
	writeTestFile(t, keyPath+".pub", "stale\n", 0o644)
	runner := &fixtureRunner{}

	result, err := Ensure(context.Background(), Options{KeyPath: keyPath, Runner: runner})
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if result.Replaced {
		t.Fatal("Ensure() replaced a valid private key")
	}
	if runner.generations != 0 {
		t.Fatalf("ssh-keygen generation calls = %d, want 0", runner.generations)
	}
	assertTestFile(t, keyPath, "valid:stable\n", 0o600)
	assertTestFile(t, keyPath+".pub", "ssh-ed25519 stable\n", 0o644)
}

func TestEnsureGeneratesMissingKeyAtomically(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "host-keys", "ssh_host_ed25519_key")
	runner := &fixtureRunner{}

	result, err := Ensure(context.Background(), Options{KeyPath: keyPath, Runner: runner})
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if !result.Replaced {
		t.Fatal("Ensure() did not report replacing a missing key")
	}
	if runner.generations != 1 {
		t.Fatalf("ssh-keygen generation calls = %d, want 1", runner.generations)
	}
	assertTestFile(t, keyPath, "valid:generated\n", 0o600)
	assertTestFile(t, keyPath+".pub", "ssh-ed25519 generated\n", 0o644)
}

func TestEnsureReplacesInvalidKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "host-keys", "ssh_host_ed25519_key")
	writeTestFile(t, keyPath, "interrupted key generation\n", 0o600)
	runner := &fixtureRunner{}

	result, err := Ensure(context.Background(), Options{KeyPath: keyPath, Runner: runner})
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if !result.Replaced {
		t.Fatal("Ensure() did not report replacing an invalid key")
	}
	assertTestFile(t, keyPath, "valid:generated\n", 0o600)
	assertTestFile(t, keyPath+".pub", "ssh-ed25519 generated\n", 0o644)
}

func TestEnsureGenerationFailurePreservesInvalidDestination(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "host-keys", "ssh_host_ed25519_key")
	writeTestFile(t, keyPath, "interrupted key generation\n", 0o600)
	runner := &fixtureRunner{generationError: errors.New("injected generation failure")}

	_, err := Ensure(context.Background(), Options{KeyPath: keyPath, Runner: runner})
	if err == nil || !strings.Contains(err.Error(), "injected generation failure") {
		t.Fatalf("Ensure() error = %v, want generation failure", err)
	}
	assertTestFile(t, keyPath, "interrupted key generation\n", 0o600)
}

type fixtureRunner struct {
	generations     int
	generationError error
}

func (runner *fixtureRunner) CombinedOutput(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) >= 3 && args[0] == "-y" && args[1] == "-f" {
		data, err := os.ReadFile(args[2])
		if err != nil {
			return nil, err
		}
		value := strings.TrimSpace(string(data))
		if !strings.HasPrefix(value, "valid:") {
			return []byte("invalid format"), errors.New("invalid key")
		}
		return []byte("ssh-ed25519 " + strings.TrimPrefix(value, "valid:") + "\n"), nil
	}
	if len(args) >= 7 && args[0] == "-q" && args[1] == "-t" && args[2] == "ed25519" && args[5] == "-f" {
		runner.generations++
		if runner.generationError != nil {
			if err := os.WriteFile(args[6], []byte("partial generated key\n"), 0o600); err != nil {
				return nil, err
			}
			return []byte("generation failed"), runner.generationError
		}
		if err := os.WriteFile(args[6], []byte("valid:generated\n"), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(args[6]+".pub", []byte("ssh-ed25519 generated\n"), 0o644); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return nil, errors.New("unexpected ssh-keygen invocation")
}

func writeTestFile(t *testing.T, path, value string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
}

func assertTestFile(t *testing.T, path, want string, wantMode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != wantMode {
		t.Fatalf("%s mode = %o, want %o", path, got, wantMode)
	}
}
