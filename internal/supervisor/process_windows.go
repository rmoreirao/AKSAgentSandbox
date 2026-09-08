//go:build windows

package supervisor

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

type terminalReadCloser struct {
	io.Reader
	io.Closer
}

const (
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
	processSuspendResume              = 0x0800
	createSuspended                   = 0x00000004
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	ntdll                    = syscall.NewLazyDLL("ntdll.dll")
	createJobObject          = kernel32.NewProc("CreateJobObjectW")
	setInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	assignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	terminateJobObject       = kernel32.NewProc("TerminateJobObject")
	openProcess              = kernel32.NewProc("OpenProcess")
	closeHandle              = kernel32.NewProc("CloseHandle")
	ntResumeProcess          = ntdll.NewProc("NtResumeProcess")
)

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type basicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type extendedLimitInformation struct {
	BasicLimitInformation basicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type processTree struct {
	cmd  *exec.Cmd
	job  syscall.Handle
	once sync.Once
}

func startProcess(cmd *exec.Cmd) (*processTree, error) {
	job, _, err := createJobObject.Call(0, 0)
	if job == 0 {
		return nil, fmt.Errorf("create Windows job object: %w", err)
	}

	handle := syscall.Handle(job)
	info := extendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	ok, _, setErr := setInformationJobObject.Call(
		job,
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if ok == 0 {
		closeHandle.Call(job)
		return nil, fmt.Errorf("configure Windows job object: %w", setErr)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	if err := cmd.Start(); err != nil {
		closeHandle.Call(job)
		return nil, err
	}
	process, _, openErr := openProcess.Call(
		processSetQuota|processTerminate|processSuspendResume,
		0,
		uintptr(uint32(cmd.Process.Pid)),
	)
	if process == 0 {
		_ = cmd.Process.Kill()
		closeHandle.Call(job)
		return nil, fmt.Errorf("open child process: %w", openErr)
	}
	ok, _, assignErr := assignProcessToJobObject.Call(job, process)
	if ok == 0 {
		_ = cmd.Process.Kill()
		closeHandle.Call(process)
		closeHandle.Call(job)
		return nil, fmt.Errorf("assign child process to job: %w", assignErr)
	}
	status, _, _ := ntResumeProcess.Call(process)
	if status != 0 {
		terminateJobObject.Call(job, 1)
		closeHandle.Call(process)
		closeHandle.Call(job)
		return nil, fmt.Errorf("resume child process: NTSTATUS %#x", status)
	}
	closeHandle.Call(process)
	return &processTree{cmd: cmd, job: handle}, nil
}

func startTerminalProcess(cmd *exec.Cmd, _ TerminalSize) (*processTree, io.WriteCloser, io.ReadCloser, func(TerminalSize) error, error) {
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return nil, nil, nil, nil, err
	}
	cmd.Stderr = cmd.Stdout
	tree, err := startProcess(cmd)
	if err != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, nil, nil, nil, err
	}
	return tree, input, output, func(TerminalSize) error { return nil }, nil
}

func (p *processTree) Wait(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		p.close()
		return err
	case <-ctx.Done():
		p.Terminate()
		err := <-done
		p.close()
		return err
	}
}

func (p *processTree) Terminate() {
	p.once.Do(func() {
		if p.job != 0 {
			terminateJobObject.Call(uintptr(p.job), 1)
		} else if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
}

func (p *processTree) close() {
	p.once.Do(func() {})
	if p.job != 0 {
		closeHandle.Call(uintptr(p.job))
		p.job = 0
	}
}
