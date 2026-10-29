//go:build darwin || linux

package conversation

import (
	"context"
	"errors"
	"os"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
	"golang.org/x/sys/unix"
)

func openConversationSource(path string) (file *os.File, err error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			err = errors.Join(err, unix.Close(fd))
		}
	}()
	file = os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, errors.New("conversation source could not be opened")
	}
	transferred = true
	return file, nil
}

func openInstalledConversationSource(ctx context.Context, owner *conversationlife.Owner, path string, onClose func(error)) (*sourceOwnership, error) {
	return acquireSourceRaw(ctx, owner, onClose, func() (func() error, func() (sourceHandle, error), error) {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, nil, err
		}
		return func() error { return unix.Close(fd) }, func() (sourceHandle, error) {
			file := os.NewFile(uintptr(fd), path)
			if file == nil {
				return nil, errors.New("conversation source could not be opened")
			}
			return file, nil
		}, nil
	})
}
