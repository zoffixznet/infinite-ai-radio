package acestep

import (
	"os/exec"
	"syscall"
)

// configureSidecar puts the child in its own process group and asks the
// kernel to kill it if this process dies, so a crash can never leave an
// orphaned GPU process behind.
func configureSidecar(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
}

// killProcessGroup force-kills the child's whole process group.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Negative PID addresses the process group created by Setpgid.
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
