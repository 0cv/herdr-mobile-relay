package pibridge

import (
	"errors"
	"golang.org/x/sys/unix"
	"net"
	"os"
)

func checkPeer(conn *net.UnixConn, pid int) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peerErr error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		peerErr = e
		if e == nil && (int(cred.Uid) != os.Getuid() || int(cred.Pid) != pid) {
			peerErr = errors.New("bridge peer mismatch")
		}
	})
	if err != nil {
		return err
	}
	return peerErr
}
