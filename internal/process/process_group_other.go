//go:build !darwin && !linux

package process

import (
	"os"
	"os/exec"
)

func configureProcessGroup(cmd *exec.Cmd)             {}
func interruptProcessGroup(process *os.Process) error { return process.Interrupt() }
func killProcessGroup(process *os.Process) error      { return process.Kill() }
