//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

func watchTerminalResize(send func()) func() {
	changes := make(chan os.Signal, 1)
	signal.Notify(changes, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-changes:
				send()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(changes)
		close(done)
	}
}
