package acestep

import (
	"os/exec"
	"syscall"
)

// detach puts the daemon in its own session so it survives the client
// process and never receives its terminal signals.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
