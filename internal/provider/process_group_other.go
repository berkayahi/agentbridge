//go:build !darwin && !linux

package provider

import (
	"os"
	"os/exec"
	"time"
)

func ConfigureProcessGroup(*exec.Cmd)     {}
func SweepProcessGroup(*os.Process) error { return nil }

func StopProcessGroup(process *os.Process, done <-chan struct{}, _ time.Duration) error {
	if process == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	err := process.Kill()
	<-done
	return err
}
