package upload

import "os"

// Stage denials run before the named operation, never after an effect. Ordinary
// managers and production installed managers always use the concrete operation.
func (m *Manager) deny(stage, path string) error {
	if m.installed != nil && m.installed.denyStage != nil {
		return m.installed.denyStage(stage, path)
	}
	return nil
}

func (m *Manager) beginDirectory(path string) error {
	if err := m.deny("begin-directory", path); err != nil {
		return err
	}
	return m.root.Mkdir(path, 0o700)
}

func (m *Manager) beginFile(path string) (*os.File, error) {
	if err := m.deny("begin-file", path); err != nil {
		return nil, err
	}
	return m.root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
}

func (m *Manager) removeSessionDirectory(path string) error {
	if err := m.deny("session-remove", path); err != nil {
		return err
	}
	return m.root.RemoveAll(path)
}
