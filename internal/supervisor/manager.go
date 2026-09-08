package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

type JobState string

const (
	JobPending   JobState = "Pending"
	JobRunning   JobState = "Running"
	JobCompleted JobState = "Completed"
	JobFailed    JobState = "Failed"
	JobStopped   JobState = "Stopped"
)

type Command struct {
	Executable  string            `json:"executable"`
	Arguments   []string          `json:"arguments,omitempty"`
	Directory   string            `json:"directory,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
}

func (c Command) validate() error {
	if c.Executable == "" {
		return errors.New("executable is required")
	}
	if c.Directory != "" {
		info, err := os.Stat(c.Directory)
		if err != nil || !info.IsDir() {
			return errors.New("command directory is unavailable")
		}
	}
	return nil
}

type ExitResult struct {
	ExitCode int  `json:"exitCode"`
	Canceled bool `json:"canceled,omitempty"`
}

type Job struct {
	ID        string     `json:"id"`
	State     JobState   `json:"state"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
}

type managedJob struct {
	summary Job
	cancel  context.CancelFunc
	done    chan struct{}
	tree    *processTree
}

type foregroundProcess struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type managedConnection struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type TerminalSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type Terminal struct {
	manager *Manager
	tree    *processTree
	process *foregroundProcess
	input   io.WriteCloser
	output  io.ReadCloser
	resize  func(TerminalSize) error
	ctx     context.Context
	cancel  context.CancelFunc
	once    sync.Once
	result  ExitResult
}

// AuditEvent intentionally contains lifecycle metadata only. Command arguments,
// environment, stdout, and stderr are never represented.
type AuditEvent struct {
	Action   string    `json:"action"`
	JobID    string    `json:"jobId,omitempty"`
	State    JobState  `json:"state,omitempty"`
	Time     time.Time `json:"time"`
	ExitCode *int      `json:"exitCode,omitempty"`
}

type ActivityStatus struct {
	LastActivity      time.Time `json:"lastActivity,omitempty"`
	ForegroundActive  int       `json:"foregroundActive"`
	ManagedJobsActive int       `json:"managedJobsActive"`
}

type Manager struct {
	mu           sync.RWMutex
	jobs         map[string]*managedJob
	foreground   map[*processTree]*foregroundProcess
	connections  map[uint64]*managedConnection
	nextConnect  uint64
	shuttingDown bool
	now          func() time.Time
	audit        func(AuditEvent)
	lastActivity time.Time
}

func NewManager(audit func(AuditEvent)) *Manager {
	return &Manager{
		jobs:        make(map[string]*managedJob),
		foreground:  make(map[*processTree]*foregroundProcess),
		connections: make(map[uint64]*managedConnection),
		now:         time.Now,
		audit:       audit,
	}
}

func (m *Manager) Activity() {
	m.mu.Lock()
	m.lastActivity = m.now().UTC()
	m.mu.Unlock()
}

func (m *Manager) LastActivity() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastActivity
}

func (m *Manager) ActivityStatus() ActivityStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := ActivityStatus{
		LastActivity:     m.lastActivity,
		ForegroundActive: len(m.foreground) + len(m.connections),
	}
	for _, job := range m.jobs {
		if job.summary.State == JobRunning || job.summary.State == JobPending {
			status.ManagedJobsActive++
		}
	}
	return status
}

func (m *Manager) ConnectionContext(parent context.Context) (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown {
		return nil, nil, errors.New("supervisor is shutting down")
	}
	ctx, cancel := context.WithCancel(parent)
	m.nextConnect++
	id := m.nextConnect
	connection := &managedConnection{cancel: cancel, done: make(chan struct{})}
	m.connections[id] = connection
	m.lastActivity = m.now().UTC()
	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			m.mu.Lock()
			delete(m.connections, id)
			m.lastActivity = m.now().UTC()
			close(connection.done)
			m.mu.Unlock()
		})
	}
	go func() {
		<-ctx.Done()
		release()
	}()
	return ctx, release, nil
}

