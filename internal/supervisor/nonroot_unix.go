//go:build !windows

package supervisor

import (
	"errors"
	"os"
)

func EnsureNonRoot() error {
	if os.Geteuid() == 0 {
		return errors.New("devsandbox-agent refuses to run as root")
	}
	return nil
}
