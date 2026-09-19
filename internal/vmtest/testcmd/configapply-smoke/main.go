package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var root, currentGeneration, nextGeneration, nodeName, manifestPath, requestPath, commandLog string
	flags := flag.NewFlagSet("configapply-smoke", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&root, "root", "", "runtime root")
	flags.StringVar(&currentGeneration, "current-generation", "", "current generation id; defaults to katl.generation from root/proc/cmdline")
	flags.StringVar(&nextGeneration, "next-generation", "", "candidate generation id")
	flags.StringVar(&nodeName, "node", "", "node name")
	flags.StringVar(&manifestPath, "manifest", "", "current install manifest JSON")
	flags.StringVar(&requestPath, "request", "", "NodeConfigurationChange YAML")
	flags.StringVar(&commandLog, "command-log", "", "optional JSON command log path")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if root == "" || nextGeneration == "" || nodeName == "" || manifestPath == "" || requestPath == "" {
		return fmt.Errorf("root, next-generation, node, manifest, and request are required")
	}
	if currentGeneration == "" {
		var err error
		currentGeneration, err = generationFromCmdline(root)
		if err != nil {
			return err
		}
	}

	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		return fmt.Errorf("open manifest: %w", err)
	}
	defer manifestFile.Close()
	currentManifest, err := manifest.Decode(manifestFile)
	if err != nil {
		return err
	}
	metadataPath, err := generation.MetadataPath(root, currentGeneration)
	if err != nil {
		return err
	}
	currentRecord, err := generation.ReadRecord(metadataPath)
	if err != nil {
		return err
	}
	requestFile, err := os.Open(requestPath)
	if err != nil {
		return fmt.Errorf("open request: %w", err)
	}
	defer requestFile.Close()
	result, err := configapply.ApplyNodeConfigurationChange(context.Background(), requestFile, configapply.TrustedBundleRequest{
		Root:            root,
		NodeName:        nodeName,
		GenerationID:    nextGeneration,
		CurrentManifest: currentManifest,
		CurrentRecord:   currentRecord,
		Executor: &configapply.Executor{
			Runner:    &loggingRunner{LogPath: commandLog},
			Activator: activationRunner{Root: root},
			Timeout:   30 * time.Second,
		},
		Chown: func(string, int, int) error { return nil },
	})
	if err != nil {
		return err
	}
	summary := struct {
		AuditPath    string `json:"auditPath"`
		MetadataPath string `json:"metadataPath,omitempty"`
		StatusPath   string `json:"statusPath,omitempty"`
	}{
		AuditPath:    result.AuditPath,
		MetadataPath: result.MetadataPath,
		StatusPath:   result.StatusPath,
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}

type activationRunner struct {
	Root string
}

func (r activationRunner) Activate(_ context.Context, record generation.Record) error {
	_, err := generation.ApplyActivation(r.Root, record)
	return err
}

func (r activationRunner) Rollback(_ context.Context, targetGenerationID string) error {
	path, err := generation.MetadataPath(r.Root, targetGenerationID)
	if err != nil {
		return err
	}
	record, err := generation.ReadRecord(path)
	if err != nil {
		return err
	}
	_, err = generation.ApplyActivation(r.Root, record)
	return err
}

type loggingRunner struct {
	LogPath string
}

func (r *loggingRunner) Run(ctx context.Context, command configapply.Command) (configapply.CommandResult, error) {
	if err := r.append(command); err != nil {
		return configapply.CommandResult{}, err
	}
	cmd := exec.CommandContext(ctx, command.Argv[0], command.Argv[1:]...)
	output, err := cmd.CombinedOutput()
	result := configapply.CommandResult{
		ExitStatus: 0,
		Stdout:     string(output),
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			result.ExitStatus = exit.ExitCode()
			result.Stderr = string(output)
			return result, nil
		}
		return result, err
	}
	return result, nil
}

func (r *loggingRunner) append(command configapply.Command) error {
	if r.LogPath == "" {
		return nil
	}
	var commands []configapply.Command
	data, err := os.ReadFile(r.LogPath)
	if err == nil {
		if err := json.Unmarshal(data, &commands); err != nil {
			return fmt.Errorf("decode command log: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	commands = append(commands, command)
	data, err = json.MarshalIndent(commands, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.LogPath, append(data, '\n'), 0o600)
}

func generationFromCmdline(root string) (string, error) {
	data, err := os.ReadFile(root + "/proc/cmdline")
	if err != nil {
		return "", fmt.Errorf("read proc cmdline: %w", err)
	}
	for _, field := range strings.Fields(string(data)) {
		if value, ok := strings.CutPrefix(field, "katl.generation="); ok && strings.TrimSpace(value) != "" {
			return value, nil
		}
	}
	return "", fmt.Errorf("katl.generation kernel argument is required")
}
