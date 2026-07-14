//go:build !darwin && !linux

package controlsocket

import (
	"net"
	"runtime"
)

func validatePeerUID(*net.UnixConn, int) error {
	return &net.OpError{Op: "peer credential validation unsupported", Net: runtime.GOOS}
}
