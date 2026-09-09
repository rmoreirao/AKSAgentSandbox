package main

import (
	"context"
	"fmt"
	"os"

	"github.com/rmoreirao/AKSAgentSandbox/internal/copilotruntime"
)

const standaloneOpenCode = "/usr/local/libexec/opencode"

func main() {
	config := copilotruntime.Config{TokenFile: os.Getenv("DEVSANDBOX_GITHUB_TOKEN_FILE")}
	config.OpenCodeRuntimeDir = os.Getenv("DEVSANDBOX_OPENCODE_RUNTIME_DIR")
	environ, err := config.PrepareOpenCode(context.Background(), os.Args[1:], os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(runStandalone(standaloneOpenCode, os.Args[1:], environ))
}
