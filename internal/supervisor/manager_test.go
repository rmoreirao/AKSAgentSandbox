package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SUPERVISOR_HELPER") != "1" {
		return
	}
	separator := 0
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index + 1
			break
		}
	}
	arguments := os.Args[separator:]
	switch arguments[0] {
	case "exit":
		code, _ := strconv.Atoi(arguments[1])
		os.Exit(code)
	case "stream":
		_, _ = os.Stdout.Write([]byte("stdout-value"))
		_, _ = os.Stderr.Write([]byte("stderr-value"))
	case "binary":
		_, _ = os.Stdout.Write([]byte{0x00, 0xff, 'O', 'K'})
		_, _ = os.Stderr.Write([]byte("text-stderr"))
	case "block":
		for {
			time.Sleep(time.Hour)
		}
	case "tree":
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "tick", arguments[1])
		child.Env = append(os.Environ(), "GO_WANT_SUPERVISOR_HELPER=1")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "background":
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "tick", arguments[1])
		child.Env = append(os.Environ(), "GO_WANT_SUPERVISOR_HELPER=1")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "tick":
		for {
			_ = os.WriteFile(arguments[1], []byte(time.Now().Format(time.RFC3339Nano)), 0o600)
			time.Sleep(20 * time.Millisecond)
		}
	}
	os.Exit(0)
}

func helperCommand(mode string, arguments ...string) Command {
	return Command{
		Executable: os.Args[0],
		Arguments:  append([]string{"-test.run=TestHelperProcess", "--", mode}, arguments...),
		Environment: map[string]string{
			"GO_WANT_SUPERVISOR_HELPER": "1",
		},
	}
}

func TestRunPropagatesExitCodeAndStreams(t *testing.T) {
	manager := NewManager(nil)
	result, err := manager.Run(context.Background(), helperCommand("exit", "23"), nil, ioDiscard{}, ioDiscard{})
	if err != nil {
		t.Fatal(err)
	}

	if result.ExitCode != 23 {
		t.Fatalf("exit code = %d, want 23", result.ExitCode)
	}

	var stdout, stderr bytes.Buffer
	result, err = manager.Run(context.Background(), helperCommand("stream"), nil, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || stdout.String() != "stdout-value" || stderr.String() != "stderr-value" {
		t.Fatalf("result=%+v stdout=%q stderr=%q", result, stdout.String(), stderr.String())
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(data []byte) (int, error) { return len(data), nil }

func TestForegroundCancellation(t *testing.T) {
	manager := NewManager(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := manager.Run(ctx, helperCommand("block"), nil, ioDiscard{}, ioDiscard{})
	if err != nil {
		t.Fatal(err)
	}

	if !result.Canceled || time.Since(started) > 5*time.Second {
		t.Fatalf("cancellation result=%+v duration=%v", result, time.Since(started))
	}
}

func TestDetachedJobLifecycleAndAuditRedaction(t *testing.T) {
	var mu sync.Mutex
	var events []AuditEvent
	manager := NewManager(func(event AuditEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})
	command := helperCommand("exit", "7")
	command.Arguments = append(command.Arguments, "very-secret-command")
	command.Environment["SECRET"] = "very-secret-output"
	job, err := manager.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(job.ID, "job-") || job.State != JobRunning {
		t.Fatalf("started job = %+v", job)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, _ = manager.Job(job.ID)
		if job.State != JobRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.State != JobFailed || job.ExitCode == nil || *job.ExitCode != 7 {
		t.Fatalf("finished job = %+v", job)
	}
	jobs := manager.Jobs()
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("stable jobs = %+v", jobs)
	}
	mu.Lock()
	payload, err := json.Marshal(events)
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte("very-secret")) || bytes.Contains(payload, []byte("stdout")) {
		t.Fatalf("audit payload contains command or output: %s", payload)
	}
}

func TestStopDetachedJobAndChildTree(t *testing.T) {
	manager := NewManager(nil)
	tick := filepath.Join(t.TempDir(), "child-tick")
	job, err := manager.Start(helperCommand("tree", tick))
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(tick); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child process did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stopped, err := manager.StopJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != JobStopped {
		t.Fatalf("job state = %s, want Stopped", stopped.State)
	}
	time.Sleep(100 * time.Millisecond)
	before, err := os.Stat(tick)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	after, err := os.Stat(tick)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("child process survived managed-job cancellation")
	}
}
