//go:build unix

package shell

import (
	"os/exec"
	"syscall"
)

// startGroup puts the child in a new process group. Killing the group then
// reaches every descendant, including the ones a shell script forks.
func startGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup asks the whole group to stop. A group that is already gone
// is not an error: the command may have exited on its own.
func terminateGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// killGroup removes the whole group without asking.
func killGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
