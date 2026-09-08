//go:build windows

package cli

import (
	"sync"
	"time"
)

func watchTerminalResize(send func()) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				send()
			case <-done:
				return
			}
		}
	}()
	return func() {
		once.Do(func() { close(done) })
	}
}
