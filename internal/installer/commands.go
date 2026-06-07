package installer

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CommandRunner is the boundary between typed installer state and host tools.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
}

type OutputCommandRunner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type ExecCommandRunner struct{}

func NewExecCommandRunner() ExecCommandRunner {
	return ExecCommandRunner{}
}

func (ExecCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	return commandError(name, args, output, err)
}

func (ExecCommandRunner) RunInput(ctx context.Context, input string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	return commandError(name, args, output, err)
}

func (ExecCommandRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

func commandError(name string, args []string, output []byte, err error) error {
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(output))
	if len(text) > 4000 {
		text = text[len(text)-4000:]
	}
	if text == "" {
		return err
	}
	return fmt.Errorf("%s %s: %w; output:\n%s", name, strings.Join(args, " "), err, text)
}

type NoopCommandRunner struct {
	Calls []CommandCall
}

type CommandCall struct {
	Name string
	Args []string
}

func (r *NoopCommandRunner) Run(_ context.Context, name string, args ...string) error {
	r.Calls = append(r.Calls, CommandCall{Name: name, Args: append([]string(nil), args...)})
	return nil
}
