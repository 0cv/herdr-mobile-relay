package doctor

import "net"

// Darwin uid/mode and peer UID cannot establish path ACL writer safety. Keep
// unavailable BEFORE connecting; no Linux-only peer API, ACL subprocess,
// permission mutation or override. This is not native diagnostic qualification.
func herdrPathSafetyAvailable() bool               { return false }
func herdrPeer(*net.UnixConn) bool                 { return false }
func herdrBufferedEvidence(*net.UnixConn) Evidence { return EvidenceUnavailable }
