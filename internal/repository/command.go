package repository

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

// Command describes a trusted platform command. Arguments must never contain
// credentials; authentication is supplied by the runtime credential helper.
type Command struct {
	Name string
	Args []string
	Dir  string
	Env  []string
}

// CommandRunner makes Git operations deterministic and testable.
type CommandRunner interface {
	Output(context.Context, Command) ([]byte, error)
	Run(context.Context, Command) error
}

type ExecCommandRunner struct{}

func (ExecCommandRunner) Output(ctx context.Context, command Command) ([]byte, error) {
	process := exec.CommandContext(ctx, command.Name, command.Args...)
	process.Dir = command.Dir
	process.Env = command.Env
	if process.Env == nil {
		process.Env = os.Environ()
	}
	var stderr bytes.Buffer
	process.Stderr = &stderr
	output, err := process.Output()
	if err != nil {
		return nil, commandFailure(command.Name, stderr.String(), err)
	}
	return output, nil
}

func (ExecCommandRunner) Run(ctx context.Context, command Command) error {
	process := exec.CommandContext(ctx, command.Name, command.Args...)
	process.Dir = command.Dir
	process.Env = command.Env
	if process.Env == nil {
		process.Env = os.Environ()
	}
	var stderr bytes.Buffer
	process.Stderr = &stderr
	if err := process.Run(); err != nil {
		return commandFailure(command.Name, stderr.String(), err)
	}
	return nil
}

func commandFailure(name, stderr string, err error) error {
	if len(stderr) > 1024 {
		stderr = stderr[:1024]
	}
	if stderr != "" {
		return fmt.Errorf("%s failed: %s", name, bytes.TrimSpace([]byte(stderr)))
	}
	return fmt.Errorf("%s failed: %w", name, err)
}
