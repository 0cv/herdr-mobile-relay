package doctor

import (
	"net"

	"golang.org/x/sys/unix"
)

func herdrPathSafetyAvailable() bool { return true }

// One fixed nonblocking receive-queue METADATA query rejects already buffered
// bytes after the frame, including exactly at a read boundary, WITHOUT reading
// extra response bytes beyond 64 KiB. No EOF wait or future-output guarantee.
func herdrBufferedEvidence(conn *net.UnixConn) Evidence {
	raw, err := conn.SyscallConn()
	if err != nil { return EvidenceUnavailable }
	evidence := EvidenceUnavailable
	err = raw.Control(func(fd uintptr) {
		queued, err := unix.IoctlGetInt(int(fd), unix.TIOCINQ)
		if err != nil || queued < 0 { return }
		if queued > 0 { evidence = EvidenceMalformed; return }
		evidence = EvidenceHerdrPing
	})
	if err != nil { return EvidenceUnavailable }
	return evidence
}

// Same effective UID only. Do not guess a PID or infer app/device/service
// authority from peer credentials. Path policy is separately checked first.
func herdrPeer(conn *net.UnixConn) bool {
	raw, err := conn.SyscallConn()
	if err != nil { return false }
	allowed := false
	err = raw.Control(func(fd uintptr) {
		credential, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		allowed = err == nil && credential != nil && herdrPeerUIDAllowed(credential.Uid)
	})
	return err == nil && allowed
}
