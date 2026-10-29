//go:build !darwin && !linux

package conversation

import (
	"context"
	"os"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

func openConversationSource(path string) (*os.File, error) {
	return os.Open(path)
}

func openInstalledConversationSource(_ context.Context, owner *conversationlife.Owner, path string, onClose func(error)) (*sourceOwnership, error) {
	return acquireSourceHandle(owner, onClose, func() (sourceHandle, error) { return os.Open(path) })
}
