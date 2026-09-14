//go:build !darwin && !linux

package conversation

import "os"

func openConversationSource(path string) (*os.File, error) {
	return os.Open(path)
}
