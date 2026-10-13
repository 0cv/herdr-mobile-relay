//go:build !darwin && !linux

package panesize

// Direct PTY resizing is implemented for darwin and linux only. Elsewhere the
// manager reports itself unavailable and the relay does not advertise it.
const platformSupported = false

type ioctlTerminalOps struct{}

func (ioctlTerminalOps) Open(string) (Terminal, error) {
	return nil, ErrUnsupportedOS
}
