package cli

import "testing"

func TestCopilotEntryRunsStandaloneCLIInsideInteractiveShell(t *testing.T) {
	if got := string(entryInput("copilot")); got != "exec copilot\n" {
		t.Fatalf("Copilot entry input = %q", got)
	}
	if got := entryInput("shell"); got != nil {
		t.Fatalf("shell entry unexpectedly sent input %q", got)
	}
}
