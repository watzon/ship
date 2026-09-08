//go:build unix

package transport

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

func configureSSHStreamProcess(cmd *exec.Cmd) func() error {
	// A jump host or ProxyCommand can create descendants. Give this command
	// its own group so cleanup cannot signal the caller's process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var once sync.Once
	var err error
	terminate := func() error {
		once.Do(func() {
			err = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				err = os.ErrProcessDone
			}
		})
		return err
	}
	cmd.Cancel = terminate
	return terminate
}
