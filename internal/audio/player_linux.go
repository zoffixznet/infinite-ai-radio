package audio

import (
	"os/exec"
	"syscall"
)

// bindToParent ties a helper child process (the command-line player)
// to this process's lifetime so it can never outlive us.
func bindToParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
