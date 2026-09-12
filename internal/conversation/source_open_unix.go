//go:build darwin || linux

package conversation

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openConversationSource(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("conversation source could not be opened")
	}
	return file, nil
}
