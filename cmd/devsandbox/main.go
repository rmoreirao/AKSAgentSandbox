package main

import (
	"os"

	"github.com/rmoreirao/AKSAgentSandbox/internal/cli"
	"github.com/rmoreirao/AKSAgentSandbox/internal/version"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], cli.Options{Version: version.String()}))
}
