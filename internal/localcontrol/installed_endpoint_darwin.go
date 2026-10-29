//go:build darwin

package localcontrol

import (
	"golang.org/x/sys/unix"
	"net"
	"os"
)

func installedPeer(c *net.UnixConn) error {
	raw, e := c.SyscallConn()
	if e != nil {
		return errInstalledEndpoint
	}
	result := errInstalledEndpoint
	e = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err == nil && int(cred.Uid) == os.Geteuid() {
			result = nil
		}
	})
	if e != nil {
		return errInstalledEndpoint
	}
	return result
}
