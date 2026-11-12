//go:build !unix

package shell

import (
	"os"
	"os/exec"
)

// These platforms have no process group to signal, so a grandchild may
// outlive the command. go-mapping §8 keeps Windows out of phase one; this
// file only keeps the package buildable elsewhere.

func startGroup(cmd *exec.Cmd) {}

func terminateGroup(cmd *exec.Cmd) { _ = cmd.Process.Signal(os.Interrupt) }

func killGroup(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
