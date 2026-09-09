//go:build windows

package main

import (
	"os"
	"os/exec"
)

func runStandalone(executable string, args, environ []string) int {
	command := exec.Command(executable, args...)
	command.Env = environ
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		return 1
	}
	return 0
}
