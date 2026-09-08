//go:build !windows

package supervisor

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

type processTree struct {
	cmd  *exec.Cmd
	pgid int
	once sync.Once
}

func startProcess(cmd *exec.Cmd) (*processTree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &processTree{cmd: cmd, pgid: cmd.Process.Pid}, nil
}

func startTerminalProcess(cmd *exec.Cmd, size TerminalSize) (*processTree, io.WriteCloser, io.ReadCloser, func(TerminalSize) error, error) {
	window := &pty.Winsize{Rows: size.Rows, Cols: size.Cols}
	if window.Rows == 0 {
		window.Rows = 24
	}
	if window.Cols == 0 {
		window.Cols = 80
	}
	terminal, err := pty.StartWithSize(cmd, window)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	tree := &processTree{cmd: cmd, pgid: cmd.Process.Pid}
	resize := func(value TerminalSize) error {
		return pty.Setsize(terminal, &pty.Winsize{Rows: value.Rows, Cols: value.Cols})
	}
	return tree, terminal, terminal, resize, nil
}

func (p *processTree) Wait(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		p.Terminate()
		return err
	case <-ctx.Done():
		p.Terminate()
		return <-done
	}
}

func (p *processTree) Terminate() {
	p.once.Do(func() {
		if p.cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
	})
}
