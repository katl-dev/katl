package sshhostkey

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	DefaultKeyPath    = "/var/lib/katl/ssh/host-keys/ssh_host_ed25519_key"
	DefaultKeygenPath = "/usr/bin/ssh-keygen"
)

type CommandRunner interface {
	CombinedOutput(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type Options struct {
	KeyPath    string
	KeygenPath string
	Runner     CommandRunner
}

type Result struct {
	Replaced bool
}

func Ensure(ctx context.Context, options Options) (Result, error) {
	keyPath := strings.TrimSpace(options.KeyPath)
	if keyPath == "" {
		keyPath = DefaultKeyPath
	}
	keygenPath := strings.TrimSpace(options.KeygenPath)
	if keygenPath == "" {
		keygenPath = DefaultKeygenPath
	}
	runner := options.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	parent := filepath.Dir(keyPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Result{}, fmt.Errorf("create host-key directory: %w", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return Result{}, fmt.Errorf("secure host-key directory: %w", err)
	}

	publicKey, err := validate(ctx, runner, keygenPath, keyPath)
	if err == nil {
		if err := writePublicKey(keyPath+".pub", publicKey); err != nil {
			return Result{}, err
		}
		return Result{}, nil
	}

	temporaryDirectory, err := os.MkdirTemp(parent, ".katl-ssh-host-key-")
	if err != nil {
		return Result{}, fmt.Errorf("create temporary host-key directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	if err := os.Chmod(temporaryDirectory, 0o700); err != nil {
		return Result{}, fmt.Errorf("secure temporary host-key directory: %w", err)
	}
	temporaryKey := filepath.Join(temporaryDirectory, filepath.Base(keyPath))
	output, err := runner.CombinedOutput(ctx, keygenPath, "-q", "-t", "ed25519", "-N", "", "-f", temporaryKey)
	if err != nil {
		return Result{}, fmt.Errorf("generate host key: %w: %s", err, strings.TrimSpace(string(output)))
	}
	publicKey, err = validate(ctx, runner, keygenPath, temporaryKey)
	if err != nil {
		return Result{}, fmt.Errorf("validate generated host key: %w", err)
	}
	if err := os.Chmod(temporaryKey, 0o600); err != nil {
		return Result{}, fmt.Errorf("secure generated host key: %w", err)
	}
	if err := syncFile(temporaryKey); err != nil {
		return Result{}, fmt.Errorf("sync generated host key: %w", err)
	}
	if err := os.Rename(temporaryKey, keyPath); err != nil {
		return Result{}, fmt.Errorf("install generated host key: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return Result{}, fmt.Errorf("sync host-key directory: %w", err)
	}
	if err := writePublicKey(keyPath+".pub", publicKey); err != nil {
		return Result{}, err
	}
	return Result{Replaced: true}, nil
}

func validate(ctx context.Context, runner CommandRunner, keygenPath, keyPath string) (string, error) {
	output, err := runner.CombinedOutput(ctx, keygenPath, "-y", "-f", keyPath)
	if err != nil {
		return "", fmt.Errorf("validate %s: %w: %s", keyPath, err, strings.TrimSpace(string(output)))
	}
	publicKey := strings.TrimSpace(string(output))
	if publicKey == "" {
		return "", fmt.Errorf("validate %s: ssh-keygen returned an empty public key", keyPath)
	}
	if !strings.HasPrefix(publicKey, "ssh-ed25519 ") {
		return "", fmt.Errorf("validate %s: ssh-keygen returned a non-Ed25519 public key", keyPath)
	}
	return publicKey, nil
}

func writePublicKey(path, publicKey string) error {
	parent := filepath.Dir(path)
	temporary, err := os.CreateTemp(parent, ".katl-ssh-host-public-key-")
	if err != nil {
		return fmt.Errorf("create temporary public key: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return fmt.Errorf("set public-key permissions: %w", err)
	}
	if _, err := temporary.WriteString(strings.TrimSpace(publicKey) + "\n"); err != nil {
		temporary.Close()
		return fmt.Errorf("write public key: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync public key: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close public key: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install public key: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync public-key directory: %w", err)
	}
	return nil
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
