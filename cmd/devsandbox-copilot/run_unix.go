//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

func runStandalone(executable string, args, environ []string) int {
	if err := syscall.Exec(executable, append([]string{executable}, args...), environ); err != nil {
		fmt.Fprintln(os.Stderr, "Copilot CLI could not be started")
		return 1
	}
	return 0
}
