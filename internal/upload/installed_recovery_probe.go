package upload

// Recovery retains its existing deletion authority and NotExist policy.
// The private denial distinguishes Resolve recovery from expired index rows.
func (m *Manager) recoveryRemove(stage, path string) error {
	if err := m.deny(stage, path); err != nil {
		return err
	}
	return m.root.Remove(path)
}
