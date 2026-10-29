package upload

import (
	"errors"
	"io/fs"
	"os"
)

// Called under attachmentMu: pending refs reserve this attempt's earlier objects.
// Installed mode refuses even unindexed destinations rather than adopting them.
func (m *Manager) finishRefAvailable(ref, path string, pending []*attachmentRecord) bool {
	if m.installed == nil {
		return true
	}
	m.mu.Lock()
	_, exists := m.attachments[ref]
	m.mu.Unlock()
	if exists {
		return false
	}
	for _, record := range pending {
		if record.attachment.Ref == ref {
			return false
		}
	}
	_, err := m.root.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func (m *Manager) finishWrite(item *sessionFile, data []byte) (int, error) {
	if err := m.deny("chunk-write", item.path); err != nil {
		return 0, err
	}
	return item.file.Write(data)
}
func (m *Manager) finishSync(item *sessionFile) error {
	if err := m.deny("finish-file-sync", item.path); err != nil {
		return err
	}
	return item.file.Sync()
}
func (m *Manager) finishRename(from, to string) error {
	if err := m.deny("finish-object-rename", to); err != nil {
		return err
	}
	return m.root.Rename(from, to)
}
func (m *Manager) finishRemove(path string) error {
	if err := m.deny("finish-object-remove", path); err != nil {
		return err
	}
	return m.root.Remove(path)
}
func (m *Manager) finishObjectsOpen() (*os.File, error) {
	if err := m.deny("finish-objects-open", "objects"); err != nil {
		return nil, err
	}
	return m.root.Open("objects")
}
func (m *Manager) finishObjectsSync(directory *os.File) error {
	if err := m.deny("finish-objects-sync", "objects"); err != nil {
		return err
	}
	return directory.Sync()
}
