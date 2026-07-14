//go:build darwin

package controlsocket

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func validatePeerUID(conn *net.UnixConn, expected int) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var credential *unix.Xucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return err
	}
	if socketErr != nil {
		return socketErr
	}
	if int(credential.Uid) != expected {
		return fmt.Errorf("peer uid mismatch")
	}
	return nil
}