func (m *Manager) Run(ctx context.Context, command Command, stdin io.Reader, stdout, stderr io.Writer) (ExitResult, error) {
	if err := command.validate(); err != nil {
		return ExitResult{}, err
	}

	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return ExitResult{}, errors.New("supervisor is shutting down")
	}
	runCtx, cancel := context.WithCancel(ctx)
	cmd := buildCommand(runCtx, command, stdin, stdout, stderr)
	tree, err := startProcess(cmd)
	if err != nil {
		cancel()
		m.mu.Unlock()
		return ExitResult{}, err
	}
	foreground := &foregroundProcess{cancel: cancel, done: make(chan struct{})}
	m.foreground[tree] = foreground
	m.lastActivity = m.now().UTC()
	m.mu.Unlock()

	err = tree.Wait(runCtx)
	m.mu.Lock()
	delete(m.foreground, tree)
	close(foreground.done)
	m.lastActivity = m.now().UTC()
	m.mu.Unlock()
	cancel()
	result := exitResult(err, runCtx.Err() != nil)
	return result, nil
}

func (m *Manager) StartTerminal(ctx context.Context, command Command, size TerminalSize) (*Terminal, error) {
	if err := command.validate(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return nil, errors.New("supervisor is shutting down")
	}
	runCtx, cancel := context.WithCancel(ctx)
	cmd := buildCommand(runCtx, command, nil, nil, nil)
	tree, input, output, resize, err := startTerminalProcess(cmd, size)
	if err != nil {
		cancel()
		m.mu.Unlock()
		return nil, err
	}
	foreground := &foregroundProcess{cancel: cancel, done: make(chan struct{})}
	m.foreground[tree] = foreground
	m.lastActivity = m.now().UTC()
	m.mu.Unlock()
	return &Terminal{
		manager: m, tree: tree, process: foreground, input: input, output: output,
		resize: resize, ctx: runCtx, cancel: cancel,
	}, nil
}

func (t *Terminal) Input() io.Writer  { return t.input }
func (t *Terminal) Output() io.Reader { return t.output }

func (t *Terminal) Resize(size TerminalSize) error {
	if size.Rows == 0 || size.Cols == 0 {
		return errors.New("terminal rows and columns must be positive")
	}
	return t.resize(size)
}

func (t *Terminal) Wait() ExitResult {
	t.once.Do(func() {
		err := t.tree.Wait(t.ctx)
		t.manager.mu.Lock()
		delete(t.manager.foreground, t.tree)
		close(t.process.done)
		t.manager.lastActivity = t.manager.now().UTC()
		t.manager.mu.Unlock()
		t.result = exitResult(err, t.ctx.Err() != nil)
		_ = t.input.Close()
		_ = t.output.Close()
		t.cancel()
	})
	return t.result
}

func (t *Terminal) Close() {
	t.cancel()
	t.tree.Terminate()
	_ = t.input.Close()
	_ = t.output.Close()
}

func (m *Manager) Start(command Command) (Job, error) {
	if err := command.validate(); err != nil {
		return Job{}, err
	}
	id, err := newJobID()
	if err != nil {
		return Job{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := buildCommand(ctx, command, nil, io.Discard, io.Discard)
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		cancel()
		return Job{}, errors.New("supervisor is shutting down")
	}
	started := m.now().UTC()
	job := &managedJob{
		summary: Job{ID: id, State: JobPending, StartedAt: started},
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	m.jobs[id] = job
	tree, err := startProcess(cmd)
	if err != nil {
		delete(m.jobs, id)
		m.mu.Unlock()
		cancel()
		return Job{}, err
	}
	job.tree = tree
	job.summary.State = JobRunning
	m.lastActivity = started
	summary := job.summary
	m.mu.Unlock()
	m.emit(AuditEvent{Action: "job.started", JobID: id, State: JobRunning, Time: started})

	go m.waitJob(ctx, job)
	return summary, nil
}

func (m *Manager) waitJob(ctx context.Context, job *managedJob) {
	err := job.tree.Wait(ctx)
	ended := m.now().UTC()
	result := exitResult(err, ctx.Err() != nil)
	m.mu.Lock()
	job.summary.EndedAt = &ended
	job.summary.ExitCode = &result.ExitCode
	switch {
	case result.Canceled:
		job.summary.State = JobStopped
	case result.ExitCode == 0:
		job.summary.State = JobCompleted
	default:
		job.summary.State = JobFailed
	}
	job.tree = nil
	job.cancel()
	m.lastActivity = ended
	event := AuditEvent{
		Action: "job.finished", JobID: job.summary.ID, State: job.summary.State,
		Time: ended, ExitCode: job.summary.ExitCode,
	}
	close(job.done)
	m.mu.Unlock()
	m.emit(event)
}

func (m *Manager) Jobs() []Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		result = append(result, cloneJob(job.summary))
	}
	sortJobs(result)
	return result
}

