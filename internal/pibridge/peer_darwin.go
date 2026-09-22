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
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		peerErr = e
		if e == nil && int(cred.Uid) != os.Getuid() {
			peerErr = errors.New("bridge peer mismatch")
		}
		if peerErr != nil {
			return
		}
		peerPID, e := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		peerErr = e
		if e == nil && peerPID != pid {
			peerErr = errors.New("bridge process mismatch")
		}
	})
	if err != nil {
		return err
	}
	return peerErr
}
