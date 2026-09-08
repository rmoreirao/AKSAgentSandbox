package supervisor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunPreservesBinaryAndTextStreams(t *testing.T) {
	manager := NewManager(nil)
	var stdout, stderr bytes.Buffer
	result, err := manager.Run(context.Background(), helperCommand("binary"), nil, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !bytes.Equal(stdout.Bytes(), []byte{0x00, 0xff, 'O', 'K'}) ||
		stderr.String() != "text-stderr" {
		t.Fatalf("result=%+v stdout=%v stderr=%q", result, stdout.Bytes(), stderr.String())
	}
}

func TestTerminalResize(t *testing.T) {
	var got TerminalSize
	terminal := &Terminal{resize: func(size TerminalSize) error {
		got = size
		return nil
	}}
	if err := terminal.Resize(TerminalSize{Rows: 41, Cols: 132}); err != nil {
		t.Fatal(err)
	}
	if got.Rows != 41 || got.Cols != 132 {
		t.Fatalf("resize = %+v", got)
	}
	if err := terminal.Resize(TerminalSize{}); err == nil {
		t.Fatal("zero terminal size was accepted")
	}
}

func TestConnectionCancellationRemovesActivityAndShutdownWaits(t *testing.T) {
	manager := NewManager(nil)
	parent, cancel := context.WithCancel(context.Background())
	connection, _, err := manager.ConnectionContext(parent)
	if err != nil {
		t.Fatal(err)
	}
	if manager.ActivityStatus().ForegroundActive != 1 {
		t.Fatal("connection was not counted as active")
	}
	cancel()
	select {
	case <-connection.Done():
	case <-time.After(time.Second):
		t.Fatal("connection was not canceled")
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if manager.ActivityStatus().ForegroundActive != 0 {
		t.Fatal("canceled connection was not removed")
	}
}

func TestArbitraryBackgroundProcessIsNotManaged(t *testing.T) {
	manager := NewManager(nil)
	tick := filepath.Join(t.TempDir(), "background-tick")
	result, err := manager.Run(context.Background(), helperCommand("background", tick), nil, ioDiscard{}, ioDiscard{})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if status := manager.ActivityStatus(); status.ManagedJobsActive != 0 || status.ForegroundActive != 0 {
		t.Fatalf("background process counted as managed activity: %+v", status)
	}
	if before, statErr := os.Stat(tick); statErr == nil {
		time.Sleep(150 * time.Millisecond)
		after, err := os.Stat(tick)
		if err == nil && !after.ModTime().Equal(before.ModTime()) {
			t.Fatal("unmanaged background child survived its foreground process group")
		}
	}
}

func TestDetachedJobSurvivesCallerDisconnect(t *testing.T) {
	manager := NewManager(nil)
	caller, disconnect := context.WithCancel(context.Background())
	job, err := manager.Start(helperCommand("block"))
	if err != nil {
		t.Fatal(err)
	}
	disconnect()
	<-caller.Done()
	time.Sleep(50 * time.Millisecond)
	current, ok := manager.Job(job.ID)
	if !ok || current.State != JobRunning {
		t.Fatalf("job after caller disconnect = %+v, found=%v", current, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := manager.StopJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
}