func (m *Manager) Job(id string) (Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return cloneJob(job.summary), true
}

func (m *Manager) StopJob(ctx context.Context, id string) (Job, error) {
	m.mu.RLock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.RUnlock()
		return Job{}, os.ErrNotExist
	}
	if job.summary.State != JobRunning && job.summary.State != JobPending {
		summary := cloneJob(job.summary)
		m.mu.RUnlock()
		return summary, nil
	}
	job.cancel()
	done := job.done
	m.mu.RUnlock()
	select {
	case <-done:
		summary, _ := m.Job(id)
		return summary, nil
	case <-ctx.Done():
		return Job{}, ctx.Err()
	}
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if !m.shuttingDown {
		m.shuttingDown = true
		for tree, process := range m.foreground {
			process.cancel()
			tree.Terminate()
		}
		for _, job := range m.jobs {
			if job.summary.State == JobRunning || job.summary.State == JobPending {
				job.cancel()
				if job.tree != nil {
					job.tree.Terminate()
				}
			}
		}
		for _, connection := range m.connections {
			connection.cancel()
		}
	}
	var wait []<-chan struct{}
	for _, process := range m.foreground {
		wait = append(wait, process.done)
	}
	for _, job := range m.jobs {
		if job.summary.State == JobRunning || job.summary.State == JobPending {
			wait = append(wait, job.done)
		}
	}
	for _, connection := range m.connections {
		wait = append(wait, connection.done)
	}
	m.mu.Unlock()
	for _, done := range wait {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func buildCommand(ctx context.Context, command Command, stdin io.Reader, stdout, stderr io.Writer) *exec.Cmd {
	cmd := exec.CommandContext(ctx, command.Executable, command.Arguments...)
	cmd.Dir = command.Directory
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(command.Environment) != 0 {
		cmd.Env = os.Environ()
		for key, value := range command.Environment {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	return cmd
}

func exitResult(err error, canceled bool) ExitResult {
	if err == nil {
		return ExitResult{ExitCode: 0, Canceled: canceled}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ExitResult{ExitCode: exitErr.ExitCode(), Canceled: canceled}
	}
	if canceled {
		if runtime.GOOS == "windows" {
			return ExitResult{ExitCode: 1, Canceled: true}
		}
		return ExitResult{ExitCode: 128 + 9, Canceled: true}
	}
	return ExitResult{ExitCode: 1}
}

func (m *Manager) emit(event AuditEvent) {
	if m.audit != nil {
		m.audit(event)
	}
}

func newJobID() (string, error) {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "job-" + stringsToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])), nil
}

func stringsToLower(value string) string {
	buf := []byte(value)
	for i, c := range buf {
		if c >= 'A' && c <= 'Z' {
			buf[i] = c + ('a' - 'A')
		}
	}
	return string(buf)
}

func cloneJob(job Job) Job {
	if job.EndedAt != nil {
		value := *job.EndedAt
		job.EndedAt = &value
	}
	if job.ExitCode != nil {
		value := *job.ExitCode
		job.ExitCode = &value
	}
	return job
}

func sortJobs(jobs []Job) {
	for i := 1; i < len(jobs); i++ {
		for j := i; j > 0 && jobs[j].StartedAt.Before(jobs[j-1].StartedAt); j-- {
			jobs[j], jobs[j-1] = jobs[j-1], jobs[j]
		}
	}
}
