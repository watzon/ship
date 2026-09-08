//go:build !unix

package transport

import "os/exec"

// Ship releases target Linux and Darwin. Keep ordinary CommandContext cleanup
// available when the transport package is built on other platforms.
func configureSSHStreamProcess(cmd *exec.Cmd) func() error {
	return func() error { return nil }
}
