package main

import (
	"context"
	"fmt"
	"os"

	"github.com/rmoreirao/AKSAgentSandbox/internal/copilotruntime"
)

const standaloneCopilot = "/usr/local/libexec/copilot"

func main() {
	config := copilotruntime.Config{TokenFile: os.Getenv("DEVSANDBOX_GITHUB_TOKEN_FILE")}
	environ, err := config.Prepare(context.Background(), os.Args[1:], os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(runStandalone(standaloneCopilot, os.Args[1:], environ))
}
